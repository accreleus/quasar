#!/usr/bin/env python3
"""scripts/dx/bench_submit.py — submit a soak/observe run directory to quasar-bench.

    scripts/dx/bench_submit.py --dir <RUNDIR> --suite abr-ladder \
        --scenario 1080p120-h264-netem-moderate --tag encoder=nvenc --tag app=kde-desktop

    make bench-submit DIR=<RUNDIR> ARGS='--suite ... --scenario ... --tag k=v'

The <RUNDIR> is the shape `scripts/dx/session_soak.sh` writes:

    session.json   the resolved session (launch size, rungs, codec, fps, host)
    metrics.jsonl  {"kind":"metric","source":"agent|browser","ts_unix_ms":..,"metrics":{..}}
    trace.json     {"events":[{ts_unix_ms,type,payload,source}], "series":{..}, ...}
    marks.jsonl    {"ts_unix_ms":..,"mark":"impair|clear","level":"moderate"}
    steps.jsonl    one record per soak step
    summary.json   scripts/dx/session_soak_report.py's analysis (has "overall")
    REPORT.md / LADDER.md  the human reports

What is posted
  samples    every metrics.jsonl row, `source` verbatim, `metrics` reduced to a
             NUMERIC map (bools -> 0/1). A STRING metric (adaptation_state,
             abr_mode, external_owner) cannot be a sample value, so it is encoded
             as `<key>_code` (see STRING_CODES) and the code book is echoed in the
             run summary under `string_metric_codes` plus a `harness.string_codes`
             event, so a chart of `agent.adaptation_state_code` is decodable.
             Only REAL measurements are posted as samples — see "Phases" below.
  events     trace.json events verbatim (abr.ladder.step, abr.retarget,
             client.freeze_detected, playout.changed, ...), marks.jsonl as
             `netem.impair`/`netem.clear`, steps.jsonl as `harness.step`, and one
             explicit `harness.mark {phase, edge}` pair per phase boundary.
  artifacts  REPORT.md, LADDER.md, summary.json, steps.jsonl, marks.jsonl,
             session.json, conditions.json (each only if it exists).
  finish     status=finished, summary = summary.json verbatim (+ harness extras),
             verdict from summary.json's `overall` unless --verdict overrides.

Phases (quasar-bench 1.1)
  The SERVER derives the phase windows now, from `netem.impair`/`netem.clear`
  plus explicit `harness.mark {phase, edge:"start"|"end"}` events, and every
  query takes `window=<phase>`. So this script posts the MARKS and stops there.
  It no longer synthesises a `harness` sample of `<phase>_<source>_<key>_<stat>`
  rollups: that was a workaround for a service that could not window, and a
  derived number sitting in the same table as real measurements is a trap. The
  same rollup is still computed for the run SUMMARY (advisory, human-readable) —
  the authority is `GET /v1/stats?...&window=impaired`.
  Marks posted: only what the netem events cannot describe — `settled` (each
  impaired span minus --settle-secs) for an impaired run, `warmup`/`observe`
  for an unimpaired one. `baseline`/`impaired`/`recovery` come from the netem
  events server-side (origin `netem`), so they are not duplicated as marks.

Tags vs conditions
  Tags are what was INTENDED, `conditions` is what the host actually did. Tags
  are derived from session.json (launch WxH, fps, codec, profile like
  `1080p120`, session_id, quasar_host_id), plus the git shas of THIS worktree
  (git_quasar, git_protocol), plus every --tag k=v (which always WINS over a
  derived value — a retro-submission knows the sha the run actually used).
  `conditions` is read from <RUNDIR>/conditions.json (bench_run.sh writes it
  from GET /v1/admin/hosts/{id}/settings at launch) or --conditions FILE, and is
  completed here with `negotiated` (from session.json) and `git`. The service
  compares every `conditions.effective` key against the same-named tag and
  returns `mismatches`; any mismatch is printed loudly, tagged `mismatch=1`, and
  makes this script exit 3 so a suite STOPS on a mislabelled cell.

Idempotency
  Every submission carries a deterministic `external_id` (first-class since
  quasar-bench 1.1): sha256 of suite + scenario + the session id (or the
  absolute directory path when there is none) + the directory basename
  (+ --ext-id-salt). POST /v1/runs UPSERTS on it — 200 for an update, 201 for a
  create, both fine — so a re-submission converges instead of forking a second
  run (samples and events are idempotent on (run, source, ts) server-side).
  `--run-id <id>` targets a specific run; `--new` forces a fresh run.

Environment
  BENCH_URL   base URL of the quasar-bench service (e.g. http://bench.example.internal:9400)
  BENCH_KEY   API key (a `BENCH_API_KEYS` secret).  Never commit either.

Exit: 0 submitted (or --dry-run planned), 1 failure, 2 usage,
      3 submitted but the run's tags disagree with what the host actually did.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import urllib.request

DX_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(DX_DIR, "vendor"))
# thresholds.py lives beside this file. A direct `python3 bench_submit.py` run
# gets DX_DIR on sys.path for free (the interpreter always prepends the
# running script's own directory), but the test suite loads this module via
# importlib.util.spec_from_file_location, which does not — so state it
# explicitly rather than depend on how the caller happened to import this.
sys.path.insert(0, DX_DIR)

import bench as bench_client  # noqa: E402  (vendored client, path set above)
from bench import Bench, BenchError  # noqa: E402
import thresholds  # noqa: E402  (scripts/dx/thresholds.py, same directory)

MISMATCH_RC = 3

# ── string metrics ───────────────────────────────────────────────────────────
# A sample value must be a number. These are the string-valued keys the agent
# and browser emit; each becomes `<key>_code`. Codes are FROZEN — appending is
# fine, renumbering breaks every historical chart.
STRING_CODES = {
    "adaptation_state": {
        "unknown": 0,
        "healthy": 1,
        "network_congested": 2,
        "encoder_saturated": 3,
    },
    "abr_mode": {"off": 0, "protective": 1, "smooth": 2},
    "external_owner": {"auto": 0, "operator": 1},
}
STRING_UNKNOWN = -1

ARTIFACT_FILES = (
    "REPORT.md",
    "LADDER.md",
    "summary.json",
    "steps.jsonl",
    "marks.jsonl",
    "session.json",
    "conditions.json",
    # C11 #2/#4: bench_run.sh's best-effort fetches of the session verdict and
    # the caps.negotiated trace event, consumed below and also kept verbatim
    # as evidence.
    "verdict.json",
    "caps-negotiated.json",
)


def die(msg: str, rc: int = 1) -> "NoReturn":  # type: ignore[name-defined]
    print("FAIL bench-submit — %s" % msg, file=sys.stderr)
    print("RESULT status=failed target=bench-submit pass=0 warn=0 fail=1")
    sys.exit(rc)


# ── BENCH_KEY name:secret tolerance (2026-08-19 overnight-2 harness note #2) ──
# `deploy/.env`'s BENCH_API_KEYS stores `name:secret` pairs (e.g.
# `harness:abc123...`); BENCH_KEY/--key must be the bare secret only — the
# server's Bearer check has no notion of the name. docs/configuration.md's own
# recipe (`sed -n 's/^BENCH_API_KEYS=harness://p' ...`) strips it correctly,
# but a raw `harness:abc123...` pasted straight from .env 401s, and the first
# overnight-2 submit attempt hit exactly that before falling back to the
# documented recipe. Rather than requiring the recipe every time, tolerate the
# `name:secret` shape here directly: strip on the first colon and warn, so a
# pasted-from-.env value degrades to a clear message instead of a bare 401.
def normalize_bench_key(raw: str | None, source: str) -> str | None:
    if not raw or ":" not in raw:
        return raw
    name, _, secret = raw.partition(":")
    print("WARN  %s looks like 'name:secret' (name=%r) — using only the part "
          "after the colon as the API key" % (source, name))
    return secret


# ── vendored-client drift ────────────────────────────────────────────────────


def _ver_tuple(v: str) -> tuple:
    return tuple(int(x) for x in re.findall(r"\d+", v)[:3]) or (0,)


def server_version(url: str) -> str:
    """The service's own version, or "" when it does not publish one.

    /v1/health carries no version, so this reads `info.version` out of the
    OpenAPI document the service serves (unauthenticated). Best-effort and
    short-timeout: a drift check must never be able to fail a submission.
    """
    if not url:
        return ""
    try:
        with urllib.request.urlopen(url.rstrip("/") + "/openapi.yaml", timeout=5) as res:
            head = res.read(4096).decode("utf-8", "replace")
    except Exception:
        return ""
    m = re.search(r'^\s+version:\s*"?([0-9][0-9A-Za-z.\-]*)"?\s*$', head, re.M)
    return m.group(1) if m else ""


def check_client_version(url: str) -> None:
    mine = getattr(bench_client, "__version__", "0")
    theirs = server_version(url)
    if not theirs:
        print("     client    vendored bench.py %s (service publishes no version)" % mine)
        return
    if _ver_tuple(mine) < _ver_tuple(theirs):
        print("WARN client — vendored bench.py is %s but the service is %s; re-vendor "
              "scripts/dx/vendor/bench.py from quasar-bench client/bench.py" % (mine, theirs),
              file=sys.stderr)
    print("     client    vendored bench.py %s / service %s" % (mine, theirs))


# ── readers ──────────────────────────────────────────────────────────────────


def read_json(path: str):
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        text = fh.read().strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except ValueError:
        return None


def read_jsonl(path: str) -> list:
    rows = []
    if not os.path.exists(path):
        return rows
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    return rows


def read_caps_negotiated(rundir: str) -> dict | None:
    """The latest caps.negotiated trace-event payload from
    <RUNDIR>/caps-negotiated.json — bench_run.sh's best-effort fetch of
    GET .../trace/events?types=caps.negotiated (C11 #4).

    This is what the encode branch ACTUALLY agreed, authoritative over
    session.json's one-shot `codec` field: on the Vulkan path every rung step
    is an encoder restart, so after the first step session.json's snapshot
    describes caps that no longer exist (node-agent/src/session/runner.rs,
    caps_negotiated_payload doc comment).

    None when the file is missing, empty, or carries no events — an older
    control plane with no allow-list entry for this type, or a read that raced
    a session that had already torn down. Never required for a submission.
    """
    d = read_json(os.path.join(rundir, "caps-negotiated.json"))
    if not isinstance(d, dict):
        return None
    items = d.get("events") or d.get("items") or []
    if not items:
        return None
    # Chronological, LAUNCH first, one entry per (re)negotiation — the LAST one
    # is what the encode branch was doing when the run ended.
    last = items[-1]
    payload = last.get("payload") if isinstance(last, dict) else None
    return payload if isinstance(payload, dict) else None


def _pct(values: list, q: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    if len(s) == 1:
        return float(s[0])
    pos = q * (len(s) - 1)
    lo = int(pos)
    hi = min(lo + 1, len(s) - 1)
    return float(s[lo] + (s[hi] - s[lo]) * (pos - lo))


# Keys rolled up per phase. A whole-run p50 of browser.fps CANNOT answer "was the
# stream better under impairment" — the unimpaired baseline and recovery holds
# dominate the sample count and wash the impaired window out entirely (proved
# empirically re-submitting runs C/D/E: every group came back p50 ~60).
#
# quasar-bench 1.1 answers that server-side (`window=impaired`), so this rollup is
# NO LONGER POSTED AS A SAMPLE — it goes into the run summary only, as an advisory
# human-readable block. The authority is the API query. Posting derived aggregates
# into the same table as real measurements meant `metric=browser.fps` and
# `metric=harness.impaired_browser_fps_mean` were two different truths.
# 2026-08-23 (C11 #1): present_fps (a MEAN of the RVFC interval) is deprecated —
# docs/session-trace/metrics.json marks it deprecated_for=present_fps_median: at
# source fps == display Hz one missed vsync doubles one interval and drags the
# mean, and a healthy 1440p120 session reading it as 88-108 cost a day of
# encoder investigation. present_fps_median is THE reading now;
# present_interval_max_ms / present_beat_fraction / present_long_frames /
# present_n are the rest of the AS-04/#108 cadence vocabulary this rollup was
# still blind to. agent.encode_ms_max is the one agent key that can see a
# single one-frame stall — a mean and a p95 both wash it out over a 5s window.
ROLLUP_KEYS = {
    "browser": ("fps", "present_interval_sd_ms", "rtt_ms", "decode_ms",
                "jitter_buffer_ms", "present_fps_median", "present_beat_fraction",
                "present_long_frames", "present_interval_max_ms", "present_n"),
    "agent": ("fps", "bitrate_kbps", "abr_setpoint_kbps", "gcc_estimate_kbps",
              "encode_ms", "abr_floor_kbps", "encode_ms_max"),
}
# Counters, not gauges: report the run-window DELTA, not a mean of a rising total.
#
# present_long_frames does NOT belong here even though its name sounds like a
# counter: metrics.json declares it `window: 1s, estimator: count` — it is
# ALREADY a per-window delta (a fresh count every window), not a monotonically
# rising total like frames_dropped/packets_lost/freeze_count. Rolling it up via
# ROLLUP_KEYS (mean/p50/p95/min) answers "how many long frames per window,
# typically / at the worst window" — max-min across the phase would instead
# answer a question about a CUMULATIVE total this key never tracked, and read
# as a much larger, wrong number.
ROLLUP_COUNTERS = {"browser": ("frames_dropped", "packets_lost", "freeze_count")}


def phase_windows(marks: list, samples: list, settle_secs: float,
                  warmup_secs: float = 0.0) -> dict:
    """{phase: [(lo_ms, hi_ms), ...]} derived from marks.jsonl.

    No marks (an unimpaired baseline run) => a single `run` phase spanning
    everything. With marks: `baseline` before the first impair, `impaired` the
    UNION of the impair->clear spans, `settled` = each impaired span minus its
    first `settle_secs` (the ladder's engage transient, which is exactly the
    window VALIDATION.md excluded by hand), `recovery` after the last clear.

    `impaired` is a UNION, not simply first-impair..last-clear. A ladder run that
    alternates levels (`--levels mild,clean,severe`) writes a `clear` mark for
    each `clean` rung — abr_ladder_netem.sh marks `clean` as a clear, not as an
    impair — so spanning straight across folded those un-impaired dwells into the
    impaired numbers and quietly flattered them. Consecutive `impair` marks with
    no clear between them are a LEVEL change, not a new span: the window stays
    open, which is what `--levels moderate,severe` means.
    """
    if not samples:
        return {}
    t_lo = min(s["ts_unix_ms"] for s in samples)
    t_hi = max(s["ts_unix_ms"] for s in samples)
    ordered = sorted((m["ts_unix_ms"], m.get("mark")) for m in marks
                     if m.get("ts_unix_ms") is not None)
    spans: list = []
    open_at = None
    for ts, kind in ordered:
        if kind == "impair":
            if open_at is None:
                open_at = ts
        elif kind == "clear" and open_at is not None:
            if ts > open_at:
                spans.append((open_at, ts))
            open_at = None
    if open_at is not None and t_hi + 1 > open_at:
        spans.append((open_at, t_hi + 1))   # never cleared: impaired to the end
    if not spans:
        # An unimpaired run (a plain bench/observe cell). Its single `run` phase is
        # the WHOLE peer hold, and the peer is deliberately held longer than the
        # measurement — so a whole-run aggregate is dominated by app start-up and
        # stream warm-up. Measured on a benchapp cell (devbox 2026-08-18) the
        # missing-index rate read 2.38% over the hold but 0.86% over the observe
        # window, because the first 60s ran at 6.95%. A reader comparing one run's
        # whole-hold figure against another's steady state invents a regression.
        # So split it, and let the service answer `window=observe`.
        out = {"run": [(t_lo, t_hi + 1)]}
        if warmup_secs and warmup_secs > 0:
            boundary = t_lo + int(warmup_secs * 1000)
            if t_hi + 1 > boundary:
                out["warmup"] = [(t_lo, boundary)]
                out["observe"] = [(boundary, t_hi + 1)]
        return out
    first, last = spans[0][0], spans[-1][1]
    out = {"run": [(t_lo, t_hi + 1)], "impaired": spans}
    if first > t_lo:
        out["baseline"] = [(t_lo, first)]
    if t_hi + 1 > last:
        out["recovery"] = [(last, t_hi + 1)]
    settled = []
    for lo, hi in spans:
        s = lo + int(settle_secs * 1000)
        if hi > s:
            settled.append((s, hi))
    if settled:
        out["settled"] = settled
    return out


def _in_spans(ts: int, spans: list) -> bool:
    return any(lo <= ts < hi for lo, hi in spans)


def phase_rollup(samples: list, marks: list, settle_secs: float,
                 warmup_secs: float = 0.0) -> dict:
    """`harness` metrics: <phase>_<source>_<key>_<stat> for every phase."""
    windows = phase_windows(marks, samples, settle_secs, warmup_secs)
    out: dict = {}
    for phase, spans in sorted(windows.items()):
        rows = [s for s in samples if _in_spans(s["ts_unix_ms"], spans)]
        out["%s_secs" % phase] = round(
            sum(hi - lo for lo, hi in spans) / 1000.0, 1)
        for source, keys in ROLLUP_KEYS.items():
            vals: dict = {}
            for s in rows:
                if s["source"] != source:
                    continue
                for k in keys:
                    v = s["metrics"].get(k)
                    if isinstance(v, (int, float)):
                        vals.setdefault(k, []).append(float(v))
            for k, series in vals.items():
                base = "%s_%s_%s" % (phase, source, k)
                out[base + "_mean"] = round(sum(series) / len(series), 3)
                out[base + "_p50"] = round(_pct(series, 0.50), 3)
                out[base + "_p95"] = round(_pct(series, 0.95), 3)
                out[base + "_min"] = round(min(series), 3)
            # The headline quality proxy: how much of the phase was unwatchable.
            fps = vals.get("fps")
            if source == "browser" and fps:
                out["%s_browser_below10fps_frac" % phase] = round(
                    sum(1 for v in fps if v < 10) / len(fps), 4)
        for source, keys in ROLLUP_COUNTERS.items():
            for k in keys:
                series = [s["metrics"][k] for s in rows
                          if s["source"] == source and isinstance(s["metrics"].get(k), (int, float))]
                if series:
                    out["%s_%s_%s_delta" % (phase, source, k)] = round(
                        max(series) - min(series), 3)
    return out


# `warmup`/`observe` describe an UNIMPAIRED cell: the peer-attach + app-start
# transient, and the steady state after it. They are mutually exclusive with the
# netem phases — a run has either the impairment vocabulary or this one.
MARK_PHASES = ("baseline", "impaired", "settled", "recovery", "warmup", "observe")
#: phases the SERVICE derives from the posted netem.impair/netem.clear events —
#: never marked when netem events are posted (see phase_marks).
NETEM_DERIVED_PHASES = ("baseline", "impaired", "recovery")


def phase_marks(samples: list, marks: list, settle_secs: float,
                warmup_secs: float = 0.0) -> list:
    """`harness.mark {phase, edge}` events for the windows only the harness knows.

    The service derives baseline/impaired/recovery from the netem events itself
    (store/phases.go, origin `netem`), and this script always posts marks.jsonl
    as `netem.impair`/`netem.clear` — so for an impaired run those three phases
    are NOT marked here: a duplicate mark on the same instants is harmless for
    scoping (the window filter is an EXISTS) but reports the phase as origin
    `mixed`, and one canonical source per phase is worth more than a
    self-describing run. Decision 2026-08-18: netem events are the ground truth
    for what the link did; marks cover only what netem cannot express —
    `settled` (each impaired span minus its first --settle-secs of engage
    transient) and `warmup`/`observe` for an unimpaired cell.

    Edge timestamps are half-open [start, end) — the same convention the service
    uses for `to_unix_ms`.
    """
    windows = phase_windows(marks, samples, settle_secs, warmup_secs)
    out = []
    for phase in MARK_PHASES:
        if marks and phase in NETEM_DERIVED_PHASES:
            continue
        for lo, hi in windows.get(phase, []):
            if hi <= lo:
                continue
            out.append({"ts_unix_ms": int(lo), "type": "harness.mark",
                        "payload": {"phase": phase, "edge": "start"}})
            out.append({"ts_unix_ms": int(hi), "type": "harness.mark",
                        "payload": {"phase": phase, "edge": "end"}})
    return out


def netem_conditions(marks: list) -> dict:
    """The netem state a run was observed under, read back from marks.jsonl."""
    impairs = [m for m in marks if m.get("mark") == "impair"]
    if not impairs:
        return {"level": "none"}
    first = impairs[0]
    out = {k: v for k, v in first.items() if k not in ("ts_unix_ms", "mark")}
    out.setdefault("level", "unknown")
    levels = [m.get("level") for m in impairs if m.get("level")]
    if len(set(levels)) > 1:
        out["levels"] = levels
    return out


def build_conditions(rundir: str, session: dict, marks: list, tags: dict,
                     root: str, path: str | None) -> dict:
    """What the host ACTUALLY did, vs the tags' statement of intent.

    `effective` is the host's own effective settings map, captured by
    bench_run.sh at launch into <RUNDIR>/conditions.json — it is NOT recomputed
    here, because by submission time the suite has already PATCHed the host for
    the next cell. Everything else is filled in from what this script can see.
    """
    cond = read_json(path) if path else None
    if cond is None:
        cond = read_json(os.path.join(rundir, "conditions.json")) or {}
    if not isinstance(cond, dict):
        cond = {}
    cond = dict(cond)

    launch = session.get("launch") or []
    negotiated = {}
    if len(launch) == 2:
        negotiated["width"] = int(launch[0])
        negotiated["height"] = int(launch[1])
    # `profile` in session.json is the SOAK profile ("observe"), not the launch
    # profile — it is tagged soak_profile and must not be read as profile_id here.
    for src, dst in (("fps", "fps"), ("codec", "codec"), ("encoder", "encoder"),
                     ("profile_id", "profile_id")):
        v = session.get(src)
        if v not in (None, "") and dst not in negotiated:
            negotiated[dst] = v
    if "profile_id" not in negotiated and tags.get("launch_profile"):
        negotiated["profile_id"] = tags["launch_profile"]
    if "encoder" not in negotiated and tags.get("encoder"):
        negotiated["encoder"] = tags["encoder"]

    # caps.negotiated (C11 #4): what the encode branch actually agreed. Prefer
    # it over the session.json-derived fields above — see read_caps_negotiated.
    caps = read_caps_negotiated(rundir)
    if caps:
        if caps.get("codec"):
            negotiated["codec"] = caps["codec"]
        if caps.get("profile"):
            negotiated["codec_profile"] = caps["profile"]
        if caps.get("encoder_factory"):
            negotiated["encoder_negotiated"] = caps["encoder_factory"]
        # The mismatch flag quasar-bench already renders: the service compares
        # every conditions.effective key against the SAME-NAMED tag (the
        # mechanism every other effective/tag pair in this function already
        # relies on — abr_mode, ladder_resolution, ...). `codec_pin` is the tag
        # bench_run.sh posts only when --codec pinned a specific codec (the
        # REQUESTED side); putting the OBSERVED codec under that same key in
        # `effective` reuses that exact comparison rather than adding a second
        # one, and only participates in it when a pin was actually made.
        if caps.get("codec") and tags.get("codec_pin"):
            cond.setdefault("effective", {})
            cond["effective"].setdefault("codec_pin", caps["codec"])

    if negotiated:
        cond.setdefault("negotiated", negotiated)

    cond.setdefault("netem", netem_conditions(marks))
    cond.setdefault("git", {k: v for k, v in (
        ("quasar", tags.get("git_quasar") or git_sha(root)),
        ("protocol", tags.get("git_protocol") or protocol_sha(root)),
        ("harness", git_sha(root)),
    ) if v})

    # Session verdict (C11 #2): the control plane's own read of this session's
    # health, fetched best-effort by bench_run.sh into <RUNDIR>/verdict.json.
    # Posted as a CONDITION (what the host actually did), not a tag (what was
    # asked for) — absent on an older control plane with no /verdict route, or
    # a session with too little data to classify yet; either way, optional.
    verdict_doc = read_json(os.path.join(rundir, "verdict.json"))
    if isinstance(verdict_doc, dict) and verdict_doc.get("verdict"):
        cond.setdefault("verdict", str(verdict_doc["verdict"]))
    return cond


def numeric_metrics(raw: dict, codebook: dict) -> dict:
    """Reduce a metrics map to numbers. bool -> 0/1; str -> `<k>_code`."""
    out = {}
    for k, v in (raw or {}).items():
        if isinstance(v, bool):
            out[k] = 1 if v else 0
        elif isinstance(v, (int, float)):
            out[k] = v
        elif isinstance(v, str):
            table = STRING_CODES.get(k)
            if table is None:
                continue  # an unknown string metric is dropped, not guessed at
            code = table.get(v, STRING_UNKNOWN)
            out[k + "_code"] = code
            codebook.setdefault(k, {})[v] = code
    return out


# ── tag derivation ───────────────────────────────────────────────────────────


def _git(root: str, *argv: str) -> str:
    try:
        out = subprocess.run(["git", "-C", root, *argv],
                             capture_output=True, text=True, timeout=15)
    except Exception:
        return ""
    return out.stdout.strip() if out.returncode == 0 else ""


def git_sha(root: str) -> str:
    return _git(root, "rev-parse", "--short=8", "HEAD")


def protocol_sha(root: str) -> str:
    """The `protocol/` submodule PIN — read from the tree, not from the submodule
    working copy. A fresh worktree has no `protocol/` checkout at all, and
    `git -C protocol rev-parse HEAD` there silently walks up and answers with the
    PARENT repo's sha (which is how git_protocol == git_quasar happens)."""
    line = _git(root, "ls-tree", "HEAD", "protocol")
    parts = line.split()
    if len(parts) >= 3 and parts[1] == "commit":
        return parts[2][:8]
    return ""


def profile_slug(session: dict) -> str:
    launch = session.get("launch") or []
    fps = session.get("fps")
    if len(launch) == 2 and fps:
        h = int(launch[1])
        name = {720: "720p", 1080: "1080p", 1440: "1440p", 2160: "4k"}.get(h, "%dp" % h)
        return "%s%d" % (name, int(fps))
    return ""


def derive_tags(session: dict, root: str) -> dict:
    tags = {}
    if not session:
        session = {}
    launch = session.get("launch") or []
    if len(launch) == 2:
        tags["launch"] = "%dx%d" % (int(launch[0]), int(launch[1]))
        tags["height"] = str(int(launch[1]))
    if session.get("fps"):
        tags["fps"] = str(int(session["fps"]))
    if session.get("codec"):
        tags["codec"] = str(session["codec"])
    slug = profile_slug(session)
    if slug:
        tags["profile"] = slug
    if session.get("id"):
        tags["session_id"] = str(session["id"])
    if session.get("host_id"):
        tags["quasar_host_id"] = str(session["host_id"])
    if session.get("bitrate_kbps"):
        tags["launch_bitrate_kbps"] = str(int(session["bitrate_kbps"]))
    if session.get("profile"):
        tags["soak_profile"] = str(session["profile"])
    q = git_sha(root)
    if q:
        tags["git_quasar"] = q
    p = protocol_sha(root)
    if p:
        tags["git_protocol"] = p
    return tags


def ext_id(suite: str, scenario: str, rundir: str, salt: str, session: dict) -> str:
    """Deterministic identity for "this run" — posted as the run's `external_id`,
    which the service upserts on (200 = updated, 201 = created).

    The IDENTITY term must be unique per actual run. The directory BASENAME is
    not: `bench_suite.sh` names each cell directory after the cell, so the same
    cell in two different output roots (a KDE matrix and a Heaven matrix) has the
    identical basename — and an ext id built on the basename alone made every
    Heaven cell collide with, and silently overwrite, its KDE counterpart. That
    happened for real on 2026-08-17: 12 runs ended up holding both matrices'
    samples merged together, which reads as a plausible-looking average.

    So: prefer the Quasar SESSION ID (globally unique, and the run is literally
    an observation of that session); fall back to the absolute directory path,
    which at least distinguishes two output roots. The basename stays in the seed
    so the id is still readable-adjacent to the cell.
    """
    identity = str((session or {}).get("id") or "") or os.path.abspath(rundir)
    seed = "|".join([suite, scenario, identity,
                     os.path.basename(os.path.normpath(rundir)), salt])
    return hashlib.sha256(seed.encode()).hexdigest()[:16]


# ── main ─────────────────────────────────────────────────────────────────────


def build_payload(rundir: str, root: str, args) -> dict:
    session = read_json(os.path.join(rundir, "session.json")) or {}
    summary = read_json(os.path.join(rundir, "summary.json")) or {}
    trace = read_json(os.path.join(rundir, "trace.json")) or {}
    marks = read_jsonl(os.path.join(rundir, "marks.jsonl"))
    steps = read_jsonl(os.path.join(rundir, "steps.jsonl"))
    metrics = read_jsonl(os.path.join(rundir, "metrics.jsonl"))

    codebook: dict = {}
    samples = []
    for row in metrics:
        ts = row.get("ts_unix_ms")
        src = row.get("source")
        if ts is None or not src:
            continue
        m = numeric_metrics(row.get("metrics") or {}, codebook)
        if not m:
            continue
        samples.append({"ts_unix_ms": int(ts), "source": str(src), "metrics": m})

    # Advisory only — NOT posted as a sample. See ROLLUP_KEYS above.
    rollup = phase_rollup(samples, marks, args.settle_secs, args.warmup_secs)

    events = []
    for ev in (trace.get("events") or []) if isinstance(trace, dict) else []:
        if ev.get("ts_unix_ms") is None or not ev.get("type"):
            continue
        payload = dict(ev.get("payload") or {})
        if ev.get("source"):
            payload.setdefault("source", ev["source"])
        events.append({"ts_unix_ms": int(ev["ts_unix_ms"]), "type": str(ev["type"]),
                       "payload": payload})
    for mk in marks:
        if mk.get("ts_unix_ms") is None:
            continue
        kind = mk.get("mark")
        typ = "netem.clear" if kind == "clear" else "netem.impair"
        payload = {k: v for k, v in mk.items() if k not in ("ts_unix_ms", "mark")}
        events.append({"ts_unix_ms": int(mk["ts_unix_ms"]), "type": typ, "payload": payload})
    for st in steps:
        # `t0_unix_ms` is the observe-profile driver's key; the older ladder/
        # sawtooth/floor step records call the same instant `t_start_ms`.
        ts = st.get("t0_unix_ms") or st.get("t_start_ms") or st.get("ts_unix_ms")
        if ts is None:
            continue
        events.append({"ts_unix_ms": int(ts), "type": "harness.step",
                       "payload": {k: v for k, v in st.items() if k != "kind"}})
    if codebook:
        anchor = samples[0]["ts_unix_ms"] if samples else (
            session.get("started_at_ms") or 0)
        events.append({"ts_unix_ms": int(anchor), "type": "harness.string_codes",
                       "payload": codebook})
    events += phase_marks(samples, marks, args.settle_secs, args.warmup_secs)

    tags = derive_tags(session, root)
    # C11 #4: what actually ran, from caps.negotiated — only fills a tag
    # derive_tags left unset (session.json's own `codec`, when present, is
    # already good; caps.negotiated is the fallback/enhancement for the common
    # bench_run.sh case where session.json does not exist at all). `profile`
    # here is the codec profile (main/high/...), not the launch profile — kept
    # under `codec_profile` so it cannot be confused with the `launch_profile`
    # tag; `encoder` is kept under `encoder_negotiated` so it cannot collide
    # with the HOST-configured `encoder` tag (e.g. vulkan/nvenc), a different
    # vocabulary describing the knob rather than the GStreamer element that
    # actually bound.
    caps = read_caps_negotiated(rundir)
    if caps:
        if caps.get("codec"):
            tags.setdefault("codec", str(caps["codec"]))
        if caps.get("profile"):
            tags.setdefault("codec_profile", str(caps["profile"]))
        if caps.get("encoder_factory"):
            tags.setdefault("encoder_negotiated", str(caps["encoder_factory"]))
    tags.update(args.tags)  # explicit --tag always wins over a derived value
    tags["source_dir"] = os.path.basename(os.path.normpath(rundir))
    external_id = ext_id(args.suite, args.scenario, rundir,
                         args.ext_id_salt, session)
    conditions = build_conditions(rundir, session, marks, tags, root,
                                  args.conditions)

    # Session verdict (C11 #2), read back for the validity decision in main().
    # build_conditions already folded the same file into conditions["verdict"];
    # this is a second, cheap read rather than a threaded-through return value.
    verdict_doc = read_json(os.path.join(rundir, "verdict.json"))
    session_verdict = None
    session_verdict_evidence: list = []
    if isinstance(verdict_doc, dict) and verdict_doc.get("verdict"):
        session_verdict = str(verdict_doc["verdict"])
        session_verdict_evidence = verdict_doc.get("evidence") or []

    verdict = args.verdict or summary.get("overall")
    if verdict not in ("PASS", "FAIL", "INFO"):
        verdict = {"DEGRADED": "INFO", "UNKNOWN": "INFO", "N/A": "INFO"}.get(verdict, "INFO")

    final_summary = dict(summary)
    final_summary["harness"] = {
        "submitted_from": os.path.basename(os.path.normpath(rundir)),
        "n_samples_posted": len(samples),
        "n_events_posted": len(events),
        "settle_secs": args.settle_secs,
        "warmup_secs": args.warmup_secs,
    }
    if rollup:
        final_summary["phases"] = rollup
    if codebook:
        final_summary["string_metric_codes"] = codebook

    artifacts = [os.path.join(rundir, n) for n in ARTIFACT_FILES
                 if os.path.exists(os.path.join(rundir, n))]
    # Any OTHER markdown in the directory is a hand-written companion report
    # (PIN-RELEASE.md, NOTES.md, ...). Upload it too rather than silently
    # dropping the only artefact some archived runs have.
    for name in sorted(os.listdir(rundir)):
        if name.endswith(".md") and name not in ARTIFACT_FILES:
            artifacts.append(os.path.join(rundir, name))
    artifacts += [os.path.abspath(a) for a in args.extra_artifacts]

    return {"tags": tags, "samples": samples, "events": events, "verdict": verdict,
            "summary": final_summary, "artifacts": artifacts, "session": session,
            "external_id": external_id, "conditions": conditions,
            "session_verdict": session_verdict,
            "session_verdict_evidence": session_verdict_evidence}


# Samples upsert on (run, source, ts_unix_ms) and nothing ever deletes. So
# re-submitting a run whose sample TIMESTAMPS have moved does not replace the
# old series — it ACCUMULATES a second one, interleaved, at a different time
# offset. That is not merely double-counting: the shifted copy lands in the
# wrong phase, so every `window=`-scoped aggregate silently mixes warmup windows
# into the observe window.
#
# It happened twice in one evening on suite `opt-t1` (2026-08-18): once from a
# wrong ordinal anchor, and once from two agents re-folding the same runs
# concurrently with different --t0-ms. Eleven of twelve runs ended up holding
# 552-617 browser bench windows where 276 was correct, and there is NO signal
# from the outside except an implausible window count. So: refuse, don't warn.

#: keys that identify a browser bench-window series (old and current names)
BENCH_WINDOW_KEYS = ("missing_indices", "dropped")
#: how far a re-submitted window may move and still count as "the same instant"
SHIFT_TOLERANCE_MS = 1000


def shifted_resubmit_problem(prev, incoming):
    """Pure decision: message when submitting `incoming` would append a shifted
    copy alongside `prev`, else None.

    prev     -- (count, lo_ms, hi_ms) already stored, or None
    incoming -- sorted list of the ts about to be posted
    """
    if not incoming or not prev:
        return None
    n, lo, hi = prev
    if (abs(incoming[0] - lo) <= SHIFT_TOLERANCE_MS
            and abs(incoming[-1] - hi) <= SHIFT_TOLERANCE_MS):
        return None
    return ("this run already holds %d browser bench windows spanning %d..%d, and "
            "this submission carries %d spanning %d..%d — a shift of %+.1f s at the "
            "start. Samples upsert on (run, source, ts) and NOTHING deletes, so this "
            "would ADD a second, time-shifted copy rather than replace the first, and "
            "every window-scoped aggregate for this run would then be wrong. "
            "Re-submit with the ORIGINAL anchor, or submit to a fresh suite/scenario. "
            "--force-shifted overrides."
            % (n, lo, hi, len(incoming), incoming[0], incoming[-1],
               (incoming[0] - lo) / 1000.0))


#: a browser bench window is emitted once per second, so a run's window count
#: should track its own duration. Allow 5% either way plus a couple of windows
#: of slack for the partial windows at each end.
WINDOW_COUNT_TOLERANCE = 0.05
WINDOW_COUNT_SLACK = 2


def corrupt_window_series(prev):
    """Message when a run's STORED window series is implausible, else None.

    Independent of what is being submitted: a run holding materially more
    one-second windows than it has seconds is corrupt however it got that way.
    On suite `opt-t1` a 275 s run held 617 windows and nothing anywhere said so —
    the count was the only signal, and neither of the two agents looking at it
    checked. So check it, every time, before adding to the pile.
    """
    if not prev:
        return None
    n, lo, hi = prev
    secs = (hi - lo) / 1000.0
    if secs <= 0:
        return None
    expected = secs + 1                          # half-open span -> N+1 windows
    ceiling = expected * (1 + WINDOW_COUNT_TOLERANCE) + WINDOW_COUNT_SLACK
    if n <= ceiling:
        return None
    return ("this run already holds %d browser bench windows across only %.0f s "
            "(expected about %.0f). A one-second window series cannot outnumber "
            "its own seconds, so the stored series is ALREADY corrupt — almost "
            "certainly duplicate time-shifted copies from a previous re-fold. "
            "Adding to it makes it worse. Submit to a fresh suite/scenario "
            "instead. --force-shifted overrides."
            % (n, secs, expected))


def stored_window_span(b, run_id: str):
    """(count, lo_ms, hi_ms) of a run's stored browser bench windows, or None.

    Own urllib call rather than the vendored client's private `_get`: the vendor
    is a verbatim copy and must stay re-vendorable, so nothing here may depend on
    its internals. `url`/`key` are its public attributes.
    """
    try:
        req = urllib.request.Request("%s/v1/runs/%s/samples" % (b.url, run_id))
        req.add_header("Authorization", "Bearer %s" % b.key)
        with urllib.request.urlopen(req, timeout=30) as fh:
            d = json.load(fh)
    except Exception:
        return None                      # read failed: never block a submit on it
    for ser in (d or {}).get("series") or []:
        if ser.get("source") == "browser" and ser.get("key") in BENCH_WINDOW_KEYS:
            pts = [p["ts_unix_ms"] for p in (ser.get("points") or [])
                   if isinstance(p.get("ts_unix_ms"), int)]
            if pts:
                return len(pts), min(pts), max(pts)
    return None


#: first service version whose POST /samples honours `replace` + `expected_count`.
#: An older service IGNORES unknown body fields, so on it a re-fold silently
#: appends the time-shifted copy again — hence the version gate below.
REPLACE_MIN_SERVICE = "1.2.0"


def server_can_replace(url: str) -> bool:
    theirs = server_version(url)
    return bool(theirs) and _ver_tuple(theirs) >= _ver_tuple(REPLACE_MIN_SERVICE)


def expected_counts(samples: list) -> dict:
    """Per-source row counts of what is about to be posted — the service checks
    the stored count against these AFTER the write, inside the transaction, and
    rolls back on mismatch (bench 1.2 `expected_count`)."""
    out: dict = {}
    for s in samples:
        src = s.get("source")
        if src:
            out[src] = out.get(src, 0) + 1
    return out


def refuse_on_shifted_resubmit(b, run_id: str, samples: list, force: bool,
                               can_replace: bool = False) -> None:
    """Guard a re-submission into an existing run.

    Service >= 1.2 (`can_replace`): the submit posts with `replace=True`, which
    clears the stored series for every source in the payload first — a shifted
    or corrupt stored series is then simply overwritten, so only INFO it.
    Older service: samples upsert on (run, source, ts) and nothing deletes, so
    a shifted re-fold would APPEND a second copy — refuse unless forced.
    """
    incoming = sorted(s["ts_unix_ms"] for s in samples
                      if s.get("source") == "browser"
                      and any(k in (s.get("metrics") or {}) for k in BENCH_WINDOW_KEYS))
    prev = stored_window_span(b, run_id)
    problem = corrupt_window_series(prev) or shifted_resubmit_problem(prev, incoming)
    if problem is None:
        return
    if can_replace:
        print("INFO  run %s holds a stored browser window series that differs from "
              "this submission (%d windows %s..%s); posting with replace=True so the "
              "stored series is REPLACED, not appended to."
              % (run_id, prev[0], prev[1], prev[2]), file=sys.stderr)
        return
    if force:
        print("WARN  run %s: %s (proceeding: --force-shifted)" % (run_id, problem))
        return
    die("run %s: %s" % (run_id, problem))


def main(argv=None) -> int:
    p = argparse.ArgumentParser(prog="bench_submit.py", description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--dir", required=True, help="the soak/observe run directory")
    p.add_argument("--suite", required=True)
    p.add_argument("--scenario", required=True)
    p.add_argument("--host", default="devbox", help="which Quasar host produced the run")
    p.add_argument("--tag", action="append", default=[], metavar="K=V",
                   help="repeatable; wins over any derived tag")
    p.add_argument("--notes", default="")
    p.add_argument("--verdict", default=None, choices=["PASS", "FAIL", "INFO"])
    p.add_argument("--status", default="finished")
    p.add_argument("--run-id", default=None, help="submit into this existing run")
    p.add_argument("--new", action="store_true",
                   help="always create a new run (post no external_id, so the "
                        "service cannot upsert onto the previous submission)")
    p.add_argument("--ext-id-salt", default="",
                   help="disambiguate two submissions of the same directory")
    p.add_argument("--conditions", default=None, metavar="PATH",
                   help="conditions JSON (default: <RUNDIR>/conditions.json) — the "
                        "host's `effective` settings map at launch, netem state and "
                        "concurrent session count, as bench_run.sh captures them")
    p.add_argument("--force-shifted", action="store_true",
                   help="(bench service < 1.2 only) submit even when this run "
                        "already holds browser bench windows at DIFFERENT timestamps, "
                        "which on an old service appends a second, time-shifted copy. "
                        "On bench >= 1.2 submits always REPLACE the stored series "
                        "(per source) under an expected_count guard, so this is moot")
    p.add_argument("--warmup-secs", type=float, default=thresholds.warmup_exclude_s(),
                   help="for an UNIMPAIRED run, how many seconds at the start are "
                        "peer-attach + app-start transient. Splits the single `run` "
                        "phase into `warmup` + `observe` so a query can ask for the "
                        "steady state (default: docs/session-trace/thresholds.json's "
                        "classifier.warmup_exclude_s — the same warm-up window the "
                        "session verdict itself excludes; pass 0 for no split). "
                        "bench_run.sh always passes its own --settle value explicitly, "
                        "so this default matters only to a bare CLI/API call")
    p.add_argument("--settle-secs", type=float, default=30.0,
                   help="seconds of impaired-window transient excluded from the "
                        "`settled` phase window and rollup (default 30)")
    p.add_argument("--no-artifacts", action="store_true")
    p.add_argument("--artifact", action="append", default=[], dest="extra_artifacts",
                   metavar="PATH", help="repeatable; upload an extra file (app logs, CSVs)")
    p.add_argument("--dry-run", action="store_true",
                   help="print the plan; contact the service only for the id lookup")
    p.add_argument("--url", default=None)
    p.add_argument("--key", default=None)
    args = p.parse_args(argv)

    try:
        args.tags = dict(t.split("=", 1) for t in args.tag)
    except ValueError:
        die("every --tag must be K=V", 2)

    rundir = os.path.abspath(args.dir)
    if not os.path.isdir(rundir):
        die("no such run directory: %s" % rundir, 2)

    root = os.path.abspath(os.path.join(DX_DIR, "..", ".."))
    plan = build_payload(rundir, root, args)

    print("     dir       %s" % rundir)
    print("     suite     %s" % args.suite)
    print("     scenario  %s" % args.scenario)
    print("     host      %s" % args.host)
    print("     samples   %d" % len(plan["samples"]))
    print("     events    %d" % len(plan["events"]))
    print("     artifacts %s" % (", ".join(os.path.basename(a) for a in plan["artifacts"]) or "none"))
    print("     verdict   %s" % plan["verdict"])
    print("     tags      %s" % json.dumps(plan["tags"], sort_keys=True))
    print("     ext_id    %s" % plan["external_id"])
    print("     conditions %s" % json.dumps(plan["conditions"], sort_keys=True))
    print("     session_verdict %s" % (plan.get("session_verdict") or "n/a"))

    if args.dry_run and not (args.url or os.environ.get("BENCH_URL")):
        print("PASS plan — dry run, no service contacted")
        print("RESULT status=ok target=bench-submit dry_run=1 samples=%d events=%d"
              % (len(plan["samples"]), len(plan["events"])))
        return 0

    key = args.key if args.key is not None else os.environ.get("BENCH_KEY")
    key = normalize_bench_key(key, "--key" if args.key is not None else "BENCH_KEY")
    # #508 same-issue spirit: neither --url nor $BENCH_URL was given, so the
    # vendored client silently falls back to bench_client.DEFAULT_URL
    # (http://localhost:9400). That is correct for a local quasar-bench, but
    # when the intended target is a remote service the first sign of trouble
    # is a bare "Connection refused" against localhost with no mention of
    # BENCH_URL anywhere — confusing exactly like the QSES_ADMIN_TOKEN case
    # above. Remember whether the URL was explicit so a connection failure
    # below can name the actual cause instead.
    bench_url_explicit = bool(args.url or os.environ.get("BENCH_URL"))
    try:
        b = Bench(args.url, key)
    except BenchError as exc:
        die(str(exc), 2)
    check_client_version(b.url)

    if args.dry_run:
        print("PASS plan — dry run; would %s"
              % ("update run %s" % args.run_id if args.run_id
                 else "POST /v1/runs (upsert on external_id)"))
        print("RESULT status=ok target=bench-submit dry_run=1 samples=%d events=%d"
              % (len(plan["samples"]), len(plan["events"])))
        return 0

    run_id = args.run_id
    try:
        if not run_id:
            # The service UPSERTS on external_id: 201 when it created the run, 200
            # when it updated the previous submission of the same cell. The client
            # returns the id either way, and a re-submission converging onto the
            # same run is the point — so the two are not distinguished here.
            run_id = b.new_run(args.suite, args.scenario, args.host,
                               plan["tags"], args.notes,
                               conditions=plan["conditions"],
                               external_id=None if args.new else plan["external_id"])
            created = run_id
            print("PASS run — %s (external_id %s)" % (run_id, plan["external_id"]))
        # Guard BEFORE writing. On bench >= 1.2 the write itself is safe:
        # replace=True swaps the stored series per source, expected_count makes
        # the service roll back if the stored count is not exactly ours (the
        # 341-vs-276 stale tail). Older service: refuse a shifted re-fold.
        can_replace = server_can_replace(b.url)
        refuse_on_shifted_resubmit(b, run_id, plan["samples"], args.force_shifted,
                                   can_replace=can_replace)
        if plan["samples"]:
            if can_replace:
                n_s = b.samples(run_id, plan["samples"], replace=True,
                                expected_count=expected_counts(plan["samples"]))
            else:
                n_s = b.samples(run_id, plan["samples"])
        else:
            n_s = 0
        n_e = b.events(run_id, plan["events"]) if plan["events"] else 0
        n_a = 0
        if not args.no_artifacts:
            for path in plan["artifacts"]:
                b.artifact(run_id, path)
                n_a += 1
        fin = b.finish(run_id, args.status, plan["verdict"], plan["summary"],
                       plan["tags"], conditions=plan["conditions"])
    except BenchError as exc:
        msg = str(exc)
        # A raw connection failure (refused/unreachable/no route/timed out)
        # against the un-overridden DEFAULT_URL almost always means the
        # caller forgot to set BENCH_URL for a remote submit, not that the
        # local service is actually down — name the real cause.
        if not bench_url_explicit and any(
            s in msg for s in ("Connection refused", "Errno 61", "Errno 111",
                                "No route to host", "Name or service not known",
                                "timed out")
        ):
            msg = ("%s — BENCH_URL is unset, so this defaulted to %s. "
                   "If the target quasar-bench service is remote, set BENCH_URL "
                   "(e.g. BENCH_URL=http://bench.example.internal:9400, per this script's own "
                   "docstring) and retry." % (msg, b.url))
        die(msg)

    # Session verdict (C11 #2): control-plane/internal/session/verdict.go's
    # classifier is observational, not pass/fail — its closed vocabulary is
    # nominal / likely_network_congestion / likely_encoder_saturation /
    # likely_client_presentation_limit / indeterminate_client_hidden / unknown,
    # with no literal "failed". The three likely_* verdicts are the closest
    # analogue: a live negative signal fired somewhere in the window, so this
    # run's evidence should not silently average into a clean baseline. Mark
    # those validity=contaminated — never withheld, never dropped, just flagged —
    # and never let this step fail the submission that already succeeded.
    sess_verdict = plan.get("session_verdict")
    if sess_verdict and sess_verdict.startswith("likely_"):
        reason = "; ".join(str(x) for x in plan.get("session_verdict_evidence") or []) \
                 or ("session verdict=%s" % sess_verdict)
        try:
            b.patch(run_id, validity="contaminated", validity_reason=reason)
            print("WARN  session verdict — %s; run %s marked validity=contaminated (%s)"
                  % (sess_verdict, run_id, reason))
        except BenchError as exc:
            print("WARN  session verdict — %s, but could not set validity=contaminated: %s"
                  % (sess_verdict, exc), file=sys.stderr)

    # `mismatches` lives on the run DETAIL (the Run shape returned by create and
    # finish does not carry it), so read it back rather than trusting the write.
    mismatches = (fin or {}).get("mismatches")
    if mismatches is None:
        try:
            mismatches = (b.run(run_id) or {}).get("mismatches") or []
        except BenchError:
            mismatches = []

    url = "%s/runs/%s" % (b.url, run_id)
    print("PASS samples — %d written" % n_s)
    print("PASS events — %d written" % n_e)
    print("PASS artifacts — %d uploaded" % n_a)
    print("     %s" % url)

    if mismatches:
        # The cell is MISLABELLED: a tag says one thing, the host reported
        # another. Do NOT mark the run invalid automatically — the evidence may
        # be perfectly good and only the label wrong, and that is a human call.
        # Do tag it so it can be excluded, and exit non-zero so a suite stops
        # instead of grinding out eleven more cells under the same wrong knob.
        print("")
        for m in mismatches:
            print("FAIL mismatch — tag %s=%r but the host reported %r"
                  % (m.get("key"), m.get("intended"), m.get("effective")), file=sys.stderr)
        try:
            b.patch(run_id, tags={"mismatch": "1"})
        except BenchError as exc:
            print("WARN mismatch — could not tag the run: %s" % exc, file=sys.stderr)
        print("RESULT status=failed target=bench-submit run_id=%s url=%s samples=%d "
              "events=%d verdict=%s mismatches=%d"
              % (run_id, url, n_s, n_e, plan["verdict"], len(mismatches)))
        return MISMATCH_RC

    print("RESULT status=ok target=bench-submit run_id=%s url=%s samples=%d events=%d verdict=%s"
          % (run_id, url, n_s, n_e, plan["verdict"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
