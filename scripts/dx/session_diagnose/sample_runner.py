#!/usr/bin/env python3
"""
session_diagnose/sample_runner.py — the remote soak poller that runs ON the host
via SSH.

Called by the quasar-diagnose skill's qdiag-sample as:
  python3 /dev/stdin <session_target> <host> <base-url> <interval_s> <duration_s>
with $QDIAG_TOKEN carrying an admin bearer in the remote environment.

Polls GET /v1/admin/sessions/{id}/diagnostic-bundle every interval_s seconds for
duration_s, appends each snapshot, then prints the run JSON to stdout.

It mints NOTHING and reads no deploy/.env: the bearer comes from the ONE ladder,
scripts/dx/admin_token.sh, resolved by the caller. The previous version guessed
the repo path (~/code/quasar, /mnt/user/appdata/quasar) and was wrong on devbox.
"""

import json
import math
import os
import sys
import time
import urllib.error
import urllib.request
import ssl
# Control-plane is HTTPS-only since 2026-07-20 (self-signed on localhost);
# plain HTTP now 308-redirects, which urllib turns into a login failure.
_SSL_CTX = ssl._create_unverified_context()

args = sys.argv[1:]
SESSION_TARGET = args[0] if args else "latest"
HOST           = args[1] if len(args) > 1 else "unknown-host"
BASE           = (args[2] if len(args) > 2 else "https://localhost:8443").rstrip("/")
INTERVAL_S     = float(args[3]) if len(args) > 3 else 5.0
DURATION_S     = float(args[4]) if len(args) > 4 else 120.0

TOKEN = os.environ.get("QDIAG_TOKEN", "").strip()
if not TOKEN:
    print("FATAL: $QDIAG_TOKEN is empty. The caller resolves it with "
          "scripts/dx/admin_token.sh --host <host>; this script never mints one.",
          file=sys.stderr)
    sys.exit(2)


def http_post(path, body, tok=None):
    data = json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method="POST")
    req.add_header("content-type", "application/json")
    if tok:
        req.add_header("authorization", "Bearer " + tok)
    return json.load(urllib.request.urlopen(req, timeout=15, context=_SSL_CTX))


def http_get(path, tok):
    req = urllib.request.Request(BASE + path)
    req.add_header("authorization", "Bearer " + tok)
    return json.load(urllib.request.urlopen(req, timeout=15, context=_SSL_CTX))


def resolve_session(tok: str) -> tuple[str, dict]:
    if SESSION_TARGET not in ("latest", "running"):
        try:
            sess = http_get(f"/v1/admin/sessions/{SESSION_TARGET}", tok)
            return SESSION_TARGET, sess
        except Exception:
            pass
    sl = http_get("/v1/admin/sessions?limit=50", tok)
    items = sl.get("items") or sl.get("sessions") or (sl if isinstance(sl, list) else [])
    running = [s for s in items if (s.get("state") or s.get("status") or "").lower() == "running"]
    pick = running or items
    if not pick:
        print("No sessions found", file=sys.stderr)
        sys.exit(0)
    s = pick[0]
    sid = s.get("id") or s.get("session_id")
    return sid, s


def main():
    tok = TOKEN
    sid, sess_meta = resolve_session(tok)

    app     = sess_meta.get("app_name") or sess_meta.get("app", "?")
    profile = sess_meta.get("profile_id") or sess_meta.get("profile", "?")

    print(f"# soak: session={sid} app={app} host={HOST} "
          f"interval={INTERVAL_S}s duration={DURATION_S}s",
          file=sys.stderr)

    samples = []
    start = time.time()
    deadline = start + DURATION_S
    final_bundle = None

    while True:
        now = time.time()
        if now > deadline and len(samples) > 0:
            break
        try:
            bundle = http_get(f"/v1/admin/sessions/{sid}/diagnostic-bundle", tok)
            ts = time.time()
            samples.append({"ts_unix_s": ts, "bundle": bundle})
            final_bundle = bundle
            verdict = (bundle.get("classifier") or {}).get("verdict", "?")
            n_pts = sum(len(v) for v in (bundle.get("series") or {}).values())
            print(f"# [{len(samples):3d}] t={ts-start:6.1f}s  verdict={verdict}  series_points={n_pts}",
                  file=sys.stderr)
        except Exception as e:
            print(f"# poll error: {e}", file=sys.stderr)

        elapsed = time.time() - start
        if elapsed >= DURATION_S:
            break
        sleep_until = start + len(samples) * INTERVAL_S
        wait = sleep_until - time.time()
        if wait > 0:
            time.sleep(wait)

    run = {
        "meta": {
            "host":       HOST,
            "app":        app,
            "profile":    profile,
            "session":    sid,
            "started":    start,
            "duration_s": DURATION_S,
            "n_samples":  len(samples),
            "interval_s": INTERVAL_S,
        },
        "samples": samples,
        "final_bundle": final_bundle,
    }
    print(json.dumps(run, default=str))


if __name__ == "__main__":
    main()
