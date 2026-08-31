#!/usr/bin/env python3
"""
_ir_experiment_runner.py — implementation of qdiag-ir-experiment.

Orchestrates the IR-period experiment matrix (rows × cols) per
docs/design/plans/2026-07-21-ir-period-experiment.md §4 / §6a. Invoked by the
`qdiag-ir-experiment` bash wrapper, which sources _shared/lib.sh first so
QUASAR_GPU_SSH / QUASAR_GPU_DIR / QUASAR_GPU_IP (the gpu-test host) are already in the
environment.

Nothing here is a re-implementation of qnetem/qses/qnv: every network/session
action shells out to the existing scripts, exactly as those scripts document.

Live-cell sequencing (fixed 2026-07-22 review, see git log for the finding):
`qses run` drives its headless browser peer SYNCHRONOUSLY for its whole
--secs window and only exits at the end — so it is launched via Popen and
read incrementally until the `SID=` line appears, then left running in the
background while Gate A (log), netem, and qdiag-sample happen against the
STILL-LIVE session. Only after sampling is the drive torn down.
"""

import argparse
import base64
import datetime as dt
import json
import os
import re
import select
import shlex
import subprocess
import sys
import time
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parents[2]  # scripts/dx/session_diagnose -> repo root
SKILL_DIR = REPO_ROOT / ".claude" / "skills" / "quasar-diagnose"

sys.path.insert(0, str(SCRIPT_DIR))
import validators as V  # noqa: E402

QSES = REPO_ROOT / ".claude/skills/quasar-session/scripts/qses"
QNETEM = REPO_ROOT / ".claude/skills/quasar-netem/scripts/qnetem"
QDIAG_SAMPLE = SKILL_DIR / "scripts" / "qdiag-sample"
IR_ENV_PY = SCRIPT_DIR / "ir_env.py"

MAX_QSES_SECS = 3600
SID_WAIT_TIMEOUT_S = 90
DRIVE_END_MARGIN_S = 30

HELP_TEXT = """\
qdiag-ir-experiment — orchestrate the IR-period experiment matrix
(docs/design/plans/2026-07-21-ir-period-experiment.md §4/§6a).

Usage:
  qdiag-ir-experiment [--experiment ir|fec]
                      [--rows ...] [--cols clean,loss1,burst]
                      [--duration 120] [--out qdiag-runs/]
                      [--dry-run] [--resume] [--fail-fast]

  --experiment      which matrix: `ir` (default — ir_experiment block) or `fec`
                    (fec_experiment: FEC auto-arm, fec-off/fec-20-fixed/fec-auto ×
                    clean/loss1/burst). Same orchestration; only the config block
                    (rows/cols/fixed_env/whitelist/gate-A) differs.
  --rows / --cols   comma-separated subset of the selected block's {rows,cols}
                    keys (default: full matrix)
  --duration        seconds sampled per cell (default: ir_experiment.cell_duration_s)
  --out             output dir for run files + metadata (default: runs_dir)
  --dry-run         print the exact command plan per cell; execute nothing
  --resume          skip cells whose run files already validate (completeness)
  --fail-fast       abort the whole matrix on the first cell failure (default:
                    mark the cell failed, restore .env immediately, continue
                    with the remaining cells)

Every cell: mutate the target host's deploy/.env (idempotent), recreate quasar-node-agent,
Gate A env check, launch qses run (kept alive as a background drive), Gate A
log check, netem mid-session (qnetem sender / sender-clear by default — see
ir_experiment.netem_apply_cmd), qdiag-sample against the still-live session,
netem cleanup, stop the drive + session. deploy/.env is always restored to
its pre-experiment content — immediately on any cell failure (unless
--fail-fast, which restores once at the end) and again at the very end of
the whole matrix (belt and suspenders), alongside one full `qnetem clear`.
"""


