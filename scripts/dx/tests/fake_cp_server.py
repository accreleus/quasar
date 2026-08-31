#!/usr/bin/env python3
"""scripts/dx/tests/fake_cp_server.py — a throwaway stand-in for the control plane.

Used only by `scripts/dx/tests/run.sh` to assert how `bench_suite.sh` drives
`/v1/admin/hosts/{id}/settings`, without a stack, a GPU or a network.

It reproduces the ONE semantic that matters here, verbatim from
control-plane/internal/hostcfg/handler.go (`patchReq` + `decide`):

  * the PATCH body is {"overrides": {...}} and is MERGED into the stored map
  * a null value CLEARS that key
  * a body WITHOUT an "overrides" object decodes to an empty map, so it is a
    200 OK that changes absolutely nothing

That last line is the bug this fake exists to catch: a bare `{"abr_enabled":
false}` looks successful and does nothing, which is how a whole benchmark matrix
ran every cell against unchanged host settings while labelling half of them
`abr=off`.

It also serves just enough of `/v1/sessions/{id}` for the `qses stop` tests,
under --session-mode:

  ok      DELETE -> 202, and the follow-up GET reports state=stopped
  error   DELETE -> 500                      (the harness must exit non-zero)
  absent  DELETE -> 404 and GET -> 404       (the cross-stack trap: NOT a success)
  stuck   DELETE -> 202 but GET stays running (accepted != stopped)

    fake_cp_server.py --port 9999 --state /tmp/cp-state.json [--seed '{"abr_mode":"smooth"}']
        [--apps-seed '[{"id":"a1","name":"Quasar Benchapp","profile_policy":"force",
                        "default_profile_id":"1080p60"}]']

--state is rewritten after every mutation so the test can assert the final
stored overrides. Each PATCH body is appended to <state>.patches.jsonl.

It also stands in for the OBSERVABILITY surface `scripts/dx/session.sh` reads and
for the two credential routes `scripts/dx/admin_token.sh` mints against:

  POST /v1/dev/agent-session          200 with an access_token when X-Quasar-Dev-Key
                                      is present, else 401
  POST /v1/auth/login                 200 when the body matches --admin-email/--admin-password
  GET  /v1/admin/sessions             items = --sessions-seed
  GET  /v1/admin/sessions/{id}/metrics
  GET  /v1/admin/sessions/{id}/verdict             the ST-09 Verdict value
  GET  /v1/sessions/{id}/verdict                   the same, owner-scoped
  GET  /v1/admin/sessions/{id}/diagnostic-bundle   classifier = that same Verdict
  GET  /v1/admin/sessions/{id}/trace/window|events
  POST /v1/admin/sessions/{id}/capture             --capture-mode picks the ack outcome
  GET  /v1/admin/sessions/{id}/captures/{cid}      404 for --capture-polls polls, then 200

--expect-token makes every admin read 401 unless the bearer matches, which is how
the "a stale cached token names --fresh as the next step" path is exercised.

--apps-seed serves GET /v1/apps (items = the seeded list verbatim) and
PATCH /v1/apps/{id} (merges profile_policy only — that is all bench_run.sh's
profile_policy=force preflight touches). Each app PATCH body is appended to
<state>.app-patches.jsonl, and the live app map is rewritten to
<state>.apps.json after every PATCH, mirroring the OVERRIDES/patches.jsonl
pair above.
"""

from __future__ import annotations

import argparse
import base64
import gzip
import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer

HOST_ID = "fake-host-1"
OVERRIDES: dict = {}
APPS: dict = {}
STATE_PATH = ""
SESSION_MODE = "ok"
SESSIONS: list = []
VERDICT = "nominal"
# --no-verdict-route makes both /verdict reads 404, standing in for a control
# plane that predates ST-09 — the fallback path the DX layer must survive.
NO_VERDICT_ROUTE = False
EXPECT_TOKEN = ""
# session-capture. CAPTURE_MODE is the ack outcome the fake control plane reports
# for POST .../capture; CAPTURE_POLLS is how many 404s the read serves before the
# result "lands", which is the poll protocol the DX verb has to survive.
CAPTURE_MODE = "ok"
CAPTURE_POLLS = 1
CAPTURES: dict = {}
CAPTURE_POLL_COUNT: dict = {}
ADMIN_EMAIL = "admin@local.test"
ADMIN_PASSWORD = "local-dev-admin"
CAPTURE_RE = re.compile(r"^/v1/admin/sessions/([^/]+)/captures/([^/]+)$")
SESSION_RE = re.compile(r"^/v1/sessions/([^/]+)$")
OWNER_VERDICT_RE = re.compile(r"^/v1/sessions/([^/]+)/verdict$")
APP_RE = re.compile(r"^/v1/apps/([^/]+)$")
ADMIN_SESSION_RE = re.compile(r"^/v1/admin/sessions/([^/]+)((?:/[a-z-]+)*)$")


