#!/usr/bin/env python3
"""scripts/dx/session_soak_driver.py — the execution half of `make session-soak`.

Walks a LIVE session's EXTERNAL (stream) resolution down the rung ladder and
back up over a fixed wall-clock duration, sampling telemetry throughout, and
emits one NDJSON record per event on stdout. Human-readable progress goes to
stderr, so `driver > raw.ndjson` gives a clean machine record while the
operator still watches the walk.

It is deliberately a SINGLE self-contained stdlib-only program because it has to
run either on this workstation (HOST=local) or, unmodified, piped over one ssh
into `python3 -` on the stack host (HOST=<role>). That is also why the
concurrency lives here (a sampler thread) rather than in a bash background
subshell: a 200 ms echo poll plus a 2 s metrics poll for 180 s is ~1000 requests,
which is fine in-process on the host and ruinous as ~1000 ssh round trips.

It NEVER sends render_width/render_height. Proving the internal (app-render)
size stays put across an external resize is the entire point of the soak, so the
render readback is recorded before and after every step and never written.

NDJSON record kinds on stdout:
  {"kind":"session", ...}   the resolved session (launch size, rungs, encoder, codec)
  {"kind":"schedule","steps":[...]}   the computed walk, emitted before step 1
  {"kind":"step", ...}      one executed step (PATCH code/latency, echo latency, render before/after)
  {"kind":"metric","source":..,"ts_unix_ms":..,"metrics":{..}}  one deduped telemetry sample
  {"kind":"trace","data":{..}}        the trace/window pull, if the endpoint answered
  {"kind":"verdict","data":{..}}      the control plane's Verdict for the run window
  {"kind":"info"|"error","msg":...}

Exit codes: 0 = the walk ran (individual step failures are DATA, not a harness
failure — the report judges them); 2 = usage; 3 = the session's encoder cannot
live-resize (`stream.external_resize_supported == false`); 1 = harness failure
(login, unreachable API, no such session).
"""

import argparse
import json
import os
import signal
import ssl
import sys
import threading
import time
import urllib.error
import urllib.request

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE

EXIT_UNSUPPORTED = 3
MIN_DWELL_S = 4.0  # the report skips the first 3 s of every dwell as transient


def emit(rec):
    sys.stdout.write(json.dumps(rec, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def note(msg):
    sys.stderr.write("     %s\n" % msg)
    sys.stderr.flush()


# ── HTTP ─────────────────────────────────────────────────────────────────────
class Api(object):
    def __init__(self, base, token=None):
        self.base = base.rstrip("/")
        self.token = token

    def call(self, method, path, body=None, timeout=10, headers=None):
        """-> (http_code, parsed_or_None, elapsed_ms, transport_error_or_None).

        A non-2xx is returned as its code with the parsed body; only a transport
        failure yields code 0 plus an error string. Callers judge codes."""
        data = json.dumps(body).encode("utf-8") if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method)
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=timeout, context=CTX) as r:
                raw, code = r.read(), r.getcode()
        except urllib.error.HTTPError as e:
            raw, code = e.read(), e.code
        except Exception as e:  # noqa: BLE001 — transport, not protocol
            return 0, None, (time.time() - t0) * 1000.0, str(e)
        ms = (time.time() - t0) * 1000.0
        try:
            parsed = json.loads(raw.decode("utf-8")) if raw else None
        except Exception:
            parsed = None
        return code, parsed, ms, None


def login(api, email, password):
    code, body, _, err = api.call(
        "POST", "/v1/auth/login", {"email": email, "password": password}
    )
    if err:
        return None, "login transport failure: %s" % err
    if code != 200 or not isinstance(body, dict) or not body.get("access_token"):
        return None, "login -> HTTP %s (check BOOTSTRAP_ADMIN_* or pass QSES_ADMIN_TOKEN)" % code
    return body["access_token"], None


def dev_mint(api, dev_key, ttl=3600):
    """POST /v1/dev/agent-session (QUASAR_DEV_AGENT_AUTH=1 stacks only) -> admin bearer."""
    code, body, _, err = api.call(
        "POST",
        "/v1/dev/agent-session",
        {"role": "admin", "ttl_seconds": ttl},
        headers={"X-Quasar-Dev-Key": dev_key},
    )
    if err:
        return None, "dev mint transport failure: %s" % err
    if code != 200 or not isinstance(body, dict) or not body.get("access_token"):
        return None, "dev mint -> HTTP %s (is QUASAR_DEV_AGENT_AUTH=1 on the stack?)" % code
    return body["access_token"], None