def parse_args(argv):
    p = argparse.ArgumentParser(prog="qdiag-ir-experiment", add_help=False)
    p.add_argument("--rows")
    p.add_argument("--cols")
    p.add_argument("--duration", type=int)
    p.add_argument("--out")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--resume", action="store_true")
    p.add_argument("--fail-fast", action="store_true")
    # Which experiment matrix to run: `ir` (default, back-compat) drives the
    # config.json ir_experiment block; `fec` drives fec_experiment (FEC auto-arm,
    # docs/design/plans/2026-07-22-fec-auto-arm.md §7 L1-L3). Same orchestration —
    # only the config block (rows/cols/fixed_env/whitelist/gate-A) differs.
    p.add_argument("--experiment", choices=("ir", "fec"), default="ir")
    p.add_argument("--help", "-h", action="store_true", dest="help")
    return p.parse_args(argv)


# config.json block key + run-file prefix per experiment.
EXPERIMENTS = {
    "ir": ("ir_experiment", "ir"),
    "fec": ("fec_experiment", "fec"),
}


# The experiment always targets the streaming host (the `gpu-test` role). The
# wrapper sources _shared/lib.sh, which exports these role-derived values;
# _shared/hosts.json is the only place the actual host facts live.
def target_ssh_prefix():
    # Includes IdentityAgent=none when the host authenticates by key, so a
    # headless run can never be intercepted by an interactive agent.
    return os.environ.get("QUASAR_GPU_SSH", "$QUASAR_GPU_SSH")


def target_dir():
    return os.environ.get("QUASAR_GPU_DIR", "$QUASAR_GPU_DIR")


def target_ip():
    return os.environ.get("QUASAR_GPU_IP", "$QUASAR_GPU_IP")


def cell_secs(ir, duration):
    """qses --secs for the whole drive: enough to cover the netem delay, the
    sampled duration, and a tail margin so the session outlives sampling."""
    secs = ir["netem_apply_delay_s"] + duration + DRIVE_END_MARGIN_S
    if secs > MAX_QSES_SECS:
        raise ValueError(
            f"cell --secs {secs} exceeds qses's {MAX_QSES_SECS}s cap "
            f"(netem_apply_delay_s={ir['netem_apply_delay_s']} + duration={duration} + margin={DRIVE_END_MARGIN_S}); "
            f"reduce --duration"
        )
    return secs


def netem_commands(col, ir):
    """
    Return (apply_cmd_or_None, per_cell_clear_cmd) for this column, resolved
    from ir_experiment.netem_apply_cmd ("sender" default — sender-side egress
    shaping on the gpu-test host — or "aux-ingress", the cross-host path that
    shapes the aux-infra host's ingress).
    apply_cmd is None for the clean column (no shaping).
    """
    args = col["netem_args"]
    if args is None:
        return None, None
    verb = ir.get("netem_apply_cmd", "sender")
    if verb == "sender":
        return f"{QNETEM} sender {shlex.quote(args)}", f"{QNETEM} sender-clear"
    if verb == "aux-ingress":
        return f"{QNETEM} ingress {target_ip()} {shlex.quote(args)}", f"{QNETEM} clear"
    raise ValueError(f"ir_experiment.netem_apply_cmd: unknown verb '{verb}'")


# ── Small command builders shared between build_cell_commands (dry-run/docs) #
# and do_cell (live) so the two can never drift apart.                       #

def cmds_env(row, ir):
    env_updates = dict(ir["fixed_env"])
    env_updates.update(row["env"])
    # Defensive: config values never carry live remote file content, but quote
    # each KEY=VALUE token anyway so a future config value with shell
    # metacharacters can't leak into the remote command (see restore_env()
    # docstring for the bug class this guards against).
    env_kv = " ".join(f"{shlex.quote(k)}={shlex.quote(v)}" for k, v in env_updates.items())
    ssh = target_ssh_prefix()
    d = target_dir()
    return f"{ssh} \"cd {d} && python3 /dev/stdin {ir['env_path']} {env_kv}\" < {IR_ENV_PY}"


def cmds_recreate(ir):
    compose_flags = " ".join(f"-f {f}" for f in ir["compose_files"])
    ssh = target_ssh_prefix()
    d = target_dir()
    return f"{ssh} \"cd {d} && docker compose {compose_flags} up -d {ir['recreate_service']}\""


def cmds_gate_a_env(ir):
    ssh = target_ssh_prefix()
    return f"{ssh} \"docker exec {ir['agent_container']} env | grep -E '{ir['gate_a_env_grep']}'\""


