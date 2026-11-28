#!/usr/bin/env python3
"""Check one reference-recovery capture (#1348) from the node-agent test
`session::pipeline::ref_recovery::tests::capture_for_harness`.

    ref_recovery_check.py <dir> <h264|h265|av1>
    ref_recovery_check.py --self-test

The capture's "encoder" (vulkan or nvenc) picks the reference checks: the
Vulkan encoders' syntax is ours and checked exactly. NVENC's is the driver's,
so for it each recovery frame must name its anchor as its one reference
(H.264: long_term_pic_num; HEVC: list0 from the RPS and list_entry_l0), and no
HEVC RPS may list a lost picture, short- or long-term. A capture with
num_anchors 0 must carry no long-term syntax at all.

Drops the capture's lost frames and checks, with ffmpeg as the reference decoder:
  - the anchors, recovery frames and key frames are exactly what the capture
    script asked for (the capture's "expect" block), so an encoder that answers
    a recovery with a key frame, or keeps an anchor it should have forgotten,
    fails;
  - every frame after a loss is a key frame or a recovery frame whose anchor was kept;
  - the references each kept frame names are the pictures the encoder meant
    (AV1: VBI slots simulated from refresh_frame_flags; HEVC: no RPS entry is a lost
    picture; H.264: the recovery frame names the anchor's LongTermFrameIdx);
  - the full and the pruned stream decode without an error, and every kept frame is
    bit-identical to the full stream's decode;
  - the full decode stays close to the source, and no recovery frame is more than
    NEAR_DB below the frames before it (a picture coded from another reference
    than its header names decodes wrong without an error, and a near miss can
    still score above the absolute floor).
FFMPEG overrides the ffmpeg command (default: the linuxserver/ffmpeg image via docker).
Exit 0 on pass; every failure is printed.
"""
import json
import os
import re
import shlex
import statistics
import struct
import subprocess
import sys

PSNR_MIN = 20.0  # backstop: a picture decoded from a far-off reference
NEAR_DB = 6.0  # a recovery frame this far below the frames before it decoded wrong
EXT = {"h264": "h264", "h265": "hevc", "av1": "obu"}
FMT = {"h264": "h264", "h265": "hevc", "av1": "obu"}
FIELD = re.compile(r"\]\s+\d+\s+(\S+)\s+[01]+ = (-?\d+)")

fails = []


def check(ok, msg):
    if not ok:
        fails.append(msg)
    return ok


def ffmpeg(d, *args):
    base = os.environ.get("FFMPEG")
    if base:
        cmd = shlex.split(base)
    else:
        cmd = ["docker", "run", "--rm", "--user", f"{os.getuid()}:{os.getgid()}",
               "-v", f"{os.path.abspath(d)}:/w", "-w", "/w",
               "--entrypoint", "ffmpeg", "linuxserver/ffmpeg:latest"]
    return subprocess.run(cmd + ["-hide_banner", "-nostdin", *args],
                          capture_output=True, text=True, cwd=d)


def trace(d, name, codec):
    """Per packet: the list of (field, value) trace_headers prints."""
    r = ffmpeg(d, "-loglevel", "info", "-f", FMT[codec], "-i", name,
               "-c", "copy", "-bsf:v", "trace_headers", "-f", "null", "-")
    packets = []
    for line in r.stderr.splitlines():
        if "Packet:" in line:
            packets.append([])
        elif packets and (m := FIELD.search(line)):
            packets[-1].append((m.group(1), int(m.group(2))))
    return packets


def md5s(d, name, codec):
    dec = ["-c:v", "libdav1d"] if codec == "av1" else []
    r = ffmpeg(d, "-loglevel", "error", "-f", FMT[codec], *dec, "-i", name,
               "-fps_mode", "passthrough", "-f", "framemd5", "-")
    sums = [l.split(",")[-1].strip() for l in r.stdout.splitlines()
            if l and not l.startswith("#")]
    return sums, r.stderr.strip()


def first(fields, name, default=None):
    """The first value of @name, indexed (name[i]) or not."""
    return next((v for k, v in fields if k == name or k.startswith(name + "[")),
                default)