def bundle_for(sid: str) -> dict:
    now = 1750000000000
    return {
        "trace": {"session_id": sid, "host_id": "h1", "profile_id": "1080p60",
                  "started_at": None, "ended_at": None},
        "window": {"from_ms": now - 300000, "to_ms": now},
        "clock": {"unmeasured": True},
        "abr_mode": "smooth",
        "series": {
            "encoder.fps": [[now - 2000, 60.0], [now - 1000, 59.0]],
            "abr.setpoint_kbps": [[now - 2000, 8000], [now - 1000, 6000]],
        },
        "events": [{"source": "agent", "ts_unix_ms": now - 1500,
                    "type": "abr.downshift", "payload": {"to_kbps": 6000}}],
        "derived_windows": {"hitches": [], "abr_downshifts": [{}],
                            "encoder_saturation": [], "likely_network_congestion": []},
        "agent_adaptation": [],
        "classifier": verdict_for(sid),
        # Only ever present when this control plane dropped client points for an
        # implausible ts_unix_ms (control-api.md amendment 2026-08-23).
        "ingest": {"rejected_ts": 3, "last_rejected_ts_unix_ms": 1750000000,
                   "last_rejected_reason": "looks like seconds, not milliseconds"},
        # session-capture: captures ride inside the bundle regardless of window.
        "captures": [capture_result(cid, kind) for cid, kind in CAPTURES.items()],
    }

def verdict_for(sid: str) -> dict:
    """The ST-09 Verdict value — the body of GET .../verdict and the bundle's
    `classifier`. One shape, two places, exactly as the control plane does it."""
    now = 1750000000000
    return {
        "verdict": VERDICT,
        "evidence": ["fake evidence line"],
        "reason": "%s over a 300 s window (12 host, 11 client samples)." % VERDICT,
        "window": {"from_ms": now - 300000, "to_ms": now, "n_host": 12, "n_client": 11,
                   "warmup_excluded_ms": 20000},
        "clock": {"quality": "measured", "offset_ms": -3.2, "uncertainty_ms": 1.8,
                  "applied": True, "age_ms": 45000},
        "evidence_tier": "host_only",
        "falsifiers": [
            {"name": "encoder.fps", "estimator": "p10", "value": 59.0, "op": ">=",
             "threshold": 50.0, "unit": "fps", "n": 12, "holds": True},
            {"name": "transport.rtt_ms", "estimator": "p95", "value": None, "op": "<=",
             "threshold": 50.0, "unit": "ms", "n": 0, "holds": False, "note": "no samples"},
        ],
        "thresholds_version": "2026-08-23.3",
    }


# A real pipeline_dot capture: gzip+base64 of a tiny graphviz document, so the
# DX verb's decode path (base64 -> gunzip -> .dot on disk) is exercised for real
# rather than mocked around.
DOT_TEXT = b"digraph pipeline {\n  compositor -> encoder;\n  encoder -> webrtcbin;\n}\n"


