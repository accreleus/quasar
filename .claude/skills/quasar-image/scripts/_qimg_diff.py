#!/usr/bin/env python3
"""Diff two image snapshots for a re-pin. Pure: no host access.

The element delta is the point of this tool -- "the new GStreamer quietly dropped
an element the pipeline calls ElementFactory::make on" has no build-time signal
today and surfaces at session launch. An element assertion can regress two
different ways: its status can flip PASS -> FAIL (visible in `newly_failing`),
or the assertion for it can vanish entirely from the after snapshot's
`assertions` list (e.g. the contract stopped even checking it) -- that second
case would NOT show up in `newly_failing` (there is nothing in `after` to
compare against), so `elements_lost` is derived independently, from the set of
PASSing `gst.`/`gst-gpu.` element ids in each snapshot, not from the
newly_failing computation. Either kind of regression exits 1.

Inputs are two documents shaped like a single role entry from
`_qimg_collect.py` / `_qimg_repin.sh`: {"size_mb": int, "labels": {...},
"contract": {"assertions": [{"status","id","detail"}, ...], ...}}. A missing
or null `contract`/`labels` key degrades to empty, never raises -- an image
built before this tooling existed, or a completely failed snapshot, is a
normal degraded input, not a crash.

Optional `--deep` support (Task 6 FIX 4): a snapshot MAY additionally carry
`"deep_elements": [name, ...]` -- the full `gst-inspect-1.0` element
inventory `_qimg_repin.sh` collects only when invoked with `--deep`. When
BOTH snapshots carry that key it is unioned into the same
elements_lost/elements_gained sets the contract-derived `elements()` produces
(catching elements the contract doesn't name at all), rather than growing a
parallel set of output fields. When either snapshot lacks the key (the
default, non-`--deep` path), behaviour is unchanged from before this
existed.
"""
import argparse, json, sys


def status_map(snap):
    """id -> status for every assertion in a snapshot, tolerating malformed
    entries (non-dict, missing "id"/"status") by skipping them rather than
    raising."""
    out = {}
    for a in (snap.get("contract") or {}).get("assertions") or []:
        if not isinstance(a, dict):
            continue
        aid = a.get("id")
        if aid is None:
            continue
        out[aid] = a.get("status")
    return out


def elements(snap):
    """The set of GStreamer element names this snapshot's contract confirms
    are registered (a PASSing gst.<name> or gst-gpu.<name> assertion)."""
    out = set()
    for a in (snap.get("contract") or {}).get("assertions") or []:
        if not isinstance(a, dict):
            continue
        aid = a.get("id")
        if not isinstance(aid, str) or a.get("status") != "PASS":
            continue
        if aid.startswith("gst.") or aid.startswith("gst-gpu."):
            out.add(aid.split(".", 1)[1])
    return out


def deep_elements(snap):
    """The `--deep` full gst-inspect-1.0 element inventory, if this snapshot
    carries one. Returns None (not a crash, not an empty set) when the key is
    absent -- callers must be able to tell "no deep data collected" apart
    from "deep data collected, and it was empty", since only the former
    means "fall back to the contract-only element set"."""
    val = snap.get("deep_elements")
    if isinstance(val, list):
        return set(x for x in val if isinstance(x, str))
    return None


def size_delta_mb(before, after):
    b, a = before.get("size_mb"), after.get("size_mb")
    if isinstance(b, (int, float)) and isinstance(a, (int, float)):
        return a - b
    return None


def pin_deltas(before, after):
    lb = before.get("labels") or {}
    la = after.get("labels") or {}
    if not isinstance(lb, dict):
        lb = {}
    if not isinstance(la, dict):
        la = {}
    out = {}
    for k in sorted(set(lb) | set(la)):
        if k.startswith("org.quasar.pins.") and lb.get(k) != la.get(k):
            out[k] = [lb.get(k), la.get(k)]
    return out


def diff(before, after):
    sb, sa = status_map(before), status_map(after)
    newly_failing = sorted(k for k, v in sa.items()
                            if v == "FAIL" and sb.get(k) == "PASS")
    newly_passing = sorted(k for k, v in sa.items()
                            if v == "PASS" and sb.get(k) == "FAIL")
    eb, ea = elements(before), elements(after)
    db, da = deep_elements(before), deep_elements(after)
    if db is not None and da is not None:
        # Widen both sides with the full inventory only when BOTH snapshots
        # collected one -- a one-sided --deep run (e.g. an operator changed
        # the flag mid-investigation) has no "before" baseline to compare
        # the extra elements against, so it must not manufacture a false
        # elements_lost/gained from an asymmetric comparison.
        eb = eb | db
        ea = ea | da
    return {
        "size_delta_mb": size_delta_mb(before, after),
        "newly_failing": newly_failing,
        "newly_passing": newly_passing,
        "elements_lost": sorted(eb - ea),
        "elements_gained": sorted(ea - eb),
        "pin_deltas": pin_deltas(before, after),
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--before", required=True)
    ap.add_argument("--after", required=True)
    args = ap.parse_args()
    with open(args.before) as f:
        before = json.load(f)
    with open(args.after) as f:
        after = json.load(f)

    out = diff(before, after)
    json.dump(out, sys.stdout, indent=2)
    print()
    sys.exit(1 if (out["newly_failing"] or out["elements_lost"]) else 0)


if __name__ == "__main__":
    main()
