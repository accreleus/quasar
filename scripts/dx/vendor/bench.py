# VENDORED FILE — DO NOT EDIT HERE.
#
# Source: quasar-bench client/bench.py
# Commit: d55c43808dbde9eb9140a01580b16eb848ee960f (2026-09-22)  __version__ 1.7.0
# Fetched: 2026-09-22, as the bench.py inside the qbench 1.7.0 zipapp your bench
#          server publishes (GET $BENCH_URL/cli/qbench); byte-identical to the
#          file at the commit above (git blob 6edf2cb99c6d).
#
# Re-vendor with (from a quasar-bench checkout, or unzip the zipapp):
#   cp ../quasar-bench/client/bench.py scripts/dx/vendor/bench.py
# then re-add this header and update the commit line above. Everything after
# the END-OF-HEADER line is the upstream file verbatim; scripts/dx/tests/run.sh
# checks that.
#
# bench_submit.py compares this file's __version__ against the version the
# SERVER publishes on every submission and warns when this copy is older.
# The upstream DEFAULT_URL (localhost) is never relied on: every repo script
# resolves the server through scripts/dx/bench_config.py first.
# ── END-OF-HEADER ──
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

    export BENCH_URL=https://bench.example BENCH_KEY=...
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
__version__ = "1.7.0"

DEFAULT_URL = "http://localhost:9400"
GZIP_THRESHOLD = 64 * 1024
CHUNK = 5000  # samples per request; the server handles 20k, this keeps retries cheap


class BenchError(RuntimeError):
    """A failed call. status is the HTTP status (0 when the server was never
    reached) and server_message is the server's own `error` text, if any."""

    def __init__(self, message: str, status: int = 0, server_message: str = "",
                 method: str = "", path: str = ""):
        super().__init__(message)
        self.status, self.server_message = status, server_message
        self.method, self.path = method, path


class CountMismatch(BenchError):
    """A sample write was rejected because a source ended up the wrong length."""

    def __init__(self, message: str, source: str = "", expected: int = 0, actual: int = 0):
        super().__init__(message, status=409, server_message=message)
        self.source, self.expected, self.actual = source, expected, actual