def cmds_gate_a_log(ir, row=None):
    """Gate A log grep. Default (IR): grep the block's `gate_a_log_pattern`.
    In `gate_a_mode: per_row_expect` (FEC) grep the row's own
    `gate_a_log_expect` substring; a row without one (e.g. the off control)
    has no FEC log to assert, so this returns a no-op echo (env is the truth)."""
    ssh = target_ssh_prefix()
    if ir.get("gate_a_mode") == "per_row_expect":
        expect = (row or {}).get("gate_a_log_expect")
        if not expect:
            return "echo '(no gate-A log assertion for this row — env is the config truth)'"
        return f"{ssh} \"docker logs {ir['agent_container']} --since 5m | grep -F {shlex.quote(expect)}\""
    return f"{ssh} \"docker logs {ir['agent_container']} --since 5m | grep -F '{ir['gate_a_log_pattern']}'\""


def build_cell_commands(row_id, row, col_id, col, ir, duration, out_dir, sid_placeholder="<SID>"):
    """
    Return an ordered list of (label, shell_command_string) for one cell, in
    the ACTUAL live execution order: env -> recreate -> gate-a-env -> session
    (launched, left running) -> gate-a-log -> netem apply -> sample ->
    netem per-cell clear -> stop session. Pure/side-effect-free — used for
    both --dry-run printing and as the reference for real execution.
    """
    secs = cell_secs(ir, duration)
    apply_cmd, clear_cmd = netem_commands(col, ir)

    cmds = [
        ("env", cmds_env(row, ir)),
        ("recreate", cmds_recreate(ir)),
        ("gate-a-env", cmds_gate_a_env(ir)),
        (
            "session (launched, kept running in background)",
            f"{QSES} run --stack=gpu-test --profile {ir['profile']} --keep --secs {secs}",
        ),
        ("gate-a-log", cmds_gate_a_log(ir, row)),
    ]
    if apply_cmd is None:
        cmds.append(("netem", "# clean column — no shaping applied"))
    else:
        cmds.append(("netem", apply_cmd))
    cmds.append((
        "sample",
        f"{QDIAG_SAMPLE} --host gpu-test --session {sid_placeholder} "
        f"--interval {ir['sample_interval_s']} --duration {duration} --out {out_dir}",
    ))
    cmds.append((
        "netem-cleanup",
        clear_cmd if clear_cmd else "# clean column — nothing to clear",
    ))
    cmds.append(("stop", f"{QSES} stop {sid_placeholder}"))
    return cmds


def print_cell_plan(row_id, col_id, cmds):
    print(f"CELL {row_id} x {col_id}")
    for label, cmd in cmds:
        print(f"  [{label}]".ljust(14) + cmd)
    print()


def run(cmd, **kw):
    return subprocess.run(cmd, shell=True, text=True, capture_output=True, **kw)


def existing_valid_run_file(out_dir: Path, row_id, col_id, cfg, run_prefix="ir"):
    pattern = f"{run_prefix}-{row_id}-{col_id}-*.json"
    for candidate in sorted(out_dir.glob(pattern)):
        if candidate.name.endswith(".meta.json"):
            continue
        try:
            run_data = json.loads(candidate.read_text())
            V.validate_run_completeness(run_data, cfg)
            return candidate
        except Exception:
            continue
    return None


# --------------------------------------------------------------------------- #
# .env restore — MUST NOT let live file content pass through a shell string.  #
# The host's deploy/.env carries `$`-bearing secrets (passwords); embedding that  #
# text directly inside an f-string shell command run under shell=True would   #
# let the LOCAL shell expand `$VAR`/backticks in it before ssh ever sees it,  #
# corrupting the very file this function exists to protect. Fix: base64      #
# the content locally and pipe it through `base64 -d` remotely — the b64     #
# alphabet ([A-Za-z0-9+/=]) contains no shell metacharacters, so a           #
# single-quoted embed is injection-safe regardless of what the original      #
# .env contained.                                                             #
# --------------------------------------------------------------------------- #