def capture_result(capture_id: str, kind: str) -> dict:
    if kind == "pipeline_dot":
        blob = gzip.compress(DOT_TEXT)
        return {"capture_id": capture_id, "kind": kind, "ts_unix_ms": 1750000000000,
                "encoding": "gzip+base64", "content_type": "text/vnd.graphviz",
                "data": base64.b64encode(blob).decode(),
                "bytes": len(DOT_TEXT), "compressed_bytes": len(blob),
                "original_bytes": len(DOT_TEXT), "truncated": False, "duration_ms": 12}
    if kind == "encoder_props":
        return {"capture_id": capture_id, "kind": kind, "ts_unix_ms": 1750000000000,
                "encoding": "json", "content_type": "application/json",
                "json": {"encoder_factory": "vulkanh264enc", "codec": "h264",
                         "properties": {"bitrate": 8000}},
                "bytes": 96, "compressed_bytes": 96, "truncated": False, "duration_ms": 3}
    return {"capture_id": capture_id, "kind": kind, "ts_unix_ms": 1750000000000,
            "encoding": "json", "content_type": "application/json",
            "json": {"window_ms": 250, "windows": [{"t_ms": 0, "fps": 60}]},
            "bytes": 64, "compressed_bytes": 64, "truncated": False, "duration_ms": 5000}


# The refusal table, verbatim from control-api.md §On-demand capture. The DX
# layer has to name a DIFFERENT next command for each of these.
CAPTURE_REFUSALS = {
    "busy": (409, "capture_busy"),
    "unknown_kind": (422, "capture_kind_unsupported"),
    "old_agent": (501, "capture_unsupported"),
    "not_connected": (503, "agent_not_connected"),
}


def metrics_for(sid: str) -> dict:
    now = 1750000000000
    return {"items": [
        {"source": "agent", "ts_unix_ms": now - 2000,
         "metrics": {"fps": 60, "encode_ms_p95": 7.2, "bitrate_kbps": 8000,
                     "abr_setpoint_kbps": 8000}},
        {"source": "browser", "ts_unix_ms": now - 1000,
         "metrics": {"fps": 59.4, "present_interval_sd_ms": 4.1, "bitrate_kbps": 7900,
                     # Present cadence (additive 2026-08-23). present_fps_median
                     # is what the metrics table reads; present_beat_fraction is
                     # the BEAT column.
                     "present_fps_median": 60.0, "present_interval_median_ms": 16.67,
                     "present_interval_max_ms": 33.4, "present_beat_fraction": 0.02,
                     "present_long_frames": 0, "present_n": 59}},
    ], "next_cursor": None}


def persist() -> None:
    with open(STATE_PATH, "w") as fh:
        json.dump(OVERRIDES, fh, sort_keys=True)


def log_patch(body: dict) -> None:
    with open(STATE_PATH + ".patches.jsonl", "a") as fh:
        fh.write(json.dumps(body, sort_keys=True) + "\n")


def persist_apps() -> None:
    with open(STATE_PATH + ".apps.json", "w") as fh:
        json.dump(list(APPS.values()), fh, sort_keys=True)


