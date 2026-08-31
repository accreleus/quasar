# VENDORED FILE — DO NOT EDIT HERE.
#
# Source: quasar-bench client/bench.py
# Commit: e0e81952bdee7f6206fb3b5cf7448f06c64b3607 (2026-08-24)  __version__ 1.3.0
# Repo:   git@github.com:salty2011/quasar-bench.git
#
# Re-vendor with:
#   cp ../quasar-bench/client/bench.py scripts/dx/vendor/bench.py
# then re-add this header and update the commit line above.
#
# bench_submit.py compares this file's __version__ against the SERVER's
# openapi info.version on every submission and warns when they drift.


#!/usr/bin/env python3
"""quasar-bench client — vendor this file into a harness.

Stdlib only (urllib), no dependencies.

Library use:

    from bench import Bench
    b = Bench()                       # reads BENCH_URL / BENCH_KEY
    run = b.new_run("abr-ladder", "1080p120-h264-netem-moderate",
                    host="devbox", tags={"abr_mode": "smooth", "codec": "h264"})
    b.samples(run, [{"ts_unix_ms": ..., "source": "agent",
                     "metrics": {"fps": 118.4, "bitrate_kbps": 7840}}])
    b.events(run, [{"ts_unix_ms": ..., "type": "abr.ladder.step",
                    "payload": {"to_height": 720}}])
    b.artifact(run, "REPORT.md")
    b.finish(run, status="finished", verdict="PASS", summary={"fps_mean": 118.2})

Publishing a completion report (the durable home for a write-up plus its
evidence; identity is repo + commit, so the URL survives):

    url = b.report_put("accreleus/quasar", sha, "C11: reports and evidence",
                       body_path="REPORT.md", issues=[512],
                       runs=[before_run, after_run])["url"]
    b.report_attach("accreleus/quasar", sha, "after.png",
                    role="screenshot", caption="the ladder at 720p")
    # paste `url` into the commit body, the issue and memory

The same flow from a shell is the `qbench` CLI next to this file.

CLI use:

    export BENCH_URL=http://bench.example.internal:9400 BENCH_KEY=...
    bench.py new  --suite abr-ladder --scenario 1080p120 --tag abr_mode=smooth
    bench.py samples  <run-id> --file samples.jsonl
    bench.py events   <run-id> --file events.jsonl
    bench.py artifact <run-id> REPORT.md
    bench.py finish   <run-id> --verdict PASS --summary summary.json
    bench.py stats --metric browser.fps --group-by tag.abr_mode --suite abr-ladder

Re-folding a run (this is the safe form — a plain re-post leaves a stale tail):

    bench.py samples <run-id> --file metrics.jsonl --replace --expect browser=276
"""

from __future__ import annotations

import argparse
import gzip
import json
import mimetypes
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Iterable

# Bump on every change to this file so a vendoring harness can detect drift:
#   python3 -c "import bench; print(bench.__version__)"
__version__ = "1.3.0"

DEFAULT_URL = "http://localhost:9400"
GZIP_THRESHOLD = 64 * 1024
CHUNK = 5000  # samples per request; the server handles 20k, this keeps retries cheap


class BenchError(RuntimeError):
    pass


class CountMismatch(BenchError):
    """A sample write was rejected because a source ended up the wrong length."""

    def __init__(self, message: str, source: str = "", expected: int = 0, actual: int = 0):
        super().__init__(message)
        self.source, self.expected, self.actual = source, expected, actual


