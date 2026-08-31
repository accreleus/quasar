#!/usr/bin/env python3
"""scripts/dx/tests/fake_bench_server.py — a throwaway stand-in for quasar-bench.

Used only by `scripts/dx/tests/run.sh` so the bench_submit.py ingest path can be
asserted end to end without a real service, a network, or a key. It speaks just
enough of the /v1 contract for the client, and appends one JSON line per request
to --log so the test can assert on what the SERVER received rather than on what
the client claimed to send.

    fake_bench_server.py --port 9999 --log /tmp/bench.log

Log line: {"path": ..., "body": {...}}  (artifacts also carry "name").

What it models of the 1.1 contract, because bench_submit.py now depends on it:
  * `external_id` is an UPSERT key — the second POST /v1/runs with the same id
    returns 200 and the SAME run id, not 201 and a second run.
  * `conditions` merges on create/finish/patch, and every `conditions.effective`
    key that disagrees with the same-named tag comes back as a `mismatches`
    entry on the run DETAIL (GET /v1/runs/{id}) — the same place the real
    service puts it, which is why the client has to read it back.
  * `PATCH /v1/runs/{id}` takes validity / validity_reason / tags.
  * `GET /v1/runs/{id}/phases` derives windows from the posted marker events.
  * `GET /v1/stats` accepts (and echoes) `window=`.
"""

from __future__ import annotations

import argparse
import gzip
import json
import re
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

RUNS: dict = {}
EVENTS: dict = {}
SAMPLES: dict = {}
BY_EXT: dict = {}
BASELINES: dict = {}
LOG_PATH = ""
# Canned GET /v1/regressions responses, loaded from --regressions-file for
# scripts/dx/bench_budget.py's fake-server test: {metric: {agg: {"better":
# ..., "runs": [{"run_id":..., "value":..., "baseline_value":...,
# "delta_pct":..., "regressed":..., "threshold_pct":...}, ...]}}}. Real
# quasar-bench computes this from samples + the pinned baseline; the fake
# server has no aggregation engine, so the test supplies the numbers directly
# and asserts the CLIENT renders and gates on them correctly.
REGRESSIONS_FIXTURE: dict = {}


def log(rec: dict) -> None:
    with open(LOG_PATH, "a") as fh:
        fh.write(json.dumps(rec) + "\n")


def scalar(v) -> str:
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float, str)):
        return str(v)
    return ""


def mismatches(run: dict) -> list:
    eff = ((run.get("conditions") or {}).get("effective")) or {}
    out = []
    for k in sorted(run.get("tags") or {}):
        if k not in eff:
            continue
        e = scalar(eff[k])
        if e and e != run["tags"][k]:
            out.append({"key": k, "intended": run["tags"][k], "effective": e})
    return out


def phases(rid: str) -> list:
    """baseline/impaired/recovery from netem events + explicit harness marks."""
    evs = sorted(EVENTS.get(rid, []), key=lambda e: e["ts_unix_ms"])
    ts = [s["ts_unix_ms"] for s in SAMPLES.get(rid, [])]
    if not ts:
        return []
    lo, hi = min(ts), max(ts) + 1
    out = []
    impairs = [e["ts_unix_ms"] for e in evs if e["type"] == "netem.impair"]
    clears = [e["ts_unix_ms"] for e in evs if e["type"] == "netem.clear"]
    for t in impairs:
        nxt = [c for c in clears if c > t]
        out.append({"phase": "impaired", "from_unix_ms": t,
                    "to_unix_ms": nxt[0] if nxt else hi})
    out.append({"phase": "baseline", "from_unix_ms": lo,
                "to_unix_ms": impairs[0] if impairs else hi})
    if clears:
        out.append({"phase": "recovery", "from_unix_ms": max(clears), "to_unix_ms": hi})
    for e in evs:
        p = (e.get("payload") or {})
        if e["type"] != "harness.mark" or p.get("edge") != "start" or not p.get("phase"):
            continue
        ends = [x["ts_unix_ms"] for x in evs
                if x["type"] == "harness.mark"
                and (x.get("payload") or {}).get("phase") == p["phase"]
                and (x.get("payload") or {}).get("edge") == "end"
                and x["ts_unix_ms"] > e["ts_unix_ms"]]
        out.append({"phase": p["phase"], "from_unix_ms": e["ts_unix_ms"],
                    "to_unix_ms": ends[0] if ends else hi})
    return [w for w in sorted(out, key=lambda w: (w["from_unix_ms"], w["phase"]))
            if w["to_unix_ms"] > w["from_unix_ms"]]