def all_of(fields, prefix):
    name = prefix.rstrip("[")  # ffmpeg prints some array fields unindexed
    return [v for k, v in fields if k == name or k.startswith(name + "[")]


def read_reports(reports, idx):
    """Anchors and {recovery: anchor} by frame index; @idx maps output pts to it."""
    by_frame = {}
    for r in reports:
        if check(r["pts"] in idx, f"report {r} names no output frame"):
            by_frame[idx[r["pts"]]] = r
    anchors = sorted(i for i, r in by_frame.items() if r["anchor"])
    recov = {}
    for i, r in by_frame.items():
        if r["recover_from"] is not None and check(
                r["recover_from"] in idx, f"frame {i} recovers from a pts no frame has: {r}"):
            recov[i] = idx[r["recover_from"]]
    return anchors, recov


def check_expect(expect, anchors, recov, keys):
    """The capture script's outcome, exactly (input frame indexes)."""
    want_recov = {a: b for a, b in expect["recoveries"]}
    check(anchors == expect["anchors"],
          f"anchors {anchors}, the script asked for {expect['anchors']}")
    check(recov == want_recov, f"recoveries {sorted(recov.items())}, the script "
                               f"asked for {sorted(want_recov.items())}")
    check(keys == expect["keys"],
          f"key frames {keys}, the script asked for {expect['keys']}")


def check_recovery_psnr(psnr, recov, keys):
    """Each recovery frame against the median of the four inter frames before it.
    Not the ones after: they code from the recovery frame and inherit its error."""
    for i in sorted(recov):
        before = [psnr[j] for j in range(i - 1, -1, -1)
                  if j not in recov and j not in keys][:4]
        if i < len(psnr) and before:
            med = statistics.median(before)
            check(psnr[i] >= med - NEAR_DB,
                  f"recovery frame {i} is {psnr[i]:.1f} dB luma PSNR, "
                  f"{med - psnr[i]:.1f} dB below the frames before it ({med:.1f})")


def swap(pk, i, name, value):
    """@pk with field @name of packet @i set to @value."""
    out = [list(f) for f in pk]
    out[i] = [(k, value if k == name else v) for k, v in out[i]]
    return out


def add(pk, i, fields):
    out = [list(f) for f in pk]
    out[i] = out[i] + fields
    return out


# NVENC-shaped packets: anchors 10 and 20 (H.264 long-term 0 and 1), frames 21-34
# lost, frame 35 recovers from 20.
H264 = [[("gaps_in_frame_num_allowed_flag", 0), ("num_ref_idx_l0_default_active_minus1", 0)]] + \
    [[] for _ in range(39)]
for _i, _lt in ((10, 0), (20, 1)):
    H264[_i] = [("memory_management_control_operation", 1),
                ("memory_management_control_operation", 6), ("long_term_frame_idx", _lt),
                ("memory_management_control_operation", 0)]
H264[35] = [("num_ref_idx_active_override_flag", 0), ("modification_of_pic_nums_idc", 2),
            ("long_term_pic_num", 1), ("modification_of_pic_nums_idc", 3)]
H265_FRAMES = [{"key": i == 0} for i in range(40)]
H265_LOST = set(range(21, 35))
H265_KEPT = [i for i in range(40) if i not in H265_LOST]
H265 = [[("num_negative_pics", 1), ("delta_poc_s0_minus1[0]", 0),
         ("used_by_curr_pic_s0_flag[0]", 1), ("num_ref_idx_l0_default_active_minus1", 3)]] + \
    [[("slice_pic_order_cnt_lsb", i), ("short_term_ref_pic_set_sps_flag", 1)]
     for i in range(1, 40)]
H265[35] = [("slice_pic_order_cnt_lsb", 35), ("short_term_ref_pic_set_sps_flag", 0),
            ("num_negative_pics", 0), ("num_long_term_pics", 2),
            ("poc_lsb_lt[0]", 10), ("used_by_curr_pic_lt_flag[0]", 1),
            ("poc_lsb_lt[1]", 20), ("used_by_curr_pic_lt_flag[1]", 1),
            ("num_ref_idx_active_override_flag", 1), ("num_ref_idx_l0_active_minus1", 0),
            ("ref_pic_list_modification_flag_l0", 1), ("list_entry_l0[0]", 1)]