class Bench:
    def __init__(self, url: str | None = None, key: str | None = None, timeout: int = 120):
        self.url = (url or os.environ.get("BENCH_URL") or DEFAULT_URL).rstrip("/")
        self.key = key or os.environ.get("BENCH_KEY") or ""
        self.timeout = timeout
        if not self.key:
            raise BenchError("no API key: set BENCH_KEY or pass key=")

    # ---------------------------------------------------------------- http

    def _request(self, method: str, path: str, *, body: bytes | None = None,
                 headers: dict[str, str] | None = None) -> Any:
        req = urllib.request.Request(self.url + path, data=body, method=method)
        req.add_header("Authorization", f"Bearer {self.key}")
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as res:
                raw = res.read()
        except urllib.error.HTTPError as e:
            raw_err = e.read()
            detail = raw_err.decode("utf-8", "replace")[:400]
            if e.code == 409:
                try:
                    body = json.loads(raw_err)
                except json.JSONDecodeError:
                    body = {}
                if "expected" in body:
                    raise CountMismatch(body.get("error", detail), body.get("source", ""),
                                        int(body.get("expected", 0)),
                                        int(body.get("actual", 0))) from None
            raise BenchError(f"{method} {path} -> {e.code}: {detail}") from None
        except urllib.error.URLError as e:
            raise BenchError(f"{method} {path} -> {e.reason}") from None
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            return raw

    def _post(self, path: str, payload: dict) -> Any:
        body = json.dumps(payload).encode()
        headers = {"Content-Type": "application/json"}
        if len(body) > GZIP_THRESHOLD:
            body = gzip.compress(body, 6)
            headers["Content-Encoding"] = "gzip"
        return self._request("POST", path, body=body, headers=headers)

    def _get(self, path: str, params: dict | None = None) -> Any:
        if params:
            clean = {k: v for k, v in params.items() if v not in (None, "")}
            path += "?" + urllib.parse.urlencode(clean)
        return self._request("GET", path)

    # ---------------------------------------------------------------- write

    def new_run(self, suite: str, scenario: str, host: str = "",
                tags: dict[str, str] | None = None, notes: str = "",
                conditions: dict | None = None, external_id: str | None = None) -> str:
        """Create (or upsert, with external_id) a run and return its id.

        conditions is what the host was ACTUALLY doing — effective settings,
        negotiated encoder/codec, concurrent sessions, netem state, git shas.
        Put the intended configuration in tags; the service flags any key where
        tags and conditions["effective"] disagree.
        """
        body = {
            "suite": suite, "scenario": scenario, "host": host,
            "tags": {k: str(v) for k, v in (tags or {}).items()}, "notes": notes,
        }
        if conditions is not None:
            body["conditions"] = conditions
        if external_id:
            body["external_id"] = external_id
        return self._post("/v1/runs", body)["id"]

    def samples(self, run_id: str, samples: Iterable[dict], chunk: int = CHUNK,
                replace: bool = False, expected_count: dict[str, int] | None = None) -> int:
        """Post samples in chunks. Idempotent on (run, source, ts_unix_ms).

        replace=True clears the existing series for every source in the payload
        before inserting. Use it for any re-fold: upserting alone leaves a stale
        tail behind when the new fold lands on different timestamps.

        expected_count={"browser": 276} is checked per source after the write,
        inside the transaction — a mismatch raises CountMismatch and nothing is
        written. Only the FINAL chunk carries the guard, since earlier chunks
        are legitimately short.
        """
        batches: list[list[dict]] = []
        buf: list[dict] = []
        for s in samples:
            buf.append(s)
            if len(buf) >= chunk:
                batches.append(buf)
                buf = []
        if buf or not batches:
            batches.append(buf)

        written = 0
        for i, batch in enumerate(batches):
            body: dict[str, Any] = {"samples": batch}
            # Replace on the first chunk only, or later chunks would wipe the
            # rows the earlier ones just wrote.
            if replace and i == 0:
                body["replace"] = True
            if expected_count and i == len(batches) - 1:
                body["expected_count"] = expected_count
            res = self._post(f"/v1/runs/{run_id}/samples", body)
            written += res["written"]
        return written

    def delete_samples(self, run_id: str, source: str = "",
                       from_ms: int | None = None, to_ms: int | None = None) -> dict:
        """Delete samples, optionally narrowed to a source and a time range."""
        params = {"source": source}
        if from_ms is not None:
            params["from"] = from_ms
        if to_ms is not None:
            params["to"] = to_ms
        qs = urllib.parse.urlencode({k: v for k, v in params.items() if v not in (None, "")})
        return self._request("DELETE", f"/v1/runs/{run_id}/samples" + (f"?{qs}" if qs else ""))

    def counts(self, run_id: str) -> dict[str, int]:
        """Samples per source — what a fold checks itself against."""
        return self._get(f"/v1/runs/{run_id}/counts")["counts"]

    def rename_metric_key(self, run_id: str, from_key: str, to_key: str,
                          source: str = "", overwrite: bool = False) -> dict:
        """Rename a metric key inside a run's samples, server-side."""
        qs = urllib.parse.urlencode({k: v for k, v in {
            "from": from_key, "to": to_key, "source": source,
            "overwrite": "1" if overwrite else "",
        }.items() if v})
        return self._request("PATCH", f"/v1/runs/{run_id}/samples/rename?{qs}")

    def register_metric(self, key: str, better: str, unit: str = "",
                        regression_pct: float = 0) -> dict:
        """Declare a metric's direction so deltas colour and regress correctly.

        better is "higher", "lower" or "neutral".
        """
        return self._post("/v1/metrics", {"key": key, "better": better,
                                          "unit": unit, "regression_pct": regression_pct})

    def events(self, run_id: str, events: Iterable[dict]) -> int:
        evs = list(events)
        if not evs:
            return 0
        return self._post(f"/v1/runs/{run_id}/events", {"events": evs})["written"]

    def mark(self, run_id: str, type_: str, payload: dict | None = None, ts_ms: int | None = None) -> None:
        self.events(run_id, [{
            "ts_unix_ms": ts_ms if ts_ms is not None else now_ms(),
            "type": type_, "payload": payload or {},
        }])

    def _upload(self, path: str, file_path: str, fields: dict[str, str]) -> dict:
        """POST one file as multipart/form-data, with extra text fields."""
        name = fields.get("name") or os.path.basename(file_path)
        mime = fields.get("mime") or mimetypes.guess_type(name)[0] or "application/octet-stream"
        fields = dict(fields, name=name, mime=mime)
        with open(file_path, "rb") as fh:
            data = fh.read()
        boundary = uuid.uuid4().hex
        parts: list[bytes] = []
        for field, value in fields.items():
            if not value:
                continue
            parts += [f"--{boundary}\r\n".encode(),
                      f'Content-Disposition: form-data; name="{field}"\r\n\r\n'.encode(),
                      str(value).encode(), b"\r\n"]
        parts += [f"--{boundary}\r\n".encode(),
                  f'Content-Disposition: form-data; name="file"; filename="{name}"\r\n'.encode(),
                  f"Content-Type: {mime}\r\n\r\n".encode(), data, b"\r\n",
                  f"--{boundary}--\r\n".encode()]
        return self._request("POST", path, body=b"".join(parts),
                             headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})

    def artifact(self, run_id: str, path: str, name: str | None = None, mime: str | None = None) -> dict:
        return self._upload(f"/v1/runs/{run_id}/artifacts", path,
                            {"name": name or "", "mime": mime or ""})

    def finish(self, run_id: str, status: str = "finished", verdict: str | None = None,
               summary: dict | None = None, tags: dict[str, str] | None = None,
               conditions: dict | None = None) -> dict:
        payload: dict[str, Any] = {"status": status}
        if verdict:
            payload["verdict"] = verdict
        if summary is not None:
            payload["summary"] = summary
        if tags:
            payload["tags"] = {k: str(v) for k, v in tags.items()}
        if conditions is not None:
            payload["conditions"] = conditions
        return self._post(f"/v1/runs/{run_id}/finish", payload)

    def patch(self, run_id: str, **fields) -> dict:
        """Annotate a run: validity, validity_reason, verdict, notes, tags, conditions."""
        return self._request("PATCH", f"/v1/runs/{run_id}",
                             body=json.dumps(fields).encode(),
                             headers={"Content-Type": "application/json"})

    def set_validity(self, run_id: str, validity: str, reason: str = "") -> dict:
        """Mark a run valid | contaminated | withdrawn.

        Non-valid runs drop out of stats/compare/trends by default instead of
        having to be deleted.
        """
        if validity not in ("valid", "contaminated", "withdrawn"):
            raise BenchError(f"bad validity {validity!r}")
        return self.patch(run_id, validity=validity, validity_reason=reason)

    def phase_mark(self, run_id: str, phase: str, edge: str, ts_ms: int | None = None) -> None:
        """Emit an explicit phase boundary (edge is "start" or "end").

        netem.impair / netem.clear already derive baseline / impaired /
        recovery; use this for phases the netem markers do not describe.
        """
        if edge not in ("start", "end"):
            raise BenchError('edge must be "start" or "end"')
        self.mark(run_id, "harness.mark", {"phase": phase, "edge": edge}, ts_ms)

    def set_baseline(self, suite: str, scenario: str, run_id: str, name: str = "default",
                     thresholds: dict[str, float] | None = None) -> None:
        """Pin a run as the baseline. thresholds is per-metric percent."""
        self._post("/v1/baselines", {"suite": suite, "scenario": scenario,
                                     "name": name, "run_id": run_id,
                                     "thresholds": thresholds or {}})

    # ---------------------------------------------------------------- reports

    def report_url(self, repo: str, commit: str) -> str:
        """The stable page URL for a report — paste this into a commit body."""
        return f"{self.url}{_report_page(repo, commit)}"

    def report_put(self, repo: str, commit: str, title: str, *, branch: str = "",
                   summary: str = "", body: str | None = None, body_path: str | None = None,
                   body_mime: str | None = None, issues: Iterable[int] | None = None,
                   prs: Iterable[int] | None = None, runs: Iterable[str] | None = None,
                   tags: dict | None = None, pinned: bool | None = None) -> dict:
        """Create or replace the report for repo + commit.

        Idempotent: re-publishing the same commit replaces the row in place, so
        the URL already pasted into a commit body keeps resolving and the
        attachments survive. Publishing needs the FULL sha; short shas read.

        body_path reads the narrative from a file and infers body_mime from the
        extension (.html, .md, anything else plain).
        """
        if body_path:
            with open(body_path) as fh:
                body = fh.read()
            body_mime = body_mime or _body_mime(body_path)
        payload: dict[str, Any] = {"title": title, "branch": branch, "summary": summary}
        if body is not None:
            payload["body"] = body
            payload["body_mime"] = body_mime or "text/markdown"
        if issues is not None:
            payload["issues"] = [int(i) for i in issues]
        if prs is not None:
            payload["prs"] = [int(i) for i in prs]
        if runs is not None:
            payload["runs"] = list(runs)
        if tags is not None:
            payload["tags"] = tags
        if pinned is not None:
            payload["pinned"] = pinned
        return self._request("PUT", _report_api(repo, commit),
                             body=json.dumps(payload).encode(),
                             headers={"Content-Type": "application/json"})

    def report_get(self, repo: str, commit: str, agg: str = "") -> dict:
        """One report with its artifacts, linked runs and before/after deltas.

        A short commit prefix is enough as long as it is unique in the repo.
        """
        return self._get(_report_api(repo, commit), {"agg": agg})

    def report_list(self, **filters) -> list[dict]:
        """List reports newest first: repo, branch, issue, tag ("k=v"), since."""
        return self._get("/v1/reports", filters)["reports"]

    def report_attach(self, repo: str, commit: str, path: str, role: str = "",
                      caption: str = "", name: str | None = None, mime: str | None = None) -> dict:
        """Attach evidence.

        role is screenshot | video | log | bundle | other and decides what the
        retention job may prune: screenshots never go, video and bundles go
        first. Omitted, it is inferred from the file.
        """
        return self._upload(_report_api(repo, commit) + "/artifacts", path,
                            {"name": name or "", "mime": mime or "",
                             "role": role, "caption": caption})

    def report_pin(self, repo: str, commit: str, pinned: bool = True) -> dict:
        """Pin (or unpin) a report, exempting its attachments from pruning."""
        verb = "pin" if pinned else "unpin"
        return self._request("POST", _report_api(repo, commit) + "/" + verb)

    def report_delete(self, repo: str, commit: str) -> None:
        self._request("DELETE", _report_api(repo, commit))

    # ---------------------------------------------------------------- read

    def runs(self, **filters) -> list[dict]:
        """List runs.

        has_phase="impaired" keeps only runs that derive that window, and
        include_phases=1 returns every run's windows in the same response — both
        avoid an N+1 fan-out over /phases.
        """
        tags = filters.pop("tags", None) or {}
        params = {f"tag.{k}": v for k, v in tags.items()}
        params.update(filters)
        return self._get("/v1/runs", params)["runs"]

    def runs_with_phases(self, **filters) -> tuple[list[dict], dict[str, list[dict]]]:
        """List runs together with their phase windows, in one call."""
        tags = filters.pop("tags", None) or {}
        params = {f"tag.{k}": v for k, v in tags.items()}
        params.update(filters)
        params["include_phases"] = "1"
        res = self._get("/v1/runs", params)
        return res["runs"], res.get("phases", {})

    def run(self, run_id: str) -> dict:
        return self._get(f"/v1/runs/{run_id}")

    def series(self, run_id: str, keys: Iterable[str] | None = None, source: str = "",
               step: str = "", max_points: int = 2000, window: str = "") -> dict:
        """Downsampled series. window scopes to a phase (baseline/impaired/…)."""
        return self._get(f"/v1/runs/{run_id}/samples", {
            "keys": ",".join(keys) if keys else "", "source": source,
            "step": step, "max_points": max_points, "window": window})

    def phases(self, run_id: str) -> list[dict]:
        """Windows derived from this run's marker events."""
        return self._get(f"/v1/runs/{run_id}/phases")["phases"]

    def stats(self, metric: str, group_by: str = "", agg: str = "p50",
              window: str = "", **filters) -> list[dict]:
        """Aggregate a metric across runs.

        Pass window="impaired" to stop a clean baseline outvoting the window
        the experiment is actually about.
        """
        tags = filters.pop("tags", None) or {}
        params = {"metric": metric, "group_by": group_by, "agg": agg, "window": window}
        params.update({f"tag.{k}": v for k, v in tags.items()})
        params.update(filters)
        return self._get("/v1/stats", params)["rows"]

    def trend(self, metric: str, group_by: str = "", agg: str = "p50",
              window: str = "", **filters) -> list[dict]:
        params = {"metric": metric, "group_by": group_by, "agg": agg, "window": window}
        params.update(filters)
        return self._get("/v1/trend", params)["points"]

    def compare(self, run_ids: Iterable[str], keys: Iterable[str] | None = None,
                source: str = "", window: str = "", align: str = "") -> dict:
        """Align runs for comparison.

        align="impaired" puts t=0 at each run's first impair marker, so runs
        whose impairment started at different offsets still overlay.
        """
        return self._get("/v1/compare", {
            "runs": ",".join(run_ids), "keys": ",".join(keys) if keys else "",
            "source": source, "window": window, "align": align})

    def regressions(self, suite: str, scenario: str, metric: str, agg: str = "p50",
                    window: str = "", baseline: str = "", pct: float | None = None) -> dict:
        """Compare runs against the pinned baseline for suite+scenario.

        Direction comes from the metric registry: for a lower-is-better metric
        a RISE is the regression. Neutral metrics are never flagged.
        """
        params = {"suite": suite, "scenario": scenario, "metric": metric,
                  "agg": agg, "window": window, "baseline": baseline}
        if pct is not None:
            params["pct"] = pct
        return self._get("/v1/regressions", params)

    def metrics(self) -> list[dict]:
        """The metric registry: better = higher | lower | neutral."""
        return self._get("/v1/metrics")["metrics"]

    def health(self) -> dict:
        return self._get("/v1/health")