def merge(dst: dict, src: dict) -> dict:
    out = dict(dst or {})
    for k, v in (src or {}).items():
        if isinstance(v, dict) and isinstance(out.get(k), dict):
            out[k] = merge(out[k], v)
        else:
            out[k] = v
    return out


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

    def _read(self) -> bytes:
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if self.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        return raw

    def do_GET(self):  # noqa: N802
        path = self.path.split("?")[0]
        query = urllib.parse.parse_qs(self.path.split("?", 1)[1] if "?" in self.path else "")
        if path == "/v1/health":
            return self._send(200, {"status": "ok", "service": "fake"})
        if path == "/openapi.yaml":
            body = b'openapi: 3.1.0\ninfo:\n  title: fake\n  version: "1.1.0"\n'
            self.send_response(200)
            self.send_header("Content-Type", "text/yaml")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            return self.wfile.write(body)
        if path == "/v1/runs":
            ext = (query.get("external_id") or [""])[0]
            hits = [{"id": rid} for rid, r in RUNS.items()
                    if not ext or r.get("external_id") == ext]
            return self._send(200, {"runs": hits, "next_cursor": ""})
        m = re.match(r"^/v1/runs/([^/]+)(?:/(phases))?$", path)
        if m and m.group(1) in RUNS:
            rid = m.group(1)
            if m.group(2) == "phases":
                return self._send(200, {"phases": phases(rid)})
            run = dict(RUNS[rid], id=rid)
            run["mismatches"] = mismatches(RUNS[rid])
            run["phases"] = phases(rid)
            return self._send(200, run)
        if path == "/v1/stats":
            return self._send(200, {"metric": (query.get("metric") or [""])[0],
                                    "window": (query.get("window") or [""])[0],
                                    "rows": []})
        if path == "/v1/regressions":
            metric = (query.get("metric") or [""])[0]
            agg = (query.get("agg") or ["p50"])[0]
            fixture = (REGRESSIONS_FIXTURE.get(metric) or {}).get(agg) or {"runs": []}
            return self._send(200, {
                "metric": metric, "agg": agg,
                "window": (query.get("window") or [""])[0],
                "baseline_run_id": fixture.get("baseline_run_id", ""),
                "better": fixture.get("better", "neutral"),
                "runs": fixture.get("runs", []),
            })
        return self._send(404, {"error": "not found"})

    def do_PATCH(self):  # noqa: N802
        path = self.path.split("?")[0]
        raw = self._read()
        m = re.match(r"^/v1/runs/([^/]+)$", path)
        if not m or m.group(1) not in RUNS:
            return self._send(404, {"error": "not found"})
        rid = m.group(1)
        body = json.loads(raw or b"{}")
        log({"path": path, "method": "PATCH", "body": body})
        run = RUNS[rid]
        for k in ("validity", "validity_reason", "verdict", "notes"):
            if k in body:
                run[k] = body[k]
        run["tags"].update(body.get("tags") or {})
        if body.get("conditions") is not None:
            run["conditions"] = merge(run.get("conditions") or {}, body["conditions"])
        return self._send(200, dict(run, id=rid, mismatches=mismatches(run)))

    def do_PUT(self):  # noqa: N802
        raw = self._read()
        path = self.path.split("?")[0]
        if path != "/v1/baselines":
            return self._send(404, {"error": "not found"})
        body = json.loads(raw or b"{}")
        log({"path": path, "method": "PUT", "body": body})
        BASELINES[(body.get("suite"), body.get("scenario"))] = body.get("run_id")
        return self._send(200, {"ok": True})

    def do_POST(self):  # noqa: N802
        path = self.path.split("?")[0]
        raw = self._read()

        if path == "/v1/baselines":
            body = json.loads(raw)
            log({"path": path, "body": body})
            BASELINES[(body.get("suite"), body.get("scenario"))] = body.get("run_id")
            return self._send(200, {"ok": True})

        if path == "/v1/runs":
            body = json.loads(raw)
            log({"path": path, "body": body})
            ext = body.get("external_id") or ""
            if ext and ext in BY_EXT:
                # UPSERT: the same external_id updates the run it already made.
                rid = BY_EXT[ext]
                run = RUNS[rid]
                run["tags"].update(body.get("tags") or {})
                run["conditions"] = merge(run.get("conditions") or {},
                                          body.get("conditions") or {})
                return self._send(200, dict(run, id=rid))
            rid = "fake-run-%d" % (len(RUNS) + 1)
            RUNS[rid] = {"tags": body.get("tags") or {},
                         "conditions": body.get("conditions") or {},
                         "external_id": ext, "validity": "valid",
                         "suite": body.get("suite"), "scenario": body.get("scenario")}
            if ext:
                BY_EXT[ext] = rid
            return self._send(201, {"id": rid})

        m = re.match(r"^/v1/runs/([^/]+)/(samples|events|artifacts|finish)$", path)
        if not m:
            return self._send(404, {"error": "not found"})
        rid, kind = m.group(1), m.group(2)

        if kind == "artifacts":
            # Just enough multipart parsing to recover the `name` field.
            name = ""
            mm = re.search(rb'name="name"\r\n\r\n([^\r]*)\r\n', raw)
            if mm:
                name = mm.group(1).decode()
            log({"path": path, "name": name, "body": {}})
            return self._send(201, {"name": name, "size": len(raw)})

        body = json.loads(raw)
        log({"path": path, "body": body})
        if kind == "samples":
            SAMPLES.setdefault(rid, []).extend(body.get("samples") or [])
            return self._send(200, {"written": len(body.get("samples") or [])})
        if kind == "events":
            EVENTS.setdefault(rid, []).extend(body.get("events") or [])
            return self._send(200, {"written": len(body.get("events") or [])})
        run = RUNS.setdefault(rid, {"tags": {}, "conditions": {}})
        run["tags"].update(body.get("tags") or {})
        if body.get("conditions") is not None:
            run["conditions"] = merge(run.get("conditions") or {}, body["conditions"])
        # Like the real service, the finish response is a Run — no `mismatches`.
        # The client has to go and read the detail, and the test asserts it does.
        return self._send(200, {"id": rid, "status": body.get("status")})


def main() -> int:
    global LOG_PATH, REGRESSIONS_FIXTURE
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--log", required=True)
    p.add_argument("--regressions-file", default="",
                   help="JSON file of canned GET /v1/regressions responses "
                        "(see REGRESSIONS_FIXTURE docstring above)")
    args = p.parse_args()
    LOG_PATH = args.log
    open(LOG_PATH, "w").close()
    if args.regressions_file:
        with open(args.regressions_file) as fh:
            REGRESSIONS_FIXTURE = json.load(fh)
    HTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