def self_test():
    """The checks that judge the encoder, on streams that must fail them."""
    global fails
    expect = {"anchors": [10, 20], "recoveries": [[35, 20]], "keys": [0, 85]}
    cases = [
        ("honest", lambda: check_expect(expect, [10, 20], {35: 20}, [0, 85]), 0),
        # answers the recovery with a key frame, and says so
        ("always key frame", lambda: check_expect(expect, [10, 20], {}, [0, 35, 85]), 2),
        # recovers from an anchor it should have forgotten instead of a key frame
        ("kept a forgotten anchor",
         lambda: check_expect(expect, [10, 20], {35: 20, 85: 10}, [0]), 2),
        ("recovery close to its neighbours",
         lambda: check_recovery_psnr([38.0] * 34 + [36.5] + [38.0] * 5, {34: 20}, [0]), 0),
        # wrong but close: above the 20 dB floor, 14 dB below the frames before
        ("wrong-reference recovery",
         lambda: check_recovery_psnr([38.0] * 34 + [24.0] + [25.0] * 5, {34: 20}, [0]), 1),
        # NVENC: each recovery frame must name the anchor the agent asked for
        ("nvenc h264 named anchor", lambda: check_h264(H264, {35: 20}, [10, 20], True), 0),
        ("nvenc h264 wrong held anchor",
         lambda: check_h264(swap(H264, 35, "long_term_pic_num", 0), {35: 20}, [10, 20], True), 1),
        ("nvenc h264 two active references",
         lambda: check_h264(add(swap(H264, 35, "num_ref_idx_active_override_flag", 1), 35,
                                [("num_ref_idx_l0_active_minus1", 1)]), {35: 20}, [10, 20], True),
         1),
        ("nvenc hevc named anchor", lambda: check_h265(H265, H265_FRAMES, H265_KEPT,
                                                       H265_LOST, {35: 20}, True), 0),
        ("nvenc hevc wrong held anchor",
         lambda: check_h265(swap(H265, 35, "list_entry_l0[0]", 0), H265_FRAMES, H265_KEPT,
                            H265_LOST, {35: 20}, True), 1),
        # a forgotten anchor that was lost, still listed long-term after the recovery
        ("nvenc hevc lost long-term picture listed",
         lambda: check_h265(add(H265, 36, [("num_long_term_pics", 1), ("poc_lsb_lt[0]", 30),
                                           ("used_by_curr_pic_lt_flag[0]", 0)]),
                            H265_FRAMES, H265_KEPT, H265_LOST, {35: 20}, True), 1),
        ("num-anchors 0 with long-term syntax", lambda: check_no_long_term(H264, "h264"), 1),
        # input pts instead of output pts: a FAIL line each, not a KeyError
        ("reports name no output frame",
         lambda: read_reports([{"pts": 7, "anchor": True, "recover_from": None},
                               {"pts": 20, "anchor": False, "recover_from": 9}],
                              {10: 0, 20: 1}), 2),
    ]
    bad = 0
    for name, run, want in cases:
        fails = []
        run()
        ok = len(fails) == want
        bad += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {name}: {len(fails)} failure(s), want {want}"
              + "".join(f"\n       {f}" for f in fails))
    return 1 if bad else 0


