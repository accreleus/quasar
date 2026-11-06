#!/usr/bin/env python3
"""Pack frame 0 of a .pwv1 and of its reference-decoded .y4m into a fixture .js
that the page loads with a <script> tag (works from file://, no fetch).
Usage: make_fixture.py NAME in.pwv1 ref_dec.y4m out.js"""
import base64, gzip, struct, sys

name, pwv1, y4m, out = sys.argv[1:5]
d = open(pwv1, "rb").read()
n = struct.unpack_from("<I", d, 24)[0]
one = d[:24 + 4 + n]                          # header + first frame only
y = open(y4m, "rb").read()
hdr_end = y.index(b"\n") + 1
fr_end = y.index(b"\n", hdr_end) + 1
w = int([t for t in y[:hdr_end].split() if t.startswith(b"W")][0][1:])
h = int([t for t in y[:hdr_end].split() if t.startswith(b"H")][0][1:])
ref = y[:fr_end + w * h + 2 * (w // 2) * (h // 2)]   # header + first frame
js = ("window.B00_FIXTURES = window.B00_FIXTURES || {};\n"
      f"window.B00_FIXTURES[{name!r}] = {{ w: {w}, h: {h}, frameBytes: {n},\n"
      f"  pwv1: '{base64.b64encode(one).decode()}',\n"
      f"  refY4mGz: '{base64.b64encode(gzip.compress(ref, 9, mtime=0)).decode()}' }};\n")
open(out, "w").write(js)
print(out, len(js))
