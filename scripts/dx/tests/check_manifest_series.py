#!/usr/bin/env python3
"""Every series or event type verdict.go names must exist in the manifest.

A falsifier naming a series the taxonomy does not define reports `value: null`,
`n: 0`, `note: "no samples"` forever — indistinguishable from a measurement that
was simply quiet, which trace-format.md §8 calls the most misleading thing this
surface can do. Run from scripts/dx/tests/run.sh.
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[3]
manifest = json.loads((ROOT / "docs/session-trace/metrics.json").read_text())
taxonomy = {e["taxonomy"] for e in manifest["metrics"] if e.get("taxonomy")}
# Event TYPES share the namespace prefixes with series names (`encoder.stall` next to
# `encoder.fps`), and verdict.go names both: series in falsifiers, event types in the
# reason suffix. They are two vocabularies in one manifest, so both are legal here — an
# event type that is NOT in the manifest still fails, which is the check that matters.
event_types = {e["type"] for e in manifest.get("events", [])}
known = taxonomy | event_types

src = (ROOT / "control-plane/internal/session/verdict.go").read_text()
used = set(re.findall(
    r'"((?:client|transport|encoder|abr|source|compositor|interpipe|rtp)\.[a-z0-9_.]+)"', src))
if not used:
    print("no falsifier series found in verdict.go — the pattern has drifted", file=sys.stderr)
    sys.exit(1)

missing = sorted(used - known)
if missing:
    print("series used by verdict.go but absent from the manifest: " + ", ".join(missing),
          file=sys.stderr)
    sys.exit(1)
print("ok: %d falsifier series / event types, all defined in the manifest" % len(used))
