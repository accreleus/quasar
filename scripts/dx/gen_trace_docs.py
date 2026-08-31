#!/usr/bin/env python3
"""Render trace-format.md's metric-taxonomy and agent-event tables from the manifest.

    make docs-trace          # regenerate
    python3 scripts/dx/gen_trace_docs.py --check   # fail if stale

The manifest (docs/session-trace/metrics.json) is the source; the table between
the `<!-- metrics:begin -->` / `<!-- metrics:end -->` markers in
docs/session-trace/trace-format.md §2 is generated from it. Before this the two
were maintained by hand and disagreed: the doc table carried `client.decode_ms`
with no hint it was a 1 s mean while `client.glass_to_glass_ms` beside it was a
multi-minute rolling median.

Only rows with a taxonomy name appear — the taxonomy is the curated diagnostic
lens, and the manifest is the exhaustive dictionary behind it.
"""
import argparse
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
MANIFEST = ROOT / "docs" / "session-trace" / "metrics.json"
DOC = ROOT / "docs" / "session-trace" / "trace-format.md"
BEGIN = "<!-- metrics:begin -->"
END = "<!-- metrics:end -->"
EVENTS_BEGIN = "<!-- events:begin -->"
EVENTS_END = "<!-- events:end -->"

# The taxonomy is browser/native-shared for client-side sources (schema.md P9-01).
SOURCE_LABEL = {"agent": "agent", "browser": "browser/native", "native": "browser/native"}


def cell(text: str) -> str:
    return text.replace("|", "\\|").replace("\n", " ").strip()


def render(manifest: dict) -> str:
    rows = [e for e in manifest["metrics"] if e.get("taxonomy")]
    rows.sort(key=lambda e: (e["source"] != "agent", e["taxonomy"]))
    out = [
        BEGIN,
        "<!-- GENERATED from docs/session-trace/metrics.json by scripts/dx/gen_trace_docs.py.",
        "     Do not edit between these markers; edit the manifest and run `make docs-trace`. -->",
        "",
        f"*Manifest version `{manifest['version']}` — {len(rows)} curated series of "
        f"{len(manifest['metrics'])} documented keys.*",
        "",
        "| taxonomy name | source | raw key | unit | clock | window | estimator | n | meaning / the trap it avoids |",
        "|---|---|---|---|---|---|---|---|---|",
    ]
    for e in rows:
        note = e["why"]
        if e.get("deprecated_for"):
            note = f"**DEPRECATED — read `{e['deprecated_for']}`.** {note}"
        out.append(
            "| `{tax}` | {src} | `{key}` | {unit} | `{clock}` | `{window}` | `{est}` | {n} | {why} |".format(
                tax=e["taxonomy"],
                src=SOURCE_LABEL.get(e["source"], e["source"]),
                key=e["key"],
                unit=e["unit"],
                clock=e["clock"],
                window=e["window"],
                est=e["estimator"],
                n=f"`{e['n_key']}`" if e.get("n_key") else "—",
                why=cell(note),
            )
        )
    out += ["", END]
    return "\n".join(out)


def render_events(manifest: dict) -> str:
    """Section 3.2 — the AGENT event allow-list.

    Generated for the same reason the metric table is: the doc table and the emitters
    drifted, and the two facts a reader needs about an event — what is in the payload and
    whether the row can be missing — were only ever in prose, if anywhere.
    """
    rows = [e for e in manifest.get("events", []) if e.get("source") == "agent"]
    # Reliable first: "can this row be absent" is the first thing a reader must know.
    rows.sort(key=lambda e: (e["lane"] != "reliable", e["type"]))
    out = [
        EVENTS_BEGIN,
        "<!-- GENERATED from docs/session-trace/metrics.json by scripts/dx/gen_trace_docs.py.",
        "     Do not edit between these markers; edit the manifest and run `make docs-trace`. -->",
        "",
        f"*{len(rows)} agent-source event types. `lane` is load-bearing: a **reliable** row is "
        "ordered, never coalesced and never dropped, so its absence is a defect; a "
        "**diagnostic** row rides the bounded droppable lane, so its absence is not evidence "
        "the event did not happen.*",
        "",
        "| type | lane | payload keys | clock | meaning / the trap it avoids |",
        "|---|---|---|---|---|",
    ]
    for e in rows:
        keys = ", ".join(f"`{k}`" for k in e["payload_keys"]) or "—"
        out.append(
            "| `{t}` | {lane} | {keys} | `{clock}` | {why} |".format(
                t=e["type"], lane=e["lane"], keys=cell(keys), clock=e["clock"], why=cell(e["why"])
            )
        )
    out += ["", EVENTS_END]
    return "\n".join(out)


def splice(doc: str, block: str, begin: str = BEGIN, end: str = END) -> str:
    i = doc.find(begin)
    j = doc.find(end)
    if i < 0 or j < 0:
        sys.exit(f"markers {begin} / {end} not found in {DOC}")
    return doc[:i] + block + doc[j + len(end):]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true",
                    help="exit non-zero if the doc is stale instead of rewriting it")
    args = ap.parse_args()

    manifest = json.loads(MANIFEST.read_text())
    doc = DOC.read_text()
    want = splice(doc, render(manifest))
    want = splice(want, render_events(manifest), EVENTS_BEGIN, EVENTS_END)
    if want == doc:
        print("trace-format.md metric + event tables are up to date")
        return 0
    if args.check:
        print("STALE docs/session-trace/trace-format.md — run: make docs-trace", file=sys.stderr)
        return 1
    DOC.write_text(want)
    print("regenerated docs/session-trace/trace-format.md metric + event tables")
    return 0


if __name__ == "__main__":
    sys.exit(main())