def log_app_patch(app_id: str, body: dict) -> None:
    with open(STATE_PATH + ".app-patches.jsonl", "a") as fh:
        fh.write(json.dumps({"app_id": app_id, **body}, sort_keys=True) + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):  # keep the suite's output clean
        pass

    def _send(self, code: int, payload) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _settings_body(self) -> dict:
        # `effective` is the env<-overrides overlay; the fake has no env, so the
        # overrides ARE the effective map. Values as strings, like the real one.
        return {
            "overrides": dict(OVERRIDES),
            "resolved": dict(OVERRIDES),
            "effective": {k: str(v).lower() if isinstance(v, bool) else str(v)
                          for k, v in OVERRIDES.items()},
            "codecs": [],
            "pending_restart": False,
        }

    def _bearer(self) -> str:
        h = self.headers.get("Authorization") or ""
        return h[7:].strip() if h.lower().startswith("bearer ") else ""

    def _token_ok(self) -> bool:
        if not EXPECT_TOKEN:
            return True
        return self._bearer() == EXPECT_TOKEN

    def do_GET(self):  # noqa: N802
        path = self.path.split("?")[0]
        if path == "/health":
            # admin_token.sh preflights this before any credential tier.
            return self._send(200, {"status": "ok", "db": "ok"})
        if path.startswith("/v1/admin/") and not self._token_ok():
            return self._send(401, {"error": {"code": "unauthorized"}})
        if path == "/v1/admin/sessions":
            return self._send(200, {"items": list(SESSIONS), "next_cursor": None})
        m = CAPTURE_RE.match(path)
        if m:
            sid, cid = m.group(1), m.group(2)
            rec = CAPTURES.get(cid)
            if rec is None:
                return self._send(404, {"error": {"code": "not_found"}})
            # 404 until it "lands": the poll signal, not an error.
            n = CAPTURE_POLL_COUNT.get(cid, 0)
            CAPTURE_POLL_COUNT[cid] = n + 1
            if n < CAPTURE_POLLS:
                return self._send(404, {"error": {"code": "not_found"}})
            return self._send(200, capture_result(cid, rec))
        m = ADMIN_SESSION_RE.match(path)
        if m:
            sid, tail = m.group(1), (m.group(2) or "")
            known = {s.get("id") for s in SESSIONS}
            if known and sid not in known:
                return self._send(404, {"error": {"code": "not_found"}})
            if tail == "/verdict":
                if NO_VERDICT_ROUTE:
                    # An older control plane that predates ST-09. The DX layer
                    # must fall back to the bundle rather than fail.
                    return self._send(404, {"error": {"code": "not_found"}})
                return self._send(200, verdict_for(sid))
            if tail == "/diagnostic-bundle":
                return self._send(200, bundle_for(sid))
            if tail == "/metrics":
                return self._send(200, metrics_for(sid))
            if tail in ("/trace", "/trace/window"):
                b = bundle_for(sid)
                return self._send(200, {"session_id": sid, "window": b["window"],
                                        "clock": b["clock"], "series": b["series"],
                                        "events": b["events"]})
            if tail == "/trace/events":
                b = bundle_for(sid)
                return self._send(200, {"window": b["window"], "events": b["events"]})
            if not tail:
                for s0 in SESSIONS:
                    if s0.get("id") == sid:
                        return self._send(200, s0)
                return self._send(404, {"error": {"code": "not_found"}})
        if path == "/v1/hosts":
            return self._send(200, {"items": [{"id": HOST_ID, "node_name": "fake"}]})
        if path == "/v1/admin/sessions":
            return self._send(200, {"items": []})
        if path == "/v1/apps":
            return self._send(200, {"items": list(APPS.values())})
        if re.match(r"^/v1/admin/hosts/[^/]+/settings$", path):
            return self._send(200, self._settings_body())
        m = OWNER_VERDICT_RE.match(path)
        if m:
            if NO_VERDICT_ROUTE:
                return self._send(404, {"error": {"code": "not_found"}})
            return self._send(200, verdict_for(m.group(1)))
        m = SESSION_RE.match(path)
        if m:
            if SESSION_MODE == "absent":
                return self._send(404, {"error": {"code": "not_found"}})
            state = "running" if SESSION_MODE == "stuck" else "stopped"
            return self._send(200, {"session": {"id": m.group(1), "state": state}})
        return self._send(404, {"error": "not found"})

    def do_POST(self):  # noqa: N802
        path = self.path.split("?")[0]
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except ValueError:
            return self._send(400, {"error": "invalid body"})
        if path == "/v1/dev/agent-session":
            if not (self.headers.get("X-Quasar-Dev-Key") or "").strip():
                return self._send(401, {"error": {"code": "unauthorized"}})
            return self._send(200, {"access_token": "devkey-minted-token",
                                    "storage_keys": {"quasar.access_token": "devkey-minted-token"}})
        m = re.match(r"^/v1/admin/sessions/([^/]+)/capture$", path)
        if m:
            if not self._token_ok():
                return self._send(401, {"error": {"code": "unauthorized"}})
            if CAPTURE_MODE in CAPTURE_REFUSALS:
                code, err = CAPTURE_REFUSALS[CAPTURE_MODE]
                return self._send(code, {"error": {"code": err, "message": err}})
            kind = body.get("kind") or "pipeline_dot"
            cid = "cap-%d" % (len(CAPTURES) + 1)
            CAPTURES[cid] = kind
            if CAPTURE_MODE == "never":
                # Armed, then nothing ever reports: the timeout path.
                CAPTURE_POLL_COUNT[cid] = -10**9
            return self._send(202, {"capture_id": cid, "kind": kind,
                                    "session_id": m.group(1),
                                    "accepted_at": "2026-08-23T12:00:00Z"})
        if path == "/v1/auth/login":
            if (body.get("email") == ADMIN_EMAIL
                    and body.get("password") == ADMIN_PASSWORD):
                return self._send(200, {"access_token": "bootstrap-minted-token"})
            return self._send(401, {"error": {"code": "unauthorized"}})
        return self._send(404, {"error": "not found"})

    def do_DELETE(self):  # noqa: N802
        path = self.path.split("?")[0]
        if not SESSION_RE.match(path):
            return self._send(404, {"error": "not found"})
        if SESSION_MODE == "error":
            return self._send(500, {"error": {"code": "internal"}})
        if SESSION_MODE == "absent":
            return self._send(404, {"error": {"code": "not_found"}})
        return self._send(202, {"status": "stopping"})

    def do_PATCH(self):  # noqa: N802
        path = self.path.split("?")[0]
        m = APP_RE.match(path)
        if m:
            app_id = m.group(1)
            if app_id not in APPS:
                return self._send(404, {"error": {"code": "not_found"}})
            n = int(self.headers.get("Content-Length") or 0)
            try:
                body = json.loads(self.rfile.read(n) or b"{}")
            except ValueError:
                return self._send(400, {"error": "invalid body"})
            log_app_patch(app_id, body)
            if "profile_policy" in body:
                APPS[app_id]["profile_policy"] = body["profile_policy"]
            persist_apps()
            return self._send(200, APPS[app_id])
        if not re.match(r"^/v1/admin/hosts/[^/]+/settings$", path):
            return self._send(404, {"error": "not found"})
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except ValueError:
            return self._send(400, {"error": "invalid body"})
        log_patch(body)
        # The real handler decodes into a struct with ONE json:"overrides" field.
        # Anything else in the body is simply not read.
        req = body.get("overrides")
        if isinstance(req, dict):
            for k, v in req.items():
                if v is None:
                    OVERRIDES.pop(k, None)
                else:
                    OVERRIDES[k] = v
        persist()
        return self._send(200, self._settings_body())


