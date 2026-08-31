"""scripts/dx/thresholds.py — one loader for docs/session-trace/thresholds.json.

The bench harness had its own private copies of a couple of numbers that
belong to `docs/session-trace/thresholds.json` (the golden threshold file
consumed by the Go/web drift tests, `docs/session-trace/thresholds.json`
`_readme`). This module is the harness-side third reader, so a threshold
changing there flows here without a second hard-coded copy to forget.

Usage:

    from thresholds import value, warmup_exclude_s
    value("classifier.hitch_sd_ms")   # -> 18.0
    warmup_exclude_s()                # -> 20.0

Never raises on a missing/unreadable file or a missing key — a bench harness
concern (metric registration direction, a default) must not fail because a
doc file moved or a key was renamed; callers get `default` back and, for the
module-level accessors, a documented fallback constant that matches the value
this file shipped with at the time it was written.
"""

from __future__ import annotations

import json
import os

# scripts/dx/thresholds.py -> scripts/dx -> scripts -> <repo root>
ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
THRESHOLDS_PATH = os.path.join(ROOT, "docs", "session-trace", "thresholds.json")

# Matches docs/session-trace/thresholds.json as of 2026-08-23 — used only when
# the file cannot be read at all, so a bench harness call never hard-fails on
# a missing doc checkout (e.g. a stripped-down CI worktree).
_FALLBACK_WARMUP_EXCLUDE_S = 20.0


def load() -> dict:
    """The whole `thresholds` map, key -> {value, unit, why, consumer}. {} on
    any read/parse failure — callers must tolerate an empty map."""
    try:
        with open(THRESHOLDS_PATH) as fh:
            doc = json.load(fh)
    except (OSError, ValueError):
        return {}
    thresholds = doc.get("thresholds")
    return thresholds if isinstance(thresholds, dict) else {}


def value(key: str, default: float | None = None) -> float | None:
    """The numeric `value` for one threshold key (e.g.
    "classifier.hitch_sd_ms"), or `default` when the key is missing or the
    file could not be read."""
    row = load().get(key)
    if not isinstance(row, dict) or "value" not in row:
        return default
    try:
        return float(row["value"])
    except (TypeError, ValueError):
        return default


def warmup_exclude_s() -> float:
    """`classifier.warmup_exclude_s` — seconds after RUNNING excluded from
    hitch detection and the encoder.fps floor (thresholds.json). This is the
    canonical warm-up window; bench_submit.py's `--warmup-secs` default reads
    it so a caller that never states one gets the same warm-up the session
    verdict itself uses, rather than "no split at all"."""
    return value("classifier.warmup_exclude_s", _FALLBACK_WARMUP_EXCLUDE_S)
