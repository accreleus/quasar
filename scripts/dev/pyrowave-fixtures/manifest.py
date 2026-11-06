#!/usr/bin/env python3
"""Write manifest.json for the PyroWave golden fixtures (#1053). Stdlib only; runs in the image.

usage: manifest.py DIR PYROWAVE_REF GRANITE_REF   (DIR holds what pyrowave_fixtures wrote)

The manifest holds no timestamp, so regenerating the same bytes gives the same manifest.
"""
import hashlib, json, math, os, struct, sys

d, pyrowave_ref, granite_ref = sys.argv[1:4]
HDR = 28  # PWV1Header (24) + the u32 size of the one frame
HI_ROOM = 1111  # packet room of a 1200-byte datagram after the 25+12-byte headers (codec-pyrowave.md §Packing)


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def entry(name):
    p = os.path.join(d, name)
    return {"file": name, "bytes": os.path.getsize(p), "sha256": sha(p)}


def psnr(a, b):
    mse = sum((x - y) * (x - y) for x, y in zip(a, b)) / len(a)
    return 99.0 if mse == 0 else round(10 * math.log10(255 * 255 / mse), 2)  # 99.0 = identical


def walk(frame):
    """Split a frame into packets by the self-delimiting rule of bitstream.md / codec-pyrowave.md."""
    pos, sofs, blocks = 0, [], []
    while pos < len(frame):
        w0, w1 = struct.unpack_from("<II", frame, pos)
        if w0 >> 31:  # extended: the 8-byte start-of-frame header
            sofs.append((w0, w1)); pos += 8
        else:
            words = (w0 >> 16) & 0xFFF
            assert words >= 2, f"block with payload_words {words}"
            blocks.append({"index": w1 >> 8, "bytes": words * 4}); pos += words * 4
    assert pos == len(frame), "packets overrun the frame"
    return sofs, blocks


budgets = dict(l.split() for l in open(os.path.join(d, "budgets.tsv")))
names = sorted(f[: -len(".pwv1")] for f in os.listdir(d) if f.endswith(".pwv1"))
pictures, fixtures = {}, []
for n in names:
    pic, size, rate = n.split("_")
    w, h = map(int, size.split("x"))
    key = f"{pic}_{size}"
    src = open(os.path.join(d, f"{key}.src.yuv"), "rb").read()
    ref = open(os.path.join(d, f"{n}.ref.yuv"), "rb").read()
    cw, ch = w // 2, h // 2
    assert len(src) == len(ref) == w * h + 2 * cw * ch
    pictures.setdefault(key, {"picture": pic, "width": w, "height": h, "src": entry(f"{key}.src.yuv")})

    raw = open(os.path.join(d, f"{n}.pwv1"), "rb").read()
    magic, = struct.unpack_from("<I", raw, 0)
    assert magic == 0x31565750, "not a PWV1 file"
    fsize, = struct.unpack_from("<I", raw, 24)
    assert HDR + fsize == len(raw), "one frame, nothing after it"
    sofs, blocks = walk(raw[HDR:])
    assert len(sofs) == 1 and raw[HDR:HDR + 8] == struct.pack("<II", *sofs[0]), "one SOF, first"
    w0, w1 = sofs[0]
    assert (w0 & 0x3FFF) + 1 == w and ((w0 >> 14) & 0x3FFF) + 1 == h
    assert (w1 & 0xFFFFFF) == len(blocks), "SOF total_blocks counts the blocks"
    fixtures.append({
        "name": n, "picture": pic, "width": w, "height": h, "rate": rate,
        "budget_bytes": int(budgets[n]),
        "frame_bytes": fsize,
        "sof": {
            "sequence": (w0 >> 28) & 7, "total_blocks": w1 & 0xFFFFFF, "chroma_resolution": (w1 >> 26) & 1,
            "color_primaries": (w1 >> 27) & 1, "transfer_function": (w1 >> 28) & 1,
            "ycbcr_transform": (w1 >> 29) & 1, "ycbcr_range": (w1 >> 30) & 1, "chroma_siting": (w1 >> 31) & 1,
        },
        "blocks": len(blocks),
        "block_bytes_max": max((b["bytes"] for b in blocks), default=0),
        "blocks_over_1111": sum(b["bytes"] > HI_ROOM for b in blocks),
        "pwv1": entry(f"{n}.pwv1"), "ref": entry(f"{n}.ref.yuv"),
        "src": f"{key}.src.yuv",
        "psnr_ref_vs_src_db": {
            "y": psnr(src[: w * h], ref[: w * h]),
            "cb": psnr(src[w * h : w * h + cw * ch], ref[w * h : w * h + cw * ch]),
            "cr": psnr(src[w * h + cw * ch :], ref[w * h + cw * ch :]),
        },
    })

# Equivalent rate at 60 fps for orientation only (the budget is per frame).
for f in fixtures:
    f["budget_mbit_s_at_60fps"] = round(f["budget_bytes"] * 8 * 60 / 1e6, 1)

dev = json.load(open(os.path.join(d, "device.json")))
manifest = {
    "format": 1,
    "ticket": "accreleus/quasar#1053",
    "pin": {"pyrowave": pyrowave_ref, "granite": granite_ref},
    "bitstream": {
        "spec": "bitstream v1 (bitstream/bitstream.md at the PyroWave pin; protocol/codec-pyrowave.md)",
        "file_format": "PWV1: 24-byte PWV1Header, then one u32 frame size and that frame (SOF packet first, then 32x32 blocks)",
        "chroma": "4:2:0",
        "note": "Each frame is the library's own single packet (no datagram boundary). Datagram packing is codec-pyrowave.md §Packing and is not applied here.",
    },
    "picture_format": {
        "layout": "I420: Y (w*h bytes), Cb ((w/2)*(h/2)), Cr ((w/2)*(h/2)), 8-bit, planar, no header, no padding",
        "colour": "YCbCr authored as BT.709 limited range (Y 16-235, C 16-240); the codec itself is range-agnostic",
    },
    "reference_decode": "pyrowave_decoder_decode_cpu_buffer_synchronous, rounded to 8 bits by the library. The decode is NOT bit-exact across devices (bitstream.md): compare by PSNR/max error, never by hash, except to detect a changed fixture.",
    "psnr_note": "PSNR of the reference decode against the source picture, per plane, 8-bit, 99.0 = identical.",
    "encoder_device": dev,
    "encoder_note": "The encoder runs in Vulkan compute and needs a real GPU (subgroup-size control of wave16/32/64; lavapipe offers only 8). The files here are pinned as generated on this device; see README.md for whether other GPUs reproduce them.",
    "pictures": pictures,
    "fixtures": fixtures,
}
with open(os.path.join(d, "manifest.json"), "w") as f:
    json.dump(manifest, f, indent=2, sort_keys=False)
    f.write("\n")