def restore_env(ir, original_env_text):
    ssh = target_ssh_prefix()
    d = target_dir()
    env_path = ir["env_path"]
    b64 = base64.b64encode(original_env_text.encode()).decode()
    restore_cmd = f"{ssh} \"printf '%s' '{b64}' | base64 -d > {d}/{env_path}\""
    r = run(restore_cmd)
    compose_flags = " ".join(f"-f {f}" for f in ir["compose_files"])
    recreate_cmd = f"{ssh} \"cd {d} && docker compose {compose_flags} up -d {ir['recreate_service']}\""
    run(recreate_cmd)
    return r


def read_until_sid(proc, timeout_s):
    """
    Read proc.stdout (merged with stderr) incrementally until a `SID=<id>`
    line appears or timeout_s elapses. Returns (sid_or_None, buffered_lines).
    Uses select() so a hung process (never printing SID=) can't block past
    the timeout — subprocess.run()'s capture-at-exit would have (CRITICAL-1).
    """
    deadline = time.time() + timeout_s
    buf = []
    while True:
        remaining = deadline - time.time()
        if remaining <= 0:
            break
        ready, _, _ = select.select([proc.stdout], [], [], min(remaining, 1.0))
        if proc.stdout in ready:
            line = proc.stdout.readline()
            if line == "":
                if proc.poll() is not None:
                    break
                continue
            buf.append(line)
            m = re.match(r"^SID=(\S+)", line)
            if m:
                return m.group(1), buf
        elif proc.poll() is not None:
            break
    return None, buf


def drain_remaining(proc, timeout_s):
    """Collect the rest of a still-running qses drive's output, then ensure
    it's terminated. Returns the collected text (decode-verdict evidence)."""
    lines = []
    deadline = time.time() + timeout_s
    while time.time() < deadline and proc.poll() is None:
        remaining = deadline - time.time()
        ready, _, _ = select.select([proc.stdout], [], [], min(remaining, 1.0))
        if proc.stdout in ready:
            line = proc.stdout.readline()
            if line:
                lines.append(line)
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    try:
        rest = proc.stdout.read()
        if rest:
            lines.append(rest)
    except Exception:
        pass
    return "".join(lines)