def now_ms() -> int:
    return int(time.time() * 1000)


def _report_api(repo: str, commit: str) -> str:
    """A repo is ONE path segment, so its slash must stay encoded."""
    return f"/v1/reports/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(commit)}"


def _report_page(repo: str, commit: str) -> str:
    return f"/reports/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(commit)}"


def _body_mime(path: str) -> str:
    lower = path.lower()
    if lower.endswith((".html", ".htm")):
        return "text/html"
    if lower.endswith((".md", ".markdown")):
        return "text/markdown"
    return "text/plain"


def _read_records(path: str) -> list[dict]:
    """Read a JSON array, a {"samples"/"events": [...]} object, or JSONL."""
    with open(path) as fh:
        text = fh.read().strip()
    if not text:
        return []
    if text[0] == "[":
        return json.loads(text)
    if text[0] == "{" and "\n" not in text.strip():
        obj = json.loads(text)
        for key in ("samples", "events"):
            if key in obj:
                return obj[key]
        return [obj]
    return [json.loads(line) for line in text.splitlines() if line.strip()]


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="bench.py", description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--url", default=None, help="base URL (default $BENCH_URL)")
    p.add_argument("--key", default=None, help="API key (default $BENCH_KEY)")
    sub = p.add_subparsers(dest="cmd", required=True)

    n = sub.add_parser("new", help="create a run, print its id")
    n.add_argument("--suite", required=True)
    n.add_argument("--scenario", required=True)
    n.add_argument("--host", default="")
    n.add_argument("--notes", default="")
    n.add_argument("--tag", action="append", default=[], metavar="K=V")
    n.add_argument("--external-id", default=None)
    n.add_argument("--conditions", default=None, help="path to a JSON file")

    s = sub.add_parser("samples", help="post samples from a JSON/JSONL file (- for stdin)")
    s.add_argument("run_id")
    s.add_argument("--file", required=True)
    s.add_argument("--replace", action="store_true",
                   help="clear the existing series for each source first (use for a re-fold)")
    s.add_argument("--expect", action="append", default=[], metavar="SOURCE=N",
                   help="reject the write unless the source ends up with N samples")

    ds = sub.add_parser("delete-samples", help="delete samples by source and/or time range")
    ds.add_argument("run_id")
    ds.add_argument("--source", default="")
    ds.add_argument("--from", dest="from_ms", type=int, default=None)
    ds.add_argument("--to", dest="to_ms", type=int, default=None)

    rn = sub.add_parser("rename-key", help="rename a metric key inside a run's samples")
    rn.add_argument("run_id")
    rn.add_argument("from_key")
    rn.add_argument("to_key")
    rn.add_argument("--source", default="")
    rn.add_argument("--overwrite", action="store_true")

    cn = sub.add_parser("counts", help="samples per source for a run")
    cn.add_argument("run_id")

    rm = sub.add_parser("register-metric", help="declare a metric direction")
    rm.add_argument("key")
    rm.add_argument("better", choices=["higher", "lower", "neutral"])
    rm.add_argument("--unit", default="")
    rm.add_argument("--regression-pct", type=float, default=0)

    e = sub.add_parser("events", help="post events from a JSON/JSONL file")
    e.add_argument("run_id")
    e.add_argument("--file", required=True)

    a = sub.add_parser("artifact", help="upload a file")
    a.add_argument("run_id")
    a.add_argument("path")
    a.add_argument("--name", default=None)

    f = sub.add_parser("finish", help="close a run out")
    f.add_argument("run_id")
    f.add_argument("--status", default="finished")
    f.add_argument("--verdict", default=None, choices=["PASS", "FAIL", "INFO"])
    f.add_argument("--summary", default=None, help="path to a JSON file")
    f.add_argument("--tag", action="append", default=[], metavar="K=V")

    st = sub.add_parser("stats", help="aggregate a metric across runs")
    st.add_argument("--metric", required=True)
    st.add_argument("--group-by", default="")
    st.add_argument("--agg", default="p50")
    st.add_argument("--suite", default="")
    st.add_argument("--scenario", default="")
    st.add_argument("--since", default="")
    st.add_argument("--window", default="", help="baseline | impaired | recovery | <mark phase>")

    ls = sub.add_parser("runs", help="list runs")
    ls.add_argument("--suite", default="")
    ls.add_argument("--scenario", default="")
    ls.add_argument("--limit", default=20)

    pt = sub.add_parser("validity", help="mark a run valid/contaminated/withdrawn")
    pt.add_argument("run_id")
    pt.add_argument("validity", choices=["valid", "contaminated", "withdrawn"])
    pt.add_argument("--reason", default="")

    ph = sub.add_parser("phases", help="show the derived phase windows of a run")
    ph.add_argument("run_id")

    rg = sub.add_parser("regressions", help="compare runs against the pinned baseline")
    rg.add_argument("--suite", required=True)
    rg.add_argument("--scenario", required=True)
    rg.add_argument("--metric", required=True)
    rg.add_argument("--agg", default="p50")
    rg.add_argument("--window", default="")

    bl = sub.add_parser("baseline", help="pin a run as the baseline")
    bl.add_argument("run_id")
    bl.add_argument("--suite", required=True)
    bl.add_argument("--scenario", required=True)
    bl.add_argument("--name", default="default")

    sub.add_parser("metrics", help="show the metric direction registry")
    sub.add_parser("health", help="ping the service")
    sub.add_parser("version", help="print the client version")

    args = p.parse_args(argv)
    b = Bench(args.url, args.key)

    if args.cmd == "new":
        tags = dict(t.split("=", 1) for t in args.tag)
        conditions = json.load(open(args.conditions)) if args.conditions else None
        print(b.new_run(args.suite, args.scenario, args.host, tags, args.notes,
                        conditions=conditions, external_id=args.external_id))
    elif args.cmd == "samples":
        records = json.load(sys.stdin) if args.file == "-" else _read_records(args.file)
        expected = {k: int(v) for k, v in (e.split("=", 1) for e in args.expect)}
        print(b.samples(args.run_id, records, replace=args.replace,
                        expected_count=expected or None))
    elif args.cmd == "delete-samples":
        print(json.dumps(b.delete_samples(args.run_id, args.source, args.from_ms, args.to_ms)))
    elif args.cmd == "rename-key":
        print(json.dumps(b.rename_metric_key(args.run_id, args.from_key, args.to_key,
                                             args.source, args.overwrite)))
    elif args.cmd == "counts":
        print(json.dumps(b.counts(args.run_id), indent=2))
    elif args.cmd == "register-metric":
        print(json.dumps(b.register_metric(args.key, args.better, args.unit, args.regression_pct)))
    elif args.cmd == "events":
        print(b.events(args.run_id, _read_records(args.file)))
    elif args.cmd == "artifact":
        print(json.dumps(b.artifact(args.run_id, args.path, args.name)))
    elif args.cmd == "finish":
        summary = json.load(open(args.summary)) if args.summary else None
        tags = dict(t.split("=", 1) for t in args.tag)
        print(json.dumps(b.finish(args.run_id, args.status, args.verdict, summary, tags)))
    elif args.cmd == "stats":
        rows = b.stats(args.metric, args.group_by, args.agg, window=args.window,
                       suite=args.suite, scenario=args.scenario, since=args.since)
        print(json.dumps(rows, indent=2))
    elif args.cmd == "validity":
        print(json.dumps(b.set_validity(args.run_id, args.validity, args.reason)))
    elif args.cmd == "phases":
        print(json.dumps(b.phases(args.run_id), indent=2))
    elif args.cmd == "regressions":
        print(json.dumps(b.regressions(args.suite, args.scenario, args.metric,
                                       args.agg, args.window), indent=2))
    elif args.cmd == "baseline":
        b.set_baseline(args.suite, args.scenario, args.run_id, args.name)
        print("ok")
    elif args.cmd == "metrics":
        print(json.dumps(b.metrics(), indent=2))
    elif args.cmd == "version":
        print(__version__)
    elif args.cmd == "runs":
        print(json.dumps(b.runs(suite=args.suite, scenario=args.scenario, limit=args.limit), indent=2))
    elif args.cmd == "health":
        print(json.dumps(b.health()))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except BenchError as exc:
        print(f"error: {exc}", file=sys.stderr)
        sys.exit(1)