def main(d, codec):
    meta = json.load(open(os.path.join(d, f"{codec}.json")))
    raw = open(os.path.join(d, f"{codec}.bin"), "rb").read()
    aus, off = [], 0
    while off < len(raw):
        (n,) = struct.unpack_from("<I", raw, off)
        aus.append(raw[off + 4:off + 4 + n])
        off += 4 + n
    frames = meta["frames"]
    check(len(aus) == len(frames), f"{len(aus)} access units for {len(frames)} frames")
    lost = {i for a, b in meta["lost"] for i in range(a, b + 1)}
    kept = [i for i in range(len(aus)) if i not in lost]
    idx = {f["pts"]: i for i, f in enumerate(frames)}
    anchors, recov = read_reports(meta["reports"], idx)
    keys = [i for i, f in enumerate(frames) if f["key"]]
    print(f"{codec}: {len(frames)} frames, {len(lost)} lost, anchors {anchors}, "
          f"recoveries {sorted(recov.items())}, key frames {keys}")
    if check("expect" in meta, "the capture has no expect block"):
        check_expect(meta["expect"], anchors, recov, keys)

    full, pruned = f"full.{EXT[codec]}", f"pruned.{EXT[codec]}"
    open(os.path.join(d, full), "wb").write(b"".join(aus))
    open(os.path.join(d, pruned), "wb").write(b"".join(aus[i] for i in kept))

    # every resumption is a key frame or codes from a kept anchor
    for i in kept:
        if i - 1 in lost:
            ok = frames[i]["key"] or (i in recov and recov[i] not in lost and recov[i] < i)
            check(ok, f"frame {i} follows a loss but is neither a key frame nor a "
                      f"recovery from a kept anchor")
    for i, a in recov.items():
        check(a in anchors and a < i, f"frame {i} recovers from {a}, not an earlier anchor")
        check(not frames[i]["key"], f"recovery frame {i} is a key frame")

    pk = trace(d, full, codec)
    check(len(pk) == len(aus), f"trace_headers saw {len(pk)} packets for {len(aus)}")
    nvenc = meta.get("encoder", "vulkan") == "nvenc"
    if codec == "av1":
        if not nvenc:
            check_av1(pk, frames, kept, lost, recov, anchors)
    elif codec == "h264":
        check_h264(pk, recov, anchors, nvenc)
    else:
        check_h265(pk, frames, kept, lost, recov, nvenc)
    if meta.get("num_anchors") == 0 and codec != "av1":
        check_no_long_term(pk, codec)

    a, full_err = md5s(d, full, codec)
    b, err = md5s(d, pruned, codec)
    check(not full_err, f"full decode reported: {full_err[:400]}")
    check(len(a) == len(aus), f"full decode gave {len(a)} frames")
    check(len(b) == len(kept), f"pruned decode gave {len(b)} frames for {len(kept)} kept")
    check(not err, f"pruned decode reported: {err[:400]}")
    bad = [i for k, i in enumerate(kept) if k < len(b) and i < len(a) and a[i] != b[k]]
    check(not bad, f"kept frames differ from the full decode: {bad}")

    # decode to raw frames first and compare by index: the elementary stream's
    # synthetic timestamps make psnr drop and repeat frames, pairing each with
    # a neighbour of its source
    size = f"{meta['width']}x{meta['height']}"
    ffmpeg(d, "-loglevel", "error", "-f", FMT[codec],
           *(["-c:v", "libdav1d"] if codec == "av1" else []), "-i", full,
           "-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "nv12",
           "-y", "dec.nv12")
    raw_in = ["-f", "rawvideo", "-pix_fmt", "nv12", "-s", size, "-r", "60", "-i"]
    r = ffmpeg(d, "-loglevel", "error", *raw_in, "dec.nv12", *raw_in, f"{codec}.nv12",
               "-lavfi", "[0:v][1:v]psnr=stats_file=psnr.log", "-f", "null", "-")
    if os.path.exists(os.path.join(d, "dec.nv12")):
        os.remove(os.path.join(d, "dec.nv12"))
    psnr = []
    if os.path.exists(os.path.join(d, "psnr.log")):
        for line in open(os.path.join(d, "psnr.log")):
            m = re.search(r"psnr_y:(\S+)", line)
            psnr.append(99.0 if m.group(1) == "inf" else float(m.group(1)))
        os.remove(os.path.join(d, "psnr.log"))
    check(len(psnr) == len(aus), f"psnr for {len(psnr)} frames: {r.stderr[:300]}")
    low = [(i, round(p, 1)) for i, p in enumerate(psnr) if p < PSNR_MIN]
    check(not low, f"frames below {PSNR_MIN} dB luma PSNR against the source: {low}")
    check_recovery_psnr(psnr, recov, keys)
    if psnr:
        print(f"{codec}: luma PSNR min {min(psnr):.1f} dB over {len(psnr)} frames")

    for f in fails:
        print(f"FAIL {codec}: {f}")
    print(f"{codec}: {'PASS' if not fails else 'FAIL'}")
    return 0 if not fails else 1