# ── Rung ladder (mirrors control-plane/internal/profile/rungs.go) ────────────
RUNG_FAMILIES = [
    ([(16, 9)], [(3840, 2160), (2560, 1440), (1920, 1080), (1600, 900), (1280, 720)]),
    ([(8, 5)], [(2560, 1600), (1920, 1200), (1680, 1050), (1440, 900), (1280, 800)]),
    ([(43, 18), (64, 27), (7, 3)], [(3440, 1440), (2560, 1080)]),
    ([(4, 3)], [(1600, 1200), (1280, 960), (1024, 768)]),
]


def _gcd(a, b):
    while b:
        a, b = b, a % b
    return abs(a)


def available_rungs(w, h):
    """profile.AvailableRungs: launch size first, then same-family rungs <= launch."""
    out = [(w, h)]
    if w <= 0 or h <= 0:
        return out
    g = _gcd(w, h) or 1
    ratio = (w // g, h // g)
    family = None
    for ratios, rungs in RUNG_FAMILIES:
        if ratio in ratios:
            family = rungs
            break
    if family is None:
        return out
    for rw, rh in family:
        if rw > w or rh > h or (rw == w and rh == h):
            continue
        out.append((rw, rh))
    return out


# ── Schedule ─────────────────────────────────────────────────────────────────
def build_schedule(rungs, duration, profile, dwell=None):
    """-> [{"w","h","dwell_s","label"}] covering `duration` seconds.

    rungs is descending with the launch size first. Every profile starts AND
    ends at the launch size so a Ctrl-C-free run leaves the session as it found
    it even before the restore trap fires."""
    launch = rungs[0]
    bottom = rungs[-1]
    n = len(rungs)

    def entry(wh, secs, label):
        return {"w": wh[0], "h": wh[1], "dwell_s": round(max(secs, MIN_DWELL_S), 2), "label": label}

    if profile == "observe":
        # The ABR ladder is the actor here; the harness only watches. One entry at the
        # LAUNCH size for the whole duration means `run_step` issues exactly one PATCH
        # — and `run_step` skips even that one for this profile (see below), so the run
        # is genuinely PATCH-free and cannot perturb what it is measuring.
        return [entry(launch, duration, "observe (no patches)")]

    if n == 1:
        # No ladder (aspect ratio with no family, or already at the floor).
        return [entry(launch, duration, "launch (no ladder)")]

    if profile == "floor":
        head = duration * 0.15
        tail = duration * 0.15
        return [
            entry(launch, head, "launch hold"),
            entry(bottom, duration - head - tail, "floor hold"),
            entry(launch, tail, "restore"),
        ]

    if profile == "sawtooth":
        d = dwell if dwell else max(duration / 8.0, MIN_DWELL_S)
        out = []
        spent = 0.0
        while spent + 2 * d <= duration:
            out.append(entry(launch, d, "launch"))
            out.append(entry(bottom, d, "floor"))
            spent += 2 * d
        if not out:
            out = [entry(launch, duration / 2.0, "launch"), entry(bottom, duration / 2.0, "floor")]
            spent = duration
        out.append(entry(launch, max(duration - spent, MIN_DWELL_S), "restore"))
        return out

    # ladder (default): hold launch, step down rung by rung, hold the bottom
    # (2 units), step back up, finish at launch for the remainder.
    head = duration * 0.15
    down = rungs[1:]            # n-1 entries, last is the bottom
    up = list(reversed(rungs[1:-1]))  # n-2 intermediate entries on the way back
    units = (len(down) - 1) + 2 + len(up)  # bottom counts double
    if dwell:
        d = dwell
    else:
        d = max((duration - 2 * head) / float(units), MIN_DWELL_S)
    out = [entry(launch, head, "launch hold")]
    for i, wh in enumerate(down):
        last = i == len(down) - 1
        out.append(entry(wh, d * (2 if last else 1), "floor hold" if last else "step down"))
    for wh in up:
        out.append(entry(wh, d, "step up"))
    spent = sum(e["dwell_s"] for e in out)
    out.append(entry(launch, max(duration - spent, MIN_DWELL_S), "restore"))
    return out


# ── Session read ─────────────────────────────────────────────────────────────
def read_session(api, sid):
    # Test hook: QSOAK_SESSION_FIXTURE=<file> serves a canned GET /v1/sessions/{id}
    # body so a --dry-run (and the report round trip) can be exercised with no
    # stack at all. Never set in real use.
    fixture = os.environ.get("QSOAK_SESSION_FIXTURE", "")
    if fixture:
        with open(fixture) as f:
            return (json.load(f).get("session") or {}), None
    code, body, _, err = api.call("GET", "/v1/sessions/" + sid)
    if err:
        return None, "GET session transport failure: %s" % err
    if code != 200 or not isinstance(body, dict):
        return None, "GET /v1/sessions/%s -> HTTP %s" % (sid, code)
    return body.get("session") or {}, None


def stream_of(sess):
    return (sess or {}).get("stream") or {}


def external_of(sess):
    s = stream_of(sess)
    w, h = s.get("external_width"), s.get("external_height")
    if w is None or h is None:
        return None
    return (w, h)


def render_of(sess):
    """Session-GET render readback. Absent on a control plane that only exposes
    the render size through session_metrics — the agent sample is the other
    (and authoritative) half of the internal-untouched check."""
    s = stream_of(sess)
    w = s.get("render_width")
    h = s.get("render_height")
    if w is None or h is None:
        return None
    return (w, h)


def latest_running(api):
    code, body, _, err = api.call("GET", "/v1/admin/sessions?limit=50")
    if err or code != 200 or not isinstance(body, dict):
        return None, "GET /v1/admin/sessions -> HTTP %s%s" % (code, (" " + err) if err else "")
    running = [i for i in (body.get("items") or []) if i.get("state") == "running"]
    if not running:
        return None, "no session in state=running on this stack"
    running.sort(key=lambda i: i.get("created_at") or "", reverse=True)
    return running[0], None


# ── Sampler ──────────────────────────────────────────────────────────────────
class Sampler(threading.Thread):
    """Polls GET /v1/admin/sessions/{id}/metrics every `every` seconds and emits
    only samples it has not seen before (dedupe on source+ts_unix_ms)."""

    def __init__(self, api, sid, stop_evt, every=2.0, limit=64):
        threading.Thread.__init__(self)
        self.daemon = True
        self.api, self.sid, self.stop_evt = api, sid, stop_evt
        self.every, self.limit = every, limit
        self.seen = set()
        self.count = 0
        self.errors = 0
        self.lock = threading.Lock()

    def poll_once(self):
        code, body, _, err = self.api.call(
            "GET", "/v1/admin/sessions/%s/metrics?limit=%d" % (self.sid, self.limit)
        )
        if err or code != 200 or not isinstance(body, dict):
            self.errors += 1
            return
        for item in body.get("items") or []:
            key = (item.get("source"), item.get("ts_unix_ms"))
            if key in self.seen:
                continue
            self.seen.add(key)
            self.count += 1
            with self.lock:
                emit(
                    {
                        "kind": "metric",
                        "source": item.get("source"),
                        "ts_unix_ms": item.get("ts_unix_ms"),
                        "metrics": item.get("metrics") or {},
                    }
                )

    def run(self):
        while not self.stop_evt.is_set():
            try:
                self.poll_once()
            except Exception as e:  # noqa: BLE001 — a sampler must never kill the walk
                self.errors += 1
                note("sampler error (continuing): %s" % e)
            self.stop_evt.wait(self.every)
        try:
            self.poll_once()  # final drain
        except Exception:
            pass


# ── Step execution ───────────────────────────────────────────────────────────
def run_step(api, sid, idx, total, ent, profile_name, echo_timeout, poll_every, stop_evt):
    target = (ent["w"], ent["h"])
    label = "soak: stream %dx%d (step %d/%d, %s)" % (ent["w"], ent["h"], idx, total, profile_name)
    t0_wall = time.time()
    t0_ms = int(t0_wall * 1000)

    if profile_name == "observe":
        # Observe mode never PATCHes: it annotates the timeline, then polls until the
        # dwell expires. The sampler thread keeps collecting telemetry throughout.
        emit({"kind": "step", "idx": idx, "total": total, "profile": profile_name,
              "target": [ent["w"], ent["h"]], "label": ent["label"],
              "patch_code": None, "observed_only": True,
              "t0_unix_ms": t0_ms, "dwell_s": ent["dwell_s"]})
        note("step %d/%d  observe  %dx%d  holding for %.0fs (no PATCH)"
             % (idx, total, ent["w"], ent["h"], ent["dwell_s"]))
        stop_evt.wait(ent["dwell_s"])
        return

    # Annotation is best-effort decoration on the trace timeline; a 404/403 here
    # must never abort the walk.
    ann_code, _, _, _ = api.call(
        "POST",
        "/v1/admin/sessions/%s/trace/annotations" % sid,
        {"ts_unix_ms": t0_ms, "label": label, "tags": ["soak", profile_name]},
    )

    before, err = read_session(api, sid)
    render_before = render_of(before) if before else None
    external_before = external_of(before) if before else None

    code, body, patch_ms, perr = api.call(
        "PATCH",
        "/v1/sessions/%s/display" % sid,
        {"stream_width": ent["w"], "stream_height": ent["h"]},
    )

    echo_ms = None
    echoed = False
    deadline = time.time() + echo_timeout
    after = before
    while time.time() < deadline and not stop_evt.is_set():
        time.sleep(poll_every)
        after, _ = read_session(api, sid)
        if after and external_of(after) == target:
            echo_ms = (time.time() - t0_wall) * 1000.0
            echoed = True
            break
    render_after = render_of(after) if after else None

    rec = {
        "kind": "step",
        "index": idx,
        "total": total,
        "profile": profile_name,
        "label": ent["label"],
        "target_w": ent["w"],
        "target_h": ent["h"],
        "dwell_s": ent["dwell_s"],
        "t_start_ms": t0_ms,
        "t_end_ms": int((t0_wall + ent["dwell_s"]) * 1000),
        "annotation_code": ann_code,
        "patch_code": code,
        "patch_ms": round(patch_ms, 1),
        "patch_error": perr,
        "patch_body_error": (body or {}).get("error") if isinstance(body, dict) else None,
        "echo_ms": round(echo_ms, 1) if echo_ms is not None else None,
        "echoed": echoed,
        "external_before": list(external_before) if external_before else None,
        "render_before": list(render_before) if render_before else None,
        "render_after": list(render_after) if render_after else None,
        "ok": bool(echoed and code == 202),
    }
    emit(rec)
    status = "echo %sms" % rec["echo_ms"] if echoed else "NO ECHO in %.0fs" % echo_timeout
    note(
        "step %d/%d  %dx%d  %-11s PATCH %s (%.0f ms)  %s"
        % (idx, total, ent["w"], ent["h"], ent["label"], code, patch_ms, status)
    )

    # Hold the rest of the dwell (the echo poll already consumed part of it).
    remain = ent["dwell_s"] - (time.time() - t0_wall)
    if remain > 0:
        stop_evt.wait(remain)
    return rec


# ── main ─────────────────────────────────────────────────────────────────────
def main(argv):
    p = argparse.ArgumentParser(add_help=True)
    p.add_argument("--api", default=os.environ.get("QSOAK_API", ""))
    p.add_argument("--sid", default="")
    p.add_argument("--latest", action="store_true")
    p.add_argument("--duration", type=float, default=180.0)
    p.add_argument("--profile", default="ladder", choices=["ladder", "sawtooth", "floor", "observe"])
    p.add_argument("--dwell", type=float, default=0.0)
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--launch", default="", help="WxH — dry-run only; skips every API call")
    p.add_argument("--rungs", default="", help="W1xH1,W2xH2,... — override the ladder")
    p.add_argument("--echo-timeout", type=float, default=5.0)
    p.add_argument("--poll-every", type=float, default=0.2)
    p.add_argument("--sample-every", type=float, default=2.0)
    a = p.parse_args(argv)

    def parse_wh(s):
        w, _, h = s.partition("x")
        return (int(w), int(h))

    override = [parse_wh(x) for x in a.rungs.split(",") if x.strip()] if a.rungs else None

    # ── Fully offline dry run: schedule arithmetic only, no API, no auth. ────
    if a.dry_run and a.launch:
        launch = parse_wh(a.launch)
        rungs = override or available_rungs(*launch)
        sched = build_schedule(rungs, a.duration, a.profile, a.dwell or None)
        emit({"kind": "session", "id": a.sid or "(dry-run)", "launch": list(launch),
              "rungs": [list(r) for r in rungs], "dry_run": True})
        emit({"kind": "schedule", "steps": sched,
              "total_s": round(sum(s["dwell_s"] for s in sched), 2)})
        print_schedule(launch, rungs, sched, a)
        return 0

    if not a.api:
        note("no --api / QSOAK_API")
        return 2

    token = os.environ.get("QSOAK_TOKEN", "").strip()
    api = Api(a.api, token or None)
    if not token:
        email = os.environ.get("QSOAK_ADMIN_EMAIL", "")
        password = os.environ.get("QSOAK_ADMIN_PASSWORD", "")
        # session_soak.sh always supplies QSOAK_TOKEN from the ONE ladder
        # (scripts/dx/admin_token.sh), so these two tiers only fire for a direct
        # `python3 session_soak_driver.py` invocation on a host. Do not grow them:
        # a new credential route belongs in admin_token.sh, not here.
        dev_key = os.environ.get("QSOAK_DEV_KEY", "").strip()
        err = None
        if email:
            token, err = login(api, email, password)
        else:
            err = "no QSOAK_TOKEN and no QSOAK_ADMIN_EMAIL"
        if not token and dev_key:
            # Fallback for a stack whose BOOTSTRAP_ADMIN password was rotated
            # post-deploy (login 401) but that runs QUASAR_DEV_AGENT_AUTH=1: mint
            # a short-lived admin identity from the per-boot dev key, exactly as
            # qses's agent-creds does. Dev-only route; absent in production.
            note("%s — minting an admin token from the per-boot dev key" % err)
            token, err = dev_mint(api, dev_key)
        if not token:
            note(err or "cannot authenticate")
            note("run: scripts/dx/admin_token.sh --host <host> --fresh (it names every tier it tried)")
            return 1
        api.token = token

    sid = a.sid
    listing = None
    if a.latest or not sid:
        listing, err = latest_running(api)
        if err:
            note(err)
            return 1
        sid = listing["id"]
        note("--latest resolved to %s (%s on %s)" % (
            sid, listing.get("app_name", "?"), listing.get("host_name", "?")))

    sess, err = read_session(api, sid)
    if err:
        note(err)
        return 1
    st = stream_of(sess)
    launch = (st.get("width"), st.get("height"))
    if not launch[0]:
        note("session %s has no stream.width — is it running?" % sid)
        return 1

    supported = st.get("external_resize_supported")
    if supported is False:
        if a.profile == "observe":
            # Observe is PATCH-free by construction (build_schedule/run_step): it
            # only samples telemetry, so an encoder that cannot live-resize is
            # irrelevant. Hard-failing here killed every Vulkan bench-observe cell
            # (0 samples, and bench_run then tore the session down under the still-
            # held peer, which dutifully reported fps=0 / black luma — 2026-08-22).
            note("stream.external_resize_supported=false — irrelevant for --profile "
                 "observe (never PATCHes); continuing, telemetry only")
        else:
            note("stream.external_resize_supported=false — this host's encoder cannot resize a live "
                 "stream (Vulkan). Set QUASAR_ENCODER=nvenc (NVIDIA) or va (AMD/Intel) on the host, "
                 "redeploy, relaunch the session, and re-run the soak.")
            emit({"kind": "error", "msg": "external_resize_supported=false", "sid": sid})
            return EXIT_UNSUPPORTED

    rungs = override
    if not rungs:
        wire = st.get("rungs")
        if wire:
            rungs = [(int(r[0]), int(r[1])) for r in wire]
        else:
            rungs = available_rungs(*launch)
            note("stream.rungs absent — computed the ladder locally (profile.AvailableRungs)")

    sess_rec = {
        "kind": "session",
        "id": sid,
        "state": sess.get("state"),
        "app_id": sess.get("app_id"),
        "app_name": (listing or {}).get("app_name"),
        "host_id": sess.get("host_id"),
        "host_name": (listing or {}).get("host_name"),
        "username": (listing or {}).get("username"),
        "launch": list(launch),
        "fps": st.get("fps"),
        "bitrate_kbps": st.get("bitrate_kbps"),
        "codec": st.get("codec"),
        "external_resize_supported": supported,
        "rungs": [list(r) for r in rungs],
        "profile": a.profile,
        "duration_s": a.duration,
        "started_at_ms": int(time.time() * 1000),
    }
    emit(sess_rec)

    sched = build_schedule(rungs, a.duration, a.profile, a.dwell or None)
    emit({"kind": "schedule", "steps": sched,
          "total_s": round(sum(s["dwell_s"] for s in sched), 2)})
    print_schedule(launch, rungs, sched, a)
    if a.dry_run:
        return 0

    # SIGTERM must land on the same restore-then-report path as Ctrl-C: when the
    # driver runs over ssh it never sees the operator's SIGINT, so the parent
    # kills it instead and the session must still come back to its launch size.
    def _term(_signum, _frame):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, _term)

    stop_evt = threading.Event()
    sampler = Sampler(api, sid, stop_evt, every=a.sample_every)
    sampler.start()

    t_run0 = int(time.time() * 1000)
    interrupted = False
    try:
        for i, ent in enumerate(sched):
            if stop_evt.is_set():
                break
            run_step(api, sid, i + 1, len(sched), ent, a.profile,
                     a.echo_timeout, a.poll_every, stop_evt)
    except KeyboardInterrupt:
        interrupted = True
        note("interrupted — restoring the launch size and writing the report")
    finally:
        # Restore ALWAYS — except observe, which never PATCHed in the first place.
        # The soak must never leave a session parked on a rung it moved to itself.
        if a.profile == "observe":
            note("observe: nothing was PATCHed, nothing to restore")
        else:
            code, _, _, _ = api.call(
                "PATCH", "/v1/sessions/%s/display" % sid,
                {"stream_width": launch[0], "stream_height": launch[1]})
            note("restore -> %dx%d HTTP %s" % (launch[0], launch[1], code))
            emit({"kind": "restore", "target": list(launch), "patch_code": code,
                  "ts_unix_ms": int(time.time() * 1000)})
        time.sleep(min(a.sample_every, 2.0))
        stop_evt.set()
        sampler.join(timeout=15)

    t_run1 = int(time.time() * 1000)
    code, body, _, _ = api.call(
        "GET", "/v1/admin/sessions/%s/trace/window?from=%d&to=%d" % (sid, t_run0 - 5000, t_run1 + 5000),
        timeout=30)
    if code == 200 and isinstance(body, dict):
        emit({"kind": "trace", "data": body})
    else:
        emit({"kind": "info", "msg": "trace/window -> HTTP %s (no trace captured)" % code})

    # ST-09: the control plane's Verdict for the same window. The soak has its own
    # PASS/FAIL — that answers "did the resize ladder behave" — and deliberately
    # does NOT answer "was the stream healthy". This is the one authority on that
    # question, so we record it rather than grow a second opinion here. A control
    # plane that predates the route 404s and the report simply omits the row.
    code, body, _, _ = api.call(
        "GET", "/v1/admin/sessions/%s/verdict?from=%d&to=%d" % (sid, t_run0 - 5000, t_run1 + 5000),
        timeout=30)
    if code == 200 and isinstance(body, dict):
        emit({"kind": "verdict", "data": body})
    else:
        emit({"kind": "info", "msg": "verdict -> HTTP %s (no stream verdict captured)" % code})

    emit({"kind": "summary_hint", "samples": sampler.count, "sampler_errors": sampler.errors,
          "interrupted": interrupted, "t_run0_ms": t_run0, "t_run1_ms": t_run1})
    note("collected %d telemetry samples (%d sampler errors)" % (sampler.count, sampler.errors))
    return 0


def print_schedule(launch, rungs, sched, a):
    note("launch %dx%d   ladder %s" % (
        launch[0], launch[1], " > ".join("%dx%d" % r for r in rungs)))
    note("profile=%s duration=%.0fs steps=%d" % (a.profile, a.duration, len(sched)))
    sys.stderr.write("     %-3s %-11s %-12s %8s %8s\n" % ("#", "target", "label", "dwell", "t0"))
    t = 0.0
    for i, s in enumerate(sched):
        sys.stderr.write("     %-3d %-11s %-12s %7.1fs %7.1fs\n" % (
            i + 1, "%dx%d" % (s["w"], s["h"]), s["label"], s["dwell_s"], t))
        t += s["dwell_s"]
    sys.stderr.write("     %-3s %-11s %-12s %7.1fs\n" % ("", "", "TOTAL", t))
    sys.stderr.flush()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
