#!/usr/bin/env python3
"""Every key the bench harness folds must exist in the metric manifest, with
the right `source`, and must not be `deprecated_for` another key.

C11 #5 (`docs/design/2026-08-23-c11-reports-evidence-spec.md` Phase 1): the
harness's ROLLUP_KEYS/ROLLUP_COUNTERS in bench_submit.py used to be a private
list that could silently drift from docs/session-trace/metrics.json — which is
exactly how `bitrate_kbps` (the wrong bitrate: encoder output, not what ABR
asked for) hid in bench rollups for months. This check reads ROLLUP_KEYS /
ROLLUP_COUNTERS straight out of bench_submit.py (imported as a module, not
regexed — a key list is data, not a pattern to guess at) and asserts every
key is:

  1. present in the manifest under the SAME source (browser/agent), and
  2. not `deprecated_for` another key (folding a deprecated key hides the
     regression the campaign that deprecated it was trying to surface).

Run from scripts/dx/tests/run.sh.
"""
from __future__ import annotations

import importlib.util
import json
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[3]
DX_DIR = ROOT / "scripts" / "dx"
MANIFEST_PATH = ROOT / "docs" / "session-trace" / "metrics.json"


def load_bench_submit():
    """Import scripts/dx/bench_submit.py as a module, the same way
    scripts/dx/tests/run.sh's other embedded-python assertions do (spec_from_
    file_location, not a subprocess) — the vendor/ and DX_DIR sys.path entries
    it needs on import are set up inside the module itself."""
    spec = importlib.util.spec_from_file_location("bench_submit", DX_DIR / "bench_submit.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def load_manifest() -> dict:
    doc = json.loads(MANIFEST_PATH.read_text())
    # {(source, key): entry}
    return {(e["source"], e["key"]): e for e in doc["metrics"]}


def main() -> int:
    bs = load_bench_submit()
    manifest = load_manifest()

    problems: list[str] = []

    def check(source: str, key: str) -> None:
        entry = manifest.get((source, key))
        if entry is None:
            problems.append("%s.%s is folded but absent from metrics.json" % (source, key))
            return
        if entry.get("deprecated_for"):
            problems.append(
                "%s.%s is folded but deprecated_for=%r in metrics.json — fold the "
                "replacement key instead" % (source, key, entry["deprecated_for"]))

    for source, keys in bs.ROLLUP_KEYS.items():
        for key in keys:
            check(source, key)
    for source, keys in bs.ROLLUP_COUNTERS.items():
        for key in keys:
            check(source, key)

    if problems:
        print("bench key drift:\n  " + "\n  ".join(problems), file=sys.stderr)
        return 1
    n = sum(len(v) for v in bs.ROLLUP_KEYS.values()) + \
        sum(len(v) for v in bs.ROLLUP_COUNTERS.values())
    print("ok: %d folded bench keys, all in the manifest under the right source "
          "and none deprecated" % n)
    return 0


if __name__ == "__main__":
    sys.exit(main())