def do_cell(row_id, row, col_id, col, ir, cfg, duration, out_dir, fail_fast, original_env_text):
    """Execute one live cell. Returns (ok: bool, meta: dict)."""
    run_prefix = ir.get("run_prefix", "ir")
    ts = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    stem = f"{run_prefix}-{row_id}-{col_id}-{ts}"
    run_path = out_dir / f"{stem}.json"
    meta_path = out_dir / f"{stem}.meta.json"

    env_updates = dict(ir["fixed_env"])
    env_updates.update(row["env"])
    meta = {
        "row": row_id, "col": col_id, "cell_id": f"{run_prefix}-{row_id}-{col_id}",
        "env": env_updates, "netem_args": col["netem_args"],
        "start_unix_ms": int(time.time() * 1000), "end_unix_ms": None,
        "status": "running", "gate_a": {"env_evidence": None, "log_evidence": None, "passed": False},
        "run_file": run_path.name, "failure_reason": None, "decode_evidence": None,
    }
    apply_cmd, clear_cmd = netem_commands(col, ir)

    def fail(reason, proc=None, sid=None):
        if proc is not None:
            meta["decode_evidence"] = drain_remaining(proc, 10)
        if clear_cmd:
            run(clear_cmd)
        if sid:
            run(f"{QSES} stop {sid}")
        meta["status"] = "failed"
        meta["failure_reason"] = reason
        meta["end_unix_ms"] = int(time.time() * 1000)
        meta_path.write_text(json.dumps(meta, indent=2))
        print(f"  CELL FAILED: {reason}", file=sys.stderr)
        # MINOR-4 (spec §6a): restore .env immediately on failure so the box
        # is never left mis-configured between cells; --fail-fast restores
        # once at the very end instead (main()'s finally still runs too).
        if not fail_fast:
            restore_env(ir, original_env_text)
        return False, meta

    r = run(cmds_env(row, ir))
    if r.returncode != 0:
        return fail(f".env mutation failed: {r.stderr.strip()}")

    r = run(cmds_recreate(ir))
    if r.returncode != 0:
        return fail(f"agent recreate failed: {r.stderr.strip()}")
    time.sleep(5)  # wait for the container to come up before Gate A / session start

    r = run(cmds_gate_a_env(ir))
    meta["gate_a"]["env_evidence"] = (r.stdout or "").strip()
    if r.returncode != 0 or not meta["gate_a"]["env_evidence"]:
        return fail("Gate A env check found no INTRA env vars in the recreated container")

    # CRITICAL-1 fix: qses run drives its headless browser SYNCHRONOUSLY for
    # the whole --secs window and only exits (and flushes captured stdout) at
    # the very end. subprocess.run() here would block until the drive is
    # already over — netem/Gate-A-log/qdiag-sample would then all run against
    # a dead session (no client metrics, no GCC feedback). Launch via Popen
    # instead, read incrementally until `SID=` appears, and leave the process
    # running (backgrounded) for the rest of the cell.
    secs = cell_secs(ir, duration)
    session_cmd = (
        f"{QSES} run --stack=gpu-test --profile {ir['profile']} --keep --secs {secs}"
    )
    proc = subprocess.Popen(
        session_cmd, shell=True, text=True, bufsize=1,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    )
    sid, startup_lines = read_until_sid(proc, SID_WAIT_TIMEOUT_S)
    if not sid:
        if proc.poll() is None:
            proc.terminate()
        return fail(
            f"qses run did not print SID= within {SID_WAIT_TIMEOUT_S}s: "
            f"{''.join(startup_lines)[-500:]}"
        )

    time.sleep(ir["netem_apply_delay_s"])

    # MINOR-6: Gate A log check BEFORE netem apply. The A3 summary line
    # appears at session start, independent of shaping — checking first means
    # a Gate A failure never leaves shaping applied, and cleanup is just
    # "stop the drive", no netem-clear needed.
    r = run(cmds_gate_a_log(ir, row))
    log_evidence = (r.stdout or "").strip().splitlines()[-1] if (r.stdout or "").strip() else ""
    meta["gate_a"]["log_evidence"] = log_evidence
    if ir.get("gate_a_mode") == "per_row_expect":
        # FEC: env is the config truth; a row's optional `gate_a_log_expect`
        # substring (present ⇒ the FEC-on rows) must appear. The off/control
        # row has no FEC log to assert, so it passes on env evidence alone.
        expect = row.get("gate_a_log_expect")
        gate_a_log_ok = (not expect) or bool(log_evidence)
    else:
        # IR: the A3 summary line's `=true` token must match the row's intent
        # (present for IR rows, absent for the `ir-off` control row).
        expected_not_requested = row_id == ir["control_row"]
        gate_a_log_ok = bool(log_evidence) and (("=true" in log_evidence) != expected_not_requested)
    meta["gate_a"]["passed"] = bool(meta["gate_a"]["env_evidence"]) and gate_a_log_ok
    if not meta["gate_a"]["passed"]:
        return fail(f"Gate A log check failed: evidence={log_evidence!r}", proc=proc, sid=sid)

    if apply_cmd is not None:
        r = run(apply_cmd)
        if r.returncode != 0:
            return fail(f"netem apply failed: {r.stderr.strip()}", proc=proc, sid=sid)

    sample_cmd = (
        f"{QDIAG_SAMPLE} --host gpu-test --session {sid} "
        f"--interval {ir['sample_interval_s']} --duration {duration} --out {out_dir}"
    )
    r = run(sample_cmd)
    out_match = re.search(r"^\s*output:\s*(\S+)", r.stdout or "", re.MULTILINE)

    if clear_cmd:
        run(clear_cmd)
    meta["decode_evidence"] = drain_remaining(proc, DRIVE_END_MARGIN_S + 10)
    run(f"{QSES} stop {sid}")

    if r.returncode != 0 or not out_match:
        meta["status"] = "failed"
        meta["failure_reason"] = f"qdiag-sample failed: {r.stderr.strip() or r.stdout.strip()}"
        meta["end_unix_ms"] = int(time.time() * 1000)
        meta_path.write_text(json.dumps(meta, indent=2))
        print(f"  CELL FAILED: {meta['failure_reason']}", file=sys.stderr)
        if not fail_fast:
            restore_env(ir, original_env_text)
        return False, meta

    sampled_path = Path(out_match.group(1))
    if sampled_path.exists():
        sampled_path.replace(run_path)
    else:
        meta["status"] = "failed"
        meta["failure_reason"] = f"qdiag-sample reported output {sampled_path} but it does not exist"
        meta["end_unix_ms"] = int(time.time() * 1000)
        meta_path.write_text(json.dumps(meta, indent=2))
        if not fail_fast:
            restore_env(ir, original_env_text)
        return False, meta

    meta["status"] = "ok"
    meta["end_unix_ms"] = int(time.time() * 1000)
    meta_path.write_text(json.dumps(meta, indent=2))
    return True, meta