def check_av1(pk, frames, kept, lost, recov, anchors):
    def simulate(order):
        """slot -> frame index after each frame in @order; refs each frame names."""
        vbi, named = [None] * 8, {}
        for i in order:
            f = pk[i]
            if first(f, "frame_type") == 0:
                vbi = [i] * 8
                continue
            refs = all_of(f, "ref_frame_idx[")
            named[i] = [vbi[s] for s in refs]
            flags = first(f, "refresh_frame_flags", 0)
            for s in range(8):
                if flags & (1 << s):
                    vbi[s] = i
        return named

    full = simulate(range(len(pk)))
    seen = simulate(kept)
    for i in kept:
        if i not in full:
            continue
        check(seen[i] == full[i], f"frame {i} names {seen[i]} in the pruned stream, "
                                  f"{full[i]} in the full one")
        want = recov.get(i, i - 1)
        check(set(full[i]) == {want}, f"frame {i} names {sorted(set(full[i]))}, "
                                      f"expected only {want}")
    for i, a in recov.items():
        check(first(pk[i], "error_resilient_mode") == 1,
              f"recovery frame {i} is not error resilient")
    for i in anchors:
        flags = first(pk[i], "refresh_frame_flags", 0)
        check(bin(flags).count("1") == 2, f"anchor {i} refreshes {flags:#04x}")


def active_l0(f, ps):
    """Active list-0 references of a slice; @ps holds the PPS."""
    if first(f, "num_ref_idx_active_override_flag", 0):
        return first(f, "num_ref_idx_l0_active_minus1", 0) + 1
    return first(ps, "num_ref_idx_l0_default_active_minus1", 0) + 1


def check_h264(pk, recov, anchors, nvenc=False):
    if nvenc:
        # NVENC has no setting for it; the agent loop must not rely on it
        print(f"h264: note: SPS gaps_in_frame_num_allowed_flag="
              f"{first(pk[0], 'gaps_in_frame_num_allowed_flag')}")
        lt_idx = {}
        for i in anchors:
            ops = all_of(pk[i], "memory_management_control_operation[")
            check(6 in ops, f"anchor {i} is not marked long-term: {ops}")
            lt_idx[i] = first(pk[i], "long_term_frame_idx")
        for i, a in recov.items():
            idc = all_of(pk[i], "modification_of_pic_nums_idc[")
            check(idc[:1] == [2] and first(pk[i], "long_term_pic_num") == lt_idx.get(a),
                  f"recovery frame {i} names {idc} long-term "
                  f"{first(pk[i], 'long_term_pic_num')}, anchor {a} is {lt_idx.get(a)}")
            check(active_l0(pk[i], pk[0]) == 1,
                  f"recovery frame {i} has {active_l0(pk[i], pk[0])} active references")
        return
    # 1 makes a decoder invent frames for the lost run, and Mesa VA-API then fails the recovery frame
    check(first(pk[0], "gaps_in_frame_num_allowed_flag") == 0,
          "SPS allows frame_num gaps")
    lt_idx = {}
    for i in anchors:
        ops = all_of(pk[i], "memory_management_control_operation[")
        check(ops[:4] == [4, 1, 6, 0], f"anchor {i} marking is {ops}")
        lt_idx[i] = first(pk[i], "long_term_frame_idx")
    for i, a in recov.items():
        idc = all_of(pk[i], "modification_of_pic_nums_idc[")
        check(idc[:2] == [2, 3], f"recovery frame {i} list modification is {idc}")
        check(first(pk[i], "long_term_pic_num") == lt_idx.get(a),
              f"recovery frame {i} names long-term {first(pk[i], 'long_term_pic_num')}, "
              f"anchor {a} is {lt_idx.get(a)}")
        check(first(pk[i], "num_ref_idx_l0_active_minus1", 0) == 0,
              f"recovery frame {i} has more than one active reference")