class Bench:
    def __init__(self, url: str | None = None, key: str | None = None, timeout: int = 120,
                 require_key: bool = True):
        self.url = (url or os.environ.get("BENCH_URL") or DEFAULT_URL).rstrip("/")
        self.key = key or os.environ.get("BENCH_KEY") or ""
        self.timeout = timeout
        # require_key=False is for the public endpoints (/v1/health, /cli/...):
        # the request then goes out without an Authorization header.
        if not self.key and require_key:
            raise BenchError("no API key: set BENCH_KEY or pass key=")

    # ---------------------------------------------------------------- http

    def _request(self, method: str, path: str, *, body: bytes | None = None,
                 headers: dict[str, str] | None = None) -> Any:
        return self._request_status(method, path, body=body, headers=headers)[1]

    def _request_status(self, method: str, path: str, *, body: bytes | None = None,
                        headers: dict[str, str] | None = None) -> tuple[int, Any]:
        """Same as _request, but also returns the HTTP status code — callers
        that need to tell a plain 200 (upsert matched something existing)
        from a 201 (freshly created) use this instead."""
        req = urllib.request.Request(self.url + path, data=body, method=method)
        if self.key:
            req.add_header("Authorization", f"Bearer {self.key}")
        req.add_header("User-Agent", f"quasar-bench-client/{__version__}")
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as res:
                raw = res.read()
                status = res.status
        except urllib.error.HTTPError as e:
            raw_err = e.read()
            detail = raw_err.decode("utf-8", "replace")[:400].strip()
            try:
                body = json.loads(raw_err)
            except (json.JSONDecodeError, UnicodeDecodeError):
                body = {}
            if not isinstance(body, dict):
                body = {}
            if e.code == 409 and "expected" in body:
                raise CountMismatch(body.get("error", detail), body.get("source", ""),
                                    int(body.get("expected", 0)),
                                    int(body.get("actual", 0))) from None
            msg = str(body.get("error") or detail or e.reason)
            raise BenchError(f"{method} {path} -> {e.code}: {msg}", status=e.code,
                             server_message=msg, method=method, path=path) from None
        except urllib.error.URLError as e:
            raise BenchError(f"{method} {path} -> {e.reason}", method=method, path=path) from None
        except OSError as e:
            raise BenchError(f"{method} {path} -> {e}", method=method, path=path) from None
        if not raw:
            return status, None
        try:
            return status, json.loads(raw)
        except json.JSONDecodeError:
            return status, raw

    def _post(self, path: str, payload: dict) -> Any:
        return self._post_status(path, payload)[1]

    def _post_status(self, path: str, payload: dict) -> tuple[int, Any]:
        body = json.dumps(payload).encode()
        headers = {"Content-Type": "application/json"}
        if len(body) > GZIP_THRESHOLD:
            body = gzip.compress(body, 6)
            headers["Content-Encoding"] = "gzip"
        return self._request_status("POST", path, body=body, headers=headers)

    def _get(self, path: str, params: dict | None = None) -> Any:
        if params:
            clean = {k: v for k, v in params.items() if v not in (None, "")}
            path += "?" + urllib.parse.urlencode(clean)
        return self._request("GET", path)

    # ---------------------------------------------------------------- write

    def new_run(self, suite: str, scenario: str, host: str = "",
                tags: dict[str, str] | None = None, notes: str = "",
                conditions: dict | None = None, external_id: str | None = None) -> str:
        """Create (or upsert, with external_id) a run and return its id."""
        return self.new_run_created(suite, scenario, host, tags, notes,
                                    conditions, external_id)[0]

    def new_run_created(self, suite: str, scenario: str, host: str = "",
                        tags: dict[str, str] | None = None, notes: str = "",
                        conditions: dict | None = None,
                        external_id: str | None = None) -> tuple[str, bool]:
        """Same as new_run, but also reports whether this call actually
        created the run (True, HTTP 201) or upserted an existing row that
        already carried this external_id (False, HTTP 200).

        Use the flag to keep a re-run of a seed/harness script idempotent:
        events have no upsert key, so post them only when the run is new,
        rather than appending a second copy onto a run that already has them.

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
        status, res = self._post_status("/v1/runs", body)
        return res["id"], status == 201

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

    def artifact(self, run_id: str, path: str, name: str | None = None, mime: str | None = None,
                role: str = "", caption: str = "", started_at_ms: int | None = None) -> dict:
        """Upload a run artifact.

        role is screenshot | video | log | bundle | other (inferred from the
        file when omitted) and drives the retention job's prune order the
        same way it does for report evidence. started_at_ms is the
        wall-clock ms of a capture's first frame, so the run page can sync
        the video playhead to the charts.
        """
        fields = {"name": name or "", "mime": mime or "", "role": role, "caption": caption}
        if started_at_ms is not None:
            fields["started_at_ms"] = str(started_at_ms)
        return self._upload(f"/v1/runs/{run_id}/artifacts", path, fields)

    def artifact_patch(self, run_id: str, name: str, *, caption: str | None = None,
                       role: str | None = None, started_at_ms: int | None = None) -> dict:
        """Update a run artifact's role/caption/started_at_ms after the fact."""
        fields: dict[str, Any] = {}
        if caption is not None:
            fields["caption"] = caption
        if role is not None:
            fields["role"] = role
        if started_at_ms is not None:
            fields["started_at_ms"] = started_at_ms
        return self._request("PATCH", f"/v1/runs/{run_id}/artifacts/{urllib.parse.quote(name)}",
                             body=json.dumps(fields).encode(),
                             headers={"Content-Type": "application/json"})

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

    # ---------------------------------------------------------------- sprints

    def sprint_url(self, repo: str, sprint: str) -> str:
        """The stable page URL for a sprint report."""
        return f"{self.url}{_sprint_page(repo, sprint)}"

    def sprint_put(self, repo: str, sprint: str, title: str, *, sprint_label: str = "",
                   branch: str = "", summary: str = "", body: str | None = None,
                   body_path: str | None = None, body_mime: str | None = None,
                   issues: Iterable[int] | None = None, prs: Iterable[int] | None = None,
                   runs: Iterable[str] | None = None, tags: dict | None = None,
                   pinned: bool | None = None, commit: str = "",
                   started_at: str | None = None, ended_at: str | None = None,
                   commits: list | None = None, sections: dict | None = None,
                   author: str = "") -> dict:
        """Create or replace the sprint report for repo + lower(sprint).

        Idempotent on repo + lower(sprint): re-publishing updates the row in
        place and bumps revision; if the report was changes_requested it
        resets to submitted. sections is the structured write-up (see
        client/examples/sprint.json for the shape); author defaults
        server-side to the caller's API key name when omitted.
        """
        if body_path:
            with open(body_path) as fh:
                body = fh.read()
            body_mime = body_mime or _body_mime(body_path)
        payload: dict[str, Any] = {"title": title, "branch": branch, "summary": summary}
        if sprint_label:
            payload["sprint_label"] = sprint_label
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
        if commit:
            payload["commit"] = commit
        if started_at:
            payload["started_at"] = started_at
        if ended_at:
            payload["ended_at"] = ended_at
        if commits is not None:
            payload["commits"] = commits
        if sections is not None:
            payload["sections"] = sections
        if author:
            payload["author"] = author
        return self._request("PUT", _sprint_api(repo, sprint),
                             body=json.dumps(payload).encode(),
                             headers={"Content-Type": "application/json"})

    def sprint_get(self, repo: str, sprint: str, agg: str = "") -> dict:
        """One sprint report with its sections, artifacts, linked runs,
        deltas, review status and comments."""
        return self._get(_sprint_api(repo, sprint), {"agg": agg})

    def sprint_list(self, **filters) -> list[dict]:
        """List sprint reports newest first (same filters as report_list)."""
        filters.setdefault("kind", "sprint")
        return self._get("/v1/reports", filters)["reports"]

    def sprint_attach(self, repo: str, sprint: str, path: str, role: str = "",
                      caption: str = "", name: str | None = None, mime: str | None = None) -> dict:
        """Attach evidence to a sprint report — same roles as report_attach."""
        return self._upload(_sprint_api(repo, sprint) + "/artifacts", path,
                            {"name": name or "", "mime": mime or "",
                             "role": role, "caption": caption})

    def sprint_pin(self, repo: str, sprint: str, pinned: bool = True) -> dict:
        verb = "pin" if pinned else "unpin"
        return self._request("POST", _sprint_api(repo, sprint) + "/" + verb)

    def sprint_delete(self, repo: str, sprint: str) -> None:
        self._request("DELETE", _sprint_api(repo, sprint))

    # ------------------------------------------------------- review & comments

    def _review_api(self, repo: str, ref: str, kind: str) -> str:
        return _sprint_api(repo, ref) if kind == "sprint" else _report_api(repo, ref)

    def review(self, repo: str, ref: str, status: str, note: str = "", kind: str = "commit") -> dict:
        """Set the review verdict on a report of either kind.

        kind is "commit" (ref = commit sha) or "sprint" (ref = sprint slug).
        status is submitted | approved | changes_requested | acknowledged.
        Re-publishing a report while it is changes_requested resets it to
        submitted automatically.
        """
        return self._request("POST", self._review_api(repo, ref, kind) + "/review",
                             body=json.dumps({"status": status, "note": note}).encode(),
                             headers={"Content-Type": "application/json"})

    def comment(self, repo: str, ref: str, body: str, anchor: str = "", kind: str = "commit") -> dict:
        """Add a comment, optionally anchored to a section/item (e.g. "goals.2")."""
        return self._request("POST", self._review_api(repo, ref, kind) + "/comments",
                             body=json.dumps({"anchor": anchor, "body": body}).encode(),
                             headers={"Content-Type": "application/json"})

    def comments(self, repo: str, ref: str, kind: str = "commit") -> list[dict]:
        """List a report's comments, oldest first."""
        return self._get(self._review_api(repo, ref, kind) + "/comments")["comments"]

    def comment_resolve(self, repo: str, ref: str, comment_id: int, resolved: bool = True,
                        kind: str = "commit") -> dict:
        return self._request("PATCH", self._review_api(repo, ref, kind) + f"/comments/{comment_id}",
                             body=json.dumps({"resolved": resolved}).encode(),
                             headers={"Content-Type": "application/json"})

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

    def me(self) -> dict:
        """Who the key authenticates as: kind, name, role, read_only."""
        return self._get("/v1/me")

    def summary(self, run_id: str) -> dict:
        """The agent view of one run: header strip, windowed aggregates,
        citing reports and baseline_delta."""
        return self._get(f"/v1/runs/{run_id}/summary")

    def baseline(self, suite: str, scenario: str, name: str = "default") -> dict:
        """The pinned baseline for suite+scenario ({"run_id": ...}); 404 if none."""
        return self._get(f"/v1/baselines/{urllib.parse.quote(suite, safe='')}/"
                         f"{urllib.parse.quote(scenario, safe='')}", {"name": name})

    # ---------------------------------------------------------- commit compare

    def commits(self, repo: str, limit: int = 50) -> list[dict]:
        """Commits that have runs in repo, newest first."""
        return self._get("/v1/commits", {"repo": repo, "limit": limit})["commits"]

    def compare_commits(self, base: str, head: str, repo: str = "", window: str = "",
                        stat: str = "", keys: Iterable[str] | None = None) -> dict:
        """Judge commit `head` against commit `base` (short prefixes read as
        long as they are unique; a 409 means the prefix is ambiguous).

        For every (suite, scenario) with a valid run at both commits: the
        latest run at each, a direction-aware delta table and a verdict
        (regressed | improved | mixed | unchanged). Plus only_base/only_head
        (scenarios that ran at only one commit) and excluded_invalid runs.
        repo defaults to BENCH_DEFAULT_REPO server-side when omitted.
        """
        return self._get("/v1/commits/compare", {
            "repo": repo, "base": base, "head": head, "window": window,
            "stat": stat, "keys": ",".join(keys) if keys else ""})

    # ---------------------------------------------------------- github board

    def config_links(self) -> dict:
        """github_url, project_url, default_repo, board_configured."""
        return self._get("/v1/config/links")

    def github_sync_status(self) -> dict:
        return self._get("/v1/github/sync")

    def github_sync(self) -> dict:
        """Force an immediate board sync. 409 if BENCH_GITHUB_TOKEN is unset."""
        return self._request("POST", "/v1/github/sync")

    def board_items(self, repo: str = "", status: str = "", evidence: str = "",
                    level: str = "", state: str = "", cited: bool | None = None) -> list[dict]:
        """Synced GitHub project board items, each with cited_by."""
        params: dict[str, Any] = {"repo": repo, "status": status, "evidence": evidence,
                                  "level": level, "state": state}
        if cited is not None:
            params["cited"] = "1" if cited else "0"
        return self._get("/v1/github/items", params)["items"]

    def evidence_gaps(self) -> list[dict]:
        """Open board items that need bench evidence and do not have any
        from an approved report — each row says why (uncited |
        cited_unapproved) and, if cited, by which reports."""
        return self._get("/v1/evidence/gaps")["gaps"]

    # ---------------------------------------------------------- snippets

    def report_snippet(self, repo: str, commit: str, agg: str = "") -> str:
        """A short markdown block for a GitHub issue/PR comment: the bench
        link, review status, before/after verdict and evidence summary."""
        rep = self.report_get(repo, commit, agg)
        return _report_snippet(self._absolute(rep))

    def sprint_snippet(self, repo: str, sprint: str, agg: str = "") -> str:
        """Same as report_snippet, for a sprint report."""
        rep = self.sprint_get(repo, sprint, agg)
        return _report_snippet(self._absolute(rep))

    def _absolute(self, rep: dict) -> dict:
        """The server returns page URLs as paths; a snippet pasted into
        GitHub needs the full URL."""
        url = rep.get("url") or ""
        if url.startswith("/"):
            rep = dict(rep, url=self.url + url)
        return rep


