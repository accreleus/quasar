#!/usr/bin/env python3
"""Plain PSNR between two 8-bit 4:2:0 y4m files: mean over frames of Y, Cb, Cr
and the 6:1:1 weighted YCbCr PSNR. Usage: psnr_y4m.py ref.y4m dist.y4m"""
import sys
import numpy as np


def frames(path):
    with open(path, "rb") as f:
        hdr = f.readline().decode().split()
        w = int(next(t[1:] for t in hdr if t.startswith("W")))
        h = int(next(t[1:] for t in hdr if t.startswith("H")))
        ys, cs = w * h, (w // 2) * (h // 2)
        while True:
            line = f.readline()
            if not line:
                return
            assert line.startswith(b"FRAME"), line
            buf = f.read(ys + 2 * cs)
            a = np.frombuffer(buf, np.uint8)
            yield a[:ys].astype(np.float64), a[ys:ys + cs].astype(np.float64), a[ys + cs:].astype(np.float64)


def psnr(a, b):
    mse = np.mean((a - b) ** 2)
    return 99.0 if mse == 0 else 10 * np.log10(255.0 ** 2 / mse)


res = []
for fa, fb in zip(frames(sys.argv[1]), frames(sys.argv[2])):
    p = [psnr(x, y) for x, y in zip(fa, fb)]
    res.append(p + [(6 * p[0] + p[1] + p[2]) / 8])
r = np.mean(res, 0)
print(f"frames={len(res)} Y={r[0]:.2f} Cb={r[1]:.2f} Cr={r[2]:.2f} YCbCr611={r[3]:.2f}")