def main(argv):
    args = parse_args(argv)
    if args.help:
        print(HELP_TEXT)
        return 0

    cfg = V.load_config()
    block_key, run_prefix = EXPERIMENTS[args.experiment]
    V.validate_experiment_config(cfg, block_key)
    ir = cfg[block_key]

    row_ids = (args.rows.split(",") if args.rows else list(ir["rows"].keys()))
    col_ids = (args.cols.split(",") if args.cols else list(ir["cols"].keys()))
    for r in row_ids:
        if r not in ir["rows"]:
            print(f"Unknown row '{r}'. Known rows: {list(ir['rows'].keys())}", file=sys.stderr)
            return 2
    for c in col_ids:
        if c not in ir["cols"]:
            print(f"Unknown col '{c}'. Known cols: {list(ir['cols'].keys())}", file=sys.stderr)
            return 2

    duration = args.duration or ir["cell_duration_s"]
    try:
        cell_secs(ir, duration)
    except ValueError as e:
        print(f"FATAL: {e}", file=sys.stderr)
        return 2
    out_dir = Path(args.out) if args.out else (SKILL_DIR / cfg["runs_dir"])
    out_dir.mkdir(parents=True, exist_ok=True)

    print(f"qdiag-ir-experiment [{args.experiment}]: rows={row_ids} cols={col_ids} duration={duration}s")
    print(f"  cells: {len(row_ids) * len(col_ids)}  out: {out_dir}")
    print()

    if args.dry_run:
        for row_id in row_ids:
            for col_id in col_ids:
                cmds = build_cell_commands(
                    row_id, ir["rows"][row_id], col_id, ir["cols"][col_id], ir, duration, out_dir
                )
                print_cell_plan(row_id, col_id, cmds)
        return 0

    # ── Live execution ──────────────────────────────────────────────────────
    # Capture the pre-experiment .env so it can be restored byte-identical —
    # immediately on any cell failure (unless --fail-fast) and again here at
    # the very end of the whole matrix.
    ssh = target_ssh_prefix()
    d = target_dir()
    env_path = ir["env_path"]
    snapshot = run(f"{ssh} \"cat {d}/{env_path}\"")
    if snapshot.returncode != 0:
        print(f"FATAL: could not read {env_path} on the target host: {snapshot.stderr.strip()}", file=sys.stderr)
        return 1
    original_env_text = snapshot.stdout

    any_failed = False
    try:
        for row_id in row_ids:
            for col_id in col_ids:
                if args.resume:
                    existing = existing_valid_run_file(out_dir, row_id, col_id, cfg, run_prefix)
                    if existing:
                        print(f"CELL {row_id} x {col_id}: RESUME — {existing.name} already valid, skipping")
                        continue
                print(f"CELL {row_id} x {col_id}: starting")
                ok, meta = do_cell(
                    row_id, ir["rows"][row_id], col_id, ir["cols"][col_id],
                    ir, cfg, duration, out_dir, args.fail_fast, original_env_text,
                )
                status = "OK" if ok else "FAILED"
                print(f"CELL {row_id} x {col_id}: {status}")
                if not ok:
                    any_failed = True
                    if args.fail_fast:
                        raise SystemExit(1)
    finally:
        restore_env(ir, original_env_text)
        run(f"{QNETEM} clear")  # belt-and-suspenders: clears shaping on BOTH hosts

    return 1 if any_failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