def h265_rps(f, sps):
    """A slice's short-term RPS [(delta_poc_s0_minus1, used)] (its own, or the SPS
    set it names: NVENC's SPS carries one) and long-term [(poc_lsb_lt, used)]."""
    src = sps if first(f, "short_term_ref_pic_set_sps_flag", 0) else f
    n = first(src, "num_negative_pics", 0)
    st = list(zip(all_of(src, "delta_poc_s0_minus1[")[:n],
                  all_of(src, "used_by_curr_pic_s0_flag[")[:n]))
    lt = list(zip(all_of(f, "poc_lsb_lt["), all_of(f, "used_by_curr_pic_lt_flag[")))
    return st, lt


def h265_list0(f, ps, cur, st, lt):
    """The POCs in a P slice's list 0 (H.265 8.3.4); POCs are their LSBs here."""
    before, p = [], cur
    for dlt, used in st:
        p -= dlt + 1
        if used:
            before.append(p)
    temp = before + [lsb for lsb, used in lt if used]
    n = active_l0(f, ps)
    if not temp:
        return []
    cyc = [temp[k % len(temp)] for k in range(max(n, len(temp)))]
    if first(f, "ref_pic_list_modification_flag_l0", 0):
        return [cyc[e] for e in all_of(f, "list_entry_l0[")[:n]]
    return cyc[:n]


def check_h265(pk, frames, kept, lost, recov, nvenc=False):
    # every key frame's POC is 0, and MaxPicOrderCntLsb covers the frames up to
    # the next one, so a POC is its LSB
    poc, keyof, poc_of, k = {}, {}, {}, 0
    for i, f in enumerate(pk):
        if frames[i]["key"]:
            k = i
        poc[i] = 0 if frames[i]["key"] else first(f, "slice_pic_order_cnt_lsb")
        keyof[i] = k
        check(i == k or poc[i] > poc[i - 1], f"POC of frame {i} wraps")
        poc_of[(k, poc[i])] = i
    for i in kept:
        if frames[i]["key"]:
            continue
        f = pk[i]
        st, lt = h265_rps(f, pk[0])
        used = [u for _, u in st]
        p, listed = poc[i], []
        for dlt, _ in st:
            p -= dlt + 1
            listed.append(poc_of.get((keyof[i], p)))
        lt_listed = [poc_of.get((keyof[i], lsb)) for lsb, _ in lt]
        check(None not in listed + lt_listed,
              f"frame {i} RPS lists a POC that is no frame: {listed} {lt_listed}")
        gone = [j for j in listed + lt_listed if j in lost]
        check(not gone, f"frame {i} RPS lists lost pictures {gone}")
        if nvenc:
            if i in recov:
                l0 = h265_list0(f, pk[0], poc[i], st, lt)
                check(l0 == [poc[recov[i]]],
                      f"recovery frame {i} codes from POCs {l0}, its anchor "
                      f"{recov[i]} is POC {poc[recov[i]]}")
            continue
        want = recov.get(i, i - 1)
        check(listed[:1] == [want] and used[:1] == [1] and not any(used[1:]),
              f"frame {i} uses {listed} {used}, expected only {want}")


def check_no_long_term(pk, codec):
    """num-anchors 0: the stream an unpatched encoder writes, no long-term syntax."""
    if codec == "h264":
        lt = [i for i, f in enumerate(pk) if all_of(f, "memory_management_control_operation[")
              or 2 in all_of(f, "modification_of_pic_nums_idc[")]
        check(not lt, f"num-anchors 0, yet frames {lt[:10]} carry long-term syntax")
    else:
        check(first(pk[0], "long_term_ref_pics_present_flag", 0) == 0,
              "num-anchors 0, yet the SPS has long_term_ref_pics_present_flag")


if __name__ == "__main__":
    sys.exit(self_test() if sys.argv[1:] == ["--self-test"]
             else main(sys.argv[1], sys.argv[2]))