def now_ms() -> int:
    return int(time.time() * 1000)


def _report_api(repo: str, commit: str) -> str:
    """A repo is ONE path segment, so its slash must stay encoded."""
    return f"/v1/reports/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(commit)}"


def _report_page(repo: str, commit: str) -> str:
    return f"/reports/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(commit)}"


def _sprint_api(repo: str, sprint: str) -> str:
    return f"/v1/sprints/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(sprint)}"


def _sprint_page(repo: str, sprint: str) -> str:
    return f"/sprints/{urllib.parse.quote(repo, safe='')}/{urllib.parse.quote(sprint)}"


def _body_mime(path: str) -> str:
    lower = path.lower()
    if lower.endswith((".html", ".htm")):
        return "text/html"
    if lower.endswith((".md", ".markdown")):
        return "text/markdown"
    return "text/plain"


def _report_snippet(rep: dict) -> str:
    """Render a report/sprint detail (as returned by report_get/sprint_get)
    as a short markdown block, for pasting into a GitHub issue comment."""
    title = rep.get("title") or rep.get("sprint_label") or "bench report"
    lines = [f"**Bench:** [{title}]({rep.get('url', '')})"]

    review = rep.get("review") or {}
    status = review.get("status") or "not reviewed"
    by = f" by {review['by']}" if review.get("by") else ""
    lines.append(f"**Review:** {status}{by}")

    deltas = rep.get("run_deltas") or []
    if deltas:
        regressed = [d["metric"] for d in deltas if d.get("regressed")]
        improved = [d["metric"] for d in deltas if d.get("improved")]
        bits = []
        if improved:
            bits.append(f"better on {', '.join(improved)}")
        if regressed:
            bits.append(f"worse on {', '.join(regressed)}")
        lines.append("**Runs:** " + ("; ".join(bits) if bits else f"{len(deltas)} metric(s) compared, none beyond threshold"))

    if "goal_counts" in rep and rep["goal_counts"]:
        gc = rep["goal_counts"]
        lines.append("**Goals:** " + ", ".join(f"{v} {k}" for k, v in sorted(gc.items())))
    if "test_counts" in rep and rep["test_counts"]:
        tc = rep["test_counts"]
        lines.append("**Tests:** " + ", ".join(f"{v} {k}" for k, v in sorted(tc.items())))

    n = rep.get("artifact_count", 0)
    role_counts = rep.get("role_counts") or {}
    roles = ", ".join(f"{v} {k}" for k, v in sorted(role_counts.items())) if role_counts else ""
    lines.append(f"**Evidence:** {n} attachment(s)" + (f" ({roles})" if roles else ""))

    return "\n".join(lines)


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
    a.add_argument("--role", default="",
                   choices=["", "screenshot", "video", "log", "bundle", "other"],
                   help="drives prune order; inferred from the file when omitted")
    a.add_argument("--caption", default="")
    a.add_argument("--started-at-ms", dest="started_at_ms", type=int, default=None,
                   help="wall-clock ms of a capture's first frame")

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
        print(json.dumps(b.artifact(args.run_id, args.path, args.name,
                                    role=args.role, caption=args.caption,
                                    started_at_ms=args.started_at_ms)))
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
