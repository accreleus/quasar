#!/usr/bin/env python3
"""Synthetic game-like test frames for the PyroWave spike (#1049).

Renders one high-detail RGB frame per seed at 3840x2160 (terrain noise, fine
foliage texture, saturated hard-edged shapes, HUD text, thin lines, a smooth
sky gradient), Lanczos-downscales it to 2560x1440 and 1920x1080, converts to
BT.709 limited-range YCbCr 4:2:0 and writes one y4m per resolution.
Usage: gen_frames.py OUTDIR [frames]
"""
import sys, os
import numpy as np
from PIL import Image, ImageDraw, ImageFont

W, H = 3840, 2160


def value_noise(h, w, cell, rng):
    gh, gw = h // cell + 2, w // cell + 2
    g = rng.random((gh, gw)).astype(np.float32)
    y = np.arange(h, dtype=np.float32) / cell
    x = np.arange(w, dtype=np.float32) / cell
    y0, x0 = y.astype(int), x.astype(int)
    fy, fx = y - y0, x - x0
    fy = fy * fy * (3 - 2 * fy)
    fx = fx * fx * (3 - 2 * fx)
    a = g[y0][:, x0]; b = g[y0][:, x0 + 1]
    c = g[y0 + 1][:, x0]; d = g[y0 + 1][:, x0 + 1]
    top = a + (b - a) * fx[None, :]
    bot = c + (d - c) * fx[None, :]
    return top + (bot - top) * fy[:, None]


def fbm(h, w, base, octaves, rng):
    out = np.zeros((h, w), np.float32); amp = 1.0; tot = 0.0
    for o in range(octaves):
        cell = max(2, base >> o)
        out += amp * value_noise(h, w, cell, rng); tot += amp; amp *= 0.5
    return out / tot


def render(seed):
    rng = np.random.default_rng(seed)
    yy = np.linspace(0, 1, H, dtype=np.float32)[:, None]
    img = np.zeros((H, W, 3), np.float32)
    # sky gradient
    img[...] = np.stack([0.25 + 0.3 * yy, 0.45 + 0.3 * yy, 0.85 - 0.2 * yy], -1) * np.ones((1, W, 1), np.float32)
    # mountains: ridge line from 1D fbm
    ridge = fbm(1, W, 512, 7, rng)[0] * 0.35 + 0.25
    rock = fbm(H, W, 256, 8, rng)
    mask = yy > ridge[None, :]
    rockc = np.stack([0.35 + 0.4 * rock, 0.30 + 0.35 * rock, 0.28 + 0.3 * rock], -1)
    img = np.where(mask[..., None], rockc, img)
    # foliage band: high-frequency texture in the lower third
    fol = fbm(H, W, 16, 4, rng)
    fmask = yy > 0.68 + 0.04 * np.sin(np.arange(W)[None, :] / 90.0)
    folc = np.stack([0.10 + 0.25 * fol, 0.35 + 0.5 * fol, 0.08 + 0.15 * fol], -1)
    img = np.where(fmask[..., None], folc, img)
    pil = Image.fromarray((np.clip(img, 0, 1) * 255).astype(np.uint8))
    d = ImageDraw.Draw(pil)
    # saturated hard-edged shapes (chroma edges)
    for _ in range(40):
        x, y = int(rng.integers(0, W)), int(rng.integers(0, H))
        r = int(rng.integers(20, 160))
        col = tuple(int(v) for v in rng.choice([0, 255], 3))
        if rng.random() < 0.5:
            d.ellipse([x - r, y - r, x + r, y + r], fill=col)
        else:
            d.rectangle([x - r, y - r // 2, x + r, y + r // 2], fill=col, outline=(255, 255, 255), width=3)
    # thin lines and a fine grid (aliasing stress)
    for i in range(0, 600, 6):
        d.line([(2900 + i, 200), (2900 + i, 800)], fill=(240, 240, 240), width=1)
    for i in range(0, 400, 4):
        d.line([(200, 1500 + i), (900, 1500 + i)], fill=(20, 20, 20), width=1)
    for a in range(0, 180, 3):
        t = np.deg2rad(a)
        d.line([(1900, 1100), (1900 + 500 * np.cos(t), 1100 - 500 * np.sin(t))], fill=(255, 255, 0), width=1)
    # HUD text at several sizes
    for size, y0 in ((18, 60), (28, 110), (44, 170), (72, 250)):
        try:
            f = ImageFont.load_default(size=size)
        except TypeError:
            f = ImageFont.load_default()
        d.text((60, y0), f"HP 100/100  AMMO 42  {seed:04d}  The quick brown fox jumps over the lazy dog 0123456789",
               fill=(255, 255, 255), font=f, stroke_width=2, stroke_fill=(0, 0, 0))
    d.rectangle([60, 2000, 1260, 2060], outline=(255, 255, 255), width=4)
    d.rectangle([64, 2004, 64 + int(rng.integers(300, 1190)), 2056], fill=(220, 30, 30))
    return pil


def to_420(pil):
    rgb = np.asarray(pil).astype(np.float32) / 255.0
    r, g, b = rgb[..., 0], rgb[..., 1], rgb[..., 2]
    y = 0.2126 * r + 0.7152 * g + 0.0722 * b
    cb = (b - y) / 1.8556
    cr = (r - y) / 1.5748
    Y = np.clip(np.round(16 + 219 * y), 0, 255).astype(np.uint8)
    cb2 = cb.reshape(cb.shape[0] // 2, 2, cb.shape[1] // 2, 2).mean((1, 3))
    cr2 = cr.reshape(cr.shape[0] // 2, 2, cr.shape[1] // 2, 2).mean((1, 3))
    Cb = np.clip(np.round(128 + 224 * cb2), 0, 255).astype(np.uint8)
    Cr = np.clip(np.round(128 + 224 * cr2), 0, 255).astype(np.uint8)
    return Y, Cb, Cr


def main():
    out = sys.argv[1]; n = int(sys.argv[2]) if len(sys.argv) > 2 else 3
    os.makedirs(out, exist_ok=True)
    sizes = {"2160p": (3840, 2160), "1440p": (2560, 1440), "1080p": (1920, 1080)}
    files = {k: open(os.path.join(out, f"game_{k}.y4m"), "wb") for k in sizes}
    for k, (w, h) in sizes.items():
        files[k].write(f"YUV4MPEG2 W{w} H{h} F60:1 Ip A1:1 C420jpeg XCOLORRANGE=LIMITED\n".encode())
    for s in range(n):
        frame = render(1000 + s)
        if s == 0:
            frame.save(os.path.join(out, "game_2160p_frame0.png"))
        for k, (w, h) in sizes.items():
            im = frame if (w, h) == (W, H) else frame.resize((w, h), Image.LANCZOS)
            Y, Cb, Cr = to_420(im)
            files[k].write(b"FRAME\n" + Y.tobytes() + Cb.tobytes() + Cr.tobytes())
    for f in files.values():
        f.close()
    print("ok", out)


if __name__ == "__main__":
    main()