def main() -> int:
    global STATE_PATH, SESSION_MODE, SESSIONS, VERDICT, EXPECT_TOKEN, NO_VERDICT_ROUTE
    global ADMIN_EMAIL, ADMIN_PASSWORD, CAPTURE_MODE, CAPTURE_POLLS
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--state", required=True)
    p.add_argument("--seed", default="{}")
    p.add_argument("--apps-seed", default="[]")
    p.add_argument("--session-mode", default="ok",
                   choices=["ok", "error", "absent", "stuck"])
    p.add_argument("--sessions-seed", default="[]",
                   help="GET /v1/admin/sessions items, verbatim")
    p.add_argument("--verdict", default="nominal",
                   help="classifier.verdict in the diagnostic bundle")
    p.add_argument("--no-verdict-route", action="store_true",
                   help="404 both /verdict reads (a pre-ST-09 control plane)")
    p.add_argument("--expect-token", default="",
                   help="401 every /v1/admin read whose bearer differs")
    p.add_argument("--capture-mode", default="ok",
                   choices=["ok", "busy", "unknown_kind", "old_agent",
                            "not_connected", "never"],
                   help="the ack outcome POST .../capture reports ('never' = armed, never reports)")
    p.add_argument("--capture-polls", type=int, default=1,
                   help="how many 404s GET .../captures/{id} serves before the result lands")
    p.add_argument("--admin-email", default="admin@local.test")
    p.add_argument("--admin-password", default="local-dev-admin")
    args = p.parse_args()
    STATE_PATH = args.state
    SESSION_MODE = args.session_mode
    SESSIONS = json.loads(args.sessions_seed)
    VERDICT = args.verdict
    NO_VERDICT_ROUTE = args.no_verdict_route
    EXPECT_TOKEN = args.expect_token
    CAPTURE_MODE = args.capture_mode
    CAPTURE_POLLS = args.capture_polls
    ADMIN_EMAIL = args.admin_email
    ADMIN_PASSWORD = args.admin_password
    OVERRIDES.update(json.loads(args.seed))
    for app in json.loads(args.apps_seed):
        APPS[app["id"]] = app
    persist()
    persist_apps()
    open(STATE_PATH + ".patches.jsonl", "w").close()
    open(STATE_PATH + ".app-patches.jsonl", "w").close()
    HTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
