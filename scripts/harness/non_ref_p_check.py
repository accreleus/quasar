#!/usr/bin/env python3
"""Check one `non-ref-p` capture (#1322): an Annex-B H.264 stream from vulkanh264enc (or
nvh264enc) with non-ref-p on.

    non_ref_p_check.py <stream.h264>
    non_ref_p_check.py --self-test

Passes when every second P access unit has nal_ref_idc 0 and no key frame does, and the
stream with those units removed decodes (ffmpeg, FFMPEG as in ref_recovery_check.py) with no
error and every remaining frame bit-identical to the full stream's decode: nothing is coded
from a non-reference frame, so the sender may drop one without a key frame.
"""
import os
import pathlib
import re
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import ref_recovery_check as rc  # noqa: E402

START = re.compile(rb"\x00\x00\x01")


def access_units(data):
    """Annex-B bytes -> access units as lists of NAL byte strings (start code kept off).
    A new unit starts at an AUD, SPS or a slice with first_mb_in_slice 0 (high bit set)."""
    nals = []
    marks = [m.end() for m in START.finditer(data)]
    for a, b in zip(marks, marks[1:] + [len(data) + 3]):
        nals.append(data[a:b - 3].rstrip(b"\x00") if b <= len(data) else data[a:])
    units, cur, seen_slice = [], [], False
    for n in nals:
        t = n[0] & 0x1F
        new = t in (9, 7, 8) and seen_slice or (t in (1, 5) and seen_slice and n[1] & 0x80)
        if new:
            units.append(cur)
            cur, seen_slice = [], False
        cur.append(n)
        seen_slice |= t in (1, 5)
    return units + [cur] if cur else units


def slices(unit):
    return [n for n in unit if n[0] & 0x1F in (1, 5)]


def refs(unit):
    """(is_key, nal_ref_idc of the unit's slices)."""
    s = slices(unit)
    return any(n[0] & 0x1F == 5 for n in s), {(n[0] >> 5) & 3 for n in s}


def pattern_fails(units):
    out = []
    for i, u in enumerate(units):
        key, idc = refs(u)
        if key and 0 in idc:
            out.append(f"unit {i}: a key frame with nal_ref_idc 0")
    deltas = [(i, refs(u)[1]) for i, u in enumerate(units) if not refs(u)[0]]
    non_ref = [i for i, idc in deltas if idc == {0}]
    if len(non_ref) < (len(deltas) - 2) // 2:
        out.append(f"{len(non_ref)} of {len(deltas)} P units are non-reference, want about half")
    for (i, a), (j, b) in zip(deltas, deltas[1:]):
        if a == {0} and b == {0}:
            out.append(f"units {i} and {j}: two non-reference P in a row")
    return out, non_ref


def main(path):
    d = tempfile.mkdtemp()
    data = pathlib.Path(path).read_bytes()
    units = access_units(data)
    bad, non_ref = pattern_fails(units)
    rc.check(not bad, "; ".join(bad))
    open(os.path.join(d, "full.h264"), "wb").write(data)
    pruned = [u for i, u in enumerate(units) if i not in set(non_ref)]
    open(os.path.join(d, "pruned.h264"), "wb").write(
        b"".join(b"\x00\x00\x00\x01" + n for u in pruned for n in u))
    full, err_full = rc.md5s(d, "full.h264", "h264")
    cut, err_cut = rc.md5s(d, "pruned.h264", "h264")
    rc.check(not err_full, f"full stream decode: {err_full}")
    rc.check(not err_cut, f"pruned stream decode: {err_cut}")
    rc.check(len(full) == len(units), f"full decode {len(full)} frames, stream has {len(units)}")
    keep = [m for i, m in enumerate(full) if i not in set(non_ref)]
    rc.check(cut == keep, "a frame after a removed non-reference frame decodes differently")
    print(f"{len(units)} units, {len(non_ref)} non-reference, pruned decode {len(cut)} frames")
    for f in rc.fails:
        print("FAIL", f)
    return 1 if rc.fails else 0


def need(ok, what=""):
    if not ok:
        raise SystemExit(f"self-test failed: {what}")


def au(*slice_headers, sps=False):
    out = [b"\x67\x42\xc0\x1f", b"\x68\xce"] if sps else []
    return out + [bytes([h]) + b"\x88\x84" for h in slice_headers]


def self_test():
    units = [au(0x65, sps=True), au(0x41), au(0x01), au(0x41), au(0x01), au(0x41)]
    bad, non_ref = pattern_fails(units)
    need(not bad and non_ref == [2, 4], (bad, non_ref))
    bad, _ = pattern_fails([au(0x65), au(0x01), au(0x01), au(0x41)])
    need(any("in a row" in b for b in bad), bad)
    bad, _ = pattern_fails([au(0x05), au(0x41), au(0x41), au(0x41), au(0x41), au(0x41)])
    need(any("key frame" in b for b in bad) and any("want about half" in b for b in bad), bad)
    raw = b"".join(b"\x00\x00\x00\x01" + n for u in units for n in u)
    need(len(access_units(raw)) == len(units), len(access_units(raw)))
    print("self-test ok")
    return 0


if __name__ == "__main__":
    sys.exit(self_test() if sys.argv[1:] == ["--self-test"] else main(sys.argv[1]))
