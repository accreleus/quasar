import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ControlDecoder, ControlProtocolError, encodeControl, parseFecDatagram, parseVideoDatagram, serialAfter, type VideoFragment } from "./framing";
import { FrameAssembler, INITIAL_RTT_MS, KEY_REASK_MS, MAX_PENDING, MAX_PENDING_AGE_MS, RTT_SLACK_MS } from "./reassembly";
import {
  BACKLOG_MS,
  DecodeStage,
  HOLD_MS,
  MAX_DECODE_QUEUE,
  MAX_DECODE_QUEUE_HARD,
  MAX_DECODER_RESTARTS,
  av1CodecString,
  avcCodecString,
  codecString,
  hevcCodecString,
} from "./decodeStage";
import { containRect, createWebGpuPresenter, presentGeometry } from "./presenter";
import { avcC, bytes, FakeChunk, FakeDecoder, fakeFrame, flush, packetize, type FakeFrame, type UnitSpec } from "./testSupport";

beforeEach(() => {
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const frags = (u: UnitSpec) => packetize(u).map((d) => parseVideoDatagram(d)!);
const unitOf = (frameSeq: number, keyframe: boolean, n = 1, extra: Partial<UnitSpec> = {}) =>
  frags({ configId: 0, frameSeq, keyframe, data: bytes(n, frameSeq), ...extra });

// ── Framing ─────────────────────────────────────────────────────────────────

describe("framing", () => {
  it("reads the agent's 25-byte video header", () => {
    const [d] = packetize({ configId: 7, frameSeq: 0xfffe, keyframe: true, data: bytes(10, 1), captureUs: 0x11223344556677 });
    const f = parseVideoDatagram(d)!;
    expect(f).toMatchObject({ keyframe: true, discardable: false, configId: 7, frameSeq: 0xfffe, fragIndex: 0, fragCount: 1 });
    expect(f.captureUs).toBe(0x11223344556677);
    expect(Array.from(f.payload)).toEqual(Array.from(bytes(10, 1)));
  });

  it("drops another channel and a frag_index past frag_count", () => {
    const [d] = packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(4, 0) });
    expect(parseVideoDatagram(Uint8Array.from([0x02, ...d.subarray(1)]))).toBeNull();
    const bad = d.slice();
    new DataView(bad.buffer).setUint16(9, 1, true);
    expect(parseVideoDatagram(bad)).toBeNull();
  });

  it("splits control messages across chunks and refuses one over 64 KiB", () => {
    const wire = new Uint8Array([...encodeControl({ type: "a" }), ...encodeControl({ type: "b" })]);
    const dec = new ControlDecoder();
    expect(dec.push(wire.subarray(0, 6))).toEqual([]);
    expect(dec.push(wire.subarray(6)).map((m) => m.type)).toEqual(["a", "b"]);
    const huge = new Uint8Array(4);
    new DataView(huge.buffer).setUint32(0, 65_537, true);
    expect(() => new ControlDecoder().push(huge)).toThrow(ControlProtocolError);
  });

  it("compares sequence numbers with wrap-around", () => {
    expect(serialAfter(0, 0xffff, 16)).toBe(true);
    expect(serialAfter(0xffff, 0, 16)).toBe(false);
    expect(serialAfter(0, 255, 8)).toBe(true);
    expect(serialAfter(5, 5, 8)).toBe(false);
  });
});

// ── Reassembly ──────────────────────────────────────────────────────────────

describe("FrameAssembler", () => {
  const seqs = (r: { units: { frameSeq: number }[] }) => r.units.map((u) => u.frameSeq);
  const NONE = { units: [], nacks: [], needKeyframe: false };

  it("rebuilds a unit from fragments in any order", () => {
    const data = bytes(3000, 3);
    const f = frags({ configId: 0, frameSeq: 9, keyframe: true, data });
    expect(f).toHaveLength(3);
    const a = new FrameAssembler(() => 0);
    expect(a.push(f[2]).units).toEqual([]);
    expect(a.push(f[0]).units).toEqual([]);
    const { units } = a.push(f[1]);
    expect(seqs({ units })).toEqual([9]);
    expect(Array.from(units[0].data)).toEqual(Array.from(data));
  });

  describe("FEC, scheme xor (#1046)", () => {
    /** The repair datagram an agent sends for fragments `first + i * stride` of `f`. */
    const repairOf = (f: VideoFragment[], first: number, count: number, stride = 1) => {
      const group = Array.from({ length: count }, (_, i) => f[first + i * stride].payload);
      const payload = new Uint8Array(Math.max(...group.map((g) => g.length)));
      let lengthXor = 0;
      for (const g of group) {
        lengthXor ^= g.length;
        g.forEach((b, i) => (payload[i] ^= b));
      }
      const d = new Uint8Array(14 + payload.length);
      const v = new DataView(d.buffer);
      d[0] = 0x04;
      d[5] = 1;
      v.setUint16(6, f[0].frameSeq, true);
      v.setUint16(8, first, true);
      d[10] = count;
      d[11] = stride;
      v.setUint16(12, lengthXor, true);
      d.set(payload, 14);
      return d;
    };
    // Five fragments; the last is shorter than the rest.
    const data = bytes(5000, 7);
    const five = () => frags({ configId: 0, frameSeq: 4, keyframe: true, data });
    const whole = (r: { units: { data: Uint8Array }[] }) => r.units.length === 1 && Array.from(r.units[0].data).join() === Array.from(data).join();

    it("rebuilds the one fragment its group lacks, byte for byte, and asks for nothing more", () => {
      const f = five();
      expect(f).toHaveLength(5);
      const a = new FrameAssembler(() => 0);
      for (const i of [0, 1, 3]) a.push(f[i]);
      // Fragment 2 was NACKed when fragment 3 showed the gap (the repair comes after the
      // fragments it protects); once rebuilt, nothing is asked again.
      const r = a.repair(parseFecDatagram(repairOf(f, 0, 4))!);
      expect(r.nacks).toEqual([]);
      expect(whole(a.push(f[4]))).toBe(true);
      expect(a.repaired).toBe(1);
    });

    it("rebuilds the last, shorter fragment at its own length", () => {
      const f = five();
      const a = new FrameAssembler(() => 0);
      for (const i of [0, 1, 2, 3]) a.push(f[i]);
      expect(whole(a.repair(parseFecDatagram(repairOf(f, 2, 3))!))).toBe(true);
    });

    it("holds a repair that arrives while two of its group are missing, and uses it when one of them arrives", () => {
      const f = five();
      const a = new FrameAssembler(() => 0);
      for (const i of [0, 3, 4]) a.push(f[i]);
      expect(a.repair(parseFecDatagram(repairOf(f, 0, 4))!).units).toEqual([]);
      expect(a.repaired).toBe(0);
      expect(whole(a.push(f[1]))).toBe(true); // fragment 2 comes from the held repair
      expect(a.repaired).toBe(1);
    });

    it("repairs a burst of two adjacent fragments with two interleaved groups", () => {
      const f = five();
      const a = new FrameAssembler(() => 0);
      for (const i of [0, 3, 4]) a.push(f[i]);
      a.repair(parseFecDatagram(repairOf(f, 0, 2, 2))!); // protects 0 and 2
      expect(whole(a.repair(parseFecDatagram(repairOf(f, 1, 2, 2))!))).toBe(true); // 1 and 3
      expect(a.repaired).toBe(2);
    });

    it("leaves a group with two missing to NACK", () => {
      const f = five();
      const a = new FrameAssembler(() => 0);
      for (const i of [0, 3, 4]) a.push(f[i]);
      const first = a.repair(parseFecDatagram(repairOf(f, 0, 4))!);
      // Asked once when fragment 3 arrived; nothing rebuilt, nothing released.
      expect(first.units).toEqual([]);
      expect(a.repaired).toBe(0);
    });

    it("drops a repair for a frame it is not waiting for, one past the frame's end, and one whose length does not fit", () => {
      const f = five();
      const a = new FrameAssembler(() => 0);
      // Nothing of the frame has arrived: there is no sibling header to give a rebuilt fragment.
      expect(a.repair(parseFecDatagram(repairOf(f, 0, 4))!)).toEqual(NONE);
      for (const i of [0, 1, 3, 4]) a.push(f[i]);
      const past = parseFecDatagram(repairOf(f, 0, 4))!;
      a.repair({ ...past, firstFrag: 3 }); // would cover fragments 3 … 6 of 5
      const bad = parseFecDatagram(repairOf(f, 0, 4))!;
      a.repair({ ...bad, lengthXor: bad.lengthXor ^ 0x4000 }); // a length the payload cannot hold
      expect(a.repaired).toBe(0);
      // The frame is still waiting, and a good repair still completes it.
      expect(whole(a.repair(parseFecDatagram(repairOf(f, 0, 4))!))).toBe(true);
    });

    it("refuses a repair datagram of another scheme, or with a count or stride the contract excludes", () => {
      const good = repairOf(five(), 0, 4);
      expect(parseFecDatagram(good)).toMatchObject({ frameSeq: 4, firstFrag: 0, count: 4, stride: 1 });
      const with_ = (at: number, v: number) => {
        const d = good.slice();
        d[at] = v;
        return parseFecDatagram(d);
      };
      expect(with_(5, 2)).toBeNull(); // unknown scheme
      expect(with_(10, 1)).toBeNull(); // count below 2
      expect(with_(10, 49)).toBeNull(); // count above 48
      expect(with_(11, 0)).toBeNull(); // stride 0
      expect(parseFecDatagram(good.subarray(0, 14))).toBeNull(); // no payload
      expect(with_(0, 0x01)).toBeNull(); // not the FEC channel
    });
  });

  it("carries on across the frame_seq wrap", () => {
    const a = new FrameAssembler(() => 0);
    const done = [0xfffe, 0xffff, 0, 1].flatMap((seq, i) => seqs(a.push(unitOf(seq, i === 0)[0])));
    expect(done).toEqual([0xfffe, 0xffff, 0, 1]);
  });

  it("NACKs a missing fragment, holds later frames behind it, and releases them in order on the retransmission", () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    expect(seqs(a.push(unitOf(1, true)[0]))).toEqual([1]);
    const two = unitOf(2, false, 3000); // three fragments; the middle one is lost
    a.push(two[0]);
    expect(a.push(two[2])).toEqual({ ...NONE, nacks: [{ frameSeq: 2, frags: [[1, 1]] }] });
    expect(a.lost).toBe(1);
    expect(a.push(unitOf(3, false)[0])).toEqual(NONE); // whole, but after the hole; asked once already
    t = 20;
    expect(seqs(a.push({ ...two[1], retransmit: true }))).toEqual([2, 3]);
    expect(a.rttMs).toBe(20);
    expect(a.lost).toBe(1); // counted once, when first found missing
  });

  it("NACKs fragment 0 of a frame lost whole, then the rest once its count is known", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(1, true)[0]);
    const two = unitOf(2, false, 2500);
    expect(a.push(unitOf(3, false)[0]).nacks).toEqual([{ frameSeq: 2, frags: [[0, 0]] }]);
    const r = a.push({ ...two[0], retransmit: true });
    expect(r.nacks).toEqual([{ frameSeq: 2, frags: [[1, 2]] }]);
    a.push({ ...two[1], retransmit: true });
    expect(seqs(a.push({ ...two[2], retransmit: true }))).toEqual([2, 3]);
  });

  it("repeats an unanswered NACK once after a round trip, then gives the frame up as late", () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    a.push(unitOf(1, true)[0]);
    const lost = unitOf(2, false, 2000); // two fragments; only the first arrives
    a.push(lost[0]);
    expect(a.push(unitOf(3, false)[0]).nacks).toEqual([{ frameSeq: 2, frags: [[1, 1]] }]);
    t = INITIAL_RTT_MS + RTT_SLACK_MS - 1;
    expect(a.push(unitOf(4, false)[0])).toEqual(NONE);
    t += 1;
    expect(a.push(unitOf(5, false)[0])).toEqual({ ...NONE, nacks: [{ frameSeq: 2, frags: [[1, 1]] }] });
    t += INITIAL_RTT_MS + RTT_SLACK_MS;
    expect(a.push(unitOf(6, false)[0])).toEqual({ ...NONE, needKeyframe: true });
    expect(a.push(lost[1]).units).toEqual([]); // late: older than the frame given up
    expect(a.push(unitOf(7, false)[0]).units).toEqual([]); // deltas wait for the keyframe
    expect(seqs(a.push(unitOf(8, true)[0]))).toEqual([8]);
    expect(seqs(a.push(unitOf(9, false)[0]))).toEqual([9]);
  });

  it("repeats a NACK at the measured round trip plus the slack, not the initial guess", () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    a.push(unitOf(1, true)[0]);
    const two = unitOf(2, false, 2000);
    a.push(two[1]);
    t = 20;
    a.push({ ...two[0], retransmit: true });
    expect(a.rttMs).toBe(20);
    const three = unitOf(3, false, 2000);
    t = 100;
    expect(a.push(three[1]).nacks).toEqual([{ frameSeq: 3, frags: [[0, 0]] }]);
    t = 100 + 20 + RTT_SLACK_MS - 1;
    expect(a.tick().nacks).toEqual([]);
    t += 1;
    expect(a.tick().nacks).toEqual([{ frameSeq: 3, frags: [[0, 0]] }]);
  });

  /** An assembler whose estimate is `rtt` ms (one loss, resent after `rtt`), at frame 3. */
  function measured(rtt: number) {
    const clock = { t: 0 };
    const a = new FrameAssembler(() => clock.t);
    a.push(unitOf(1, true)[0]);
    const two = unitOf(2, false, 2000);
    a.push(two[1]);
    clock.t = rtt;
    a.push({ ...two[0], retransmit: true });
    return { a, clock };
  }

  it("converges on a round trip that rises from 2 to 150 ms, at most doubling per sample", () => {
    const { a, clock } = measured(2);
    expect(a.rttMs).toBe(2);
    let seq = 3;
    const late: boolean[] = [];
    for (let i = 0; i < 12; i++) {
      const lossy = unitOf(seq++, false, 2000);
      const start = clock.t;
      const before = a.rttMs;
      a.push(lossy[1]);
      let wentLate = false;
      while (clock.t < start + 150) {
        clock.t += 1;
        if (a.tick().needKeyframe) wentLate = true;
      }
      const r = a.push({ ...lossy[0], retransmit: true }); // its first resend, 150 ms after the NACK
      late.push(wentLate);
      expect(a.rttMs).toBeLessThanOrEqual(2 * before);
      if (wentLate) a.push(unitOf(seq++, true)[0]);
      else expect(r.units.map((u) => u.frameSeq)).toEqual([lossy[0].frameSeq]);
      clock.t += 20;
    }
    // The first losses after the rise go late while the estimate doubles; then none do.
    const settled = late.indexOf(false);
    expect(late[0]).toBe(true);
    expect(settled).toBeGreaterThan(0);
    expect(settled).toBeLessThanOrEqual(7);
    expect(late.slice(settled)).not.toContain(true);
  });

  it("does not double the estimate when, in a burst, the first resend is lost and the repeat's arrives", () => {
    const { a, clock } = measured(20);
    const lossy = unitOf(3, false, 2000);
    clock.t = 100;
    a.push(lossy[1]);
    clock.t = 100 + 20 + RTT_SLACK_MS;
    expect(a.tick().nacks).toEqual([{ frameSeq: 3, frags: [[0, 0]] }]); // the repeat
    clock.t += 20; // the first resend was lost; the repeat's arrives one round trip later
    expect(seqs(a.push({ ...lossy[0], retransmit: true }))).toEqual([3]);
    expect(a.rttMs).toBe(20);
  });

  it("moves the estimate by at most 2x on a single 2 s outlier, and starts no higher than the age limit", () => {
    const { a, clock } = measured(20);
    const lossy = unitOf(3, false, 2000);
    clock.t = 100;
    a.push(lossy[1]);
    clock.t = 2_100;
    a.push({ ...lossy[0], retransmit: true });
    expect(a.rttMs).toBe(40);
    expect(measured(2_000).a.rttMs).toBe(MAX_PENDING_AGE_MS);
  });

  it(`asks for a keyframe once per wait, and again only after ${KEY_REASK_MS} ms without one`, () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    a.push(unitOf(1, true)[0]);
    a.push(unitOf(2, false, 2000)[0]);
    a.push(unitOf(3, false)[0]);
    expect(a.miss(2, [[1, 1]]).needKeyframe).toBe(true);
    a.push(unitOf(4, false, 2000)[0]);
    t = MAX_PENDING_AGE_MS + 1;
    expect(a.push(unitOf(5, false)[0]).needKeyframe).toBe(false); // frame 4 expired in the same wait
    t = KEY_REASK_MS - 1;
    expect(a.tick().needKeyframe).toBe(false);
    t = KEY_REASK_MS;
    expect(a.tick().needKeyframe).toBe(true);
    t += 1;
    expect(seqs(a.push(unitOf(6, true)[0]))).toEqual([6]);
    a.push(unitOf(7, false, 2000)[0]);
    a.push(unitOf(8, false)[0]);
    expect(a.miss(7, [[1, 1]]).needKeyframe).toBe(true); // a new wait asks at once
  });

  it("gives a frame up at once on a nack_miss for a fragment it lacks", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(1, true)[0]);
    const lost = unitOf(2, false, 2000);
    a.push(lost[0]);
    a.push(unitOf(3, false)[0]);
    expect(a.miss(2, [[0, 0]])).toEqual(NONE); // fragment 0 arrived: not late
    expect(a.miss(2, [[1, 1]])).toEqual({ ...NONE, needKeyframe: true });
  });

  it("loses a discardable frame without a keyframe, and releases the frames after it", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(1, true)[0]);
    a.push(unitOf(2, false, 2000, { discardable: true })[0]);
    expect(seqs(a.push(unitOf(3, false)[0]))).toEqual([]);
    expect(a.miss(2, [[1, 1]])).toEqual({ ...NONE, units: [expect.objectContaining({ frameSeq: 3 })] });
  });

  it("keeps a delta that completes behind a keyframe still arriving after a late frame", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(1, true)[0]);
    a.push(unitOf(2, false, 2000)[0]);
    a.push(unitOf(3, false)[0]);
    expect(a.miss(2, [[1, 1]]).needKeyframe).toBe(true);
    const key = unitOf(4, true, 2000);
    expect(a.push(key[0]).units).toEqual([]);
    expect(a.push(unitOf(5, false)[0]).nacks).toEqual([{ frameSeq: 4, frags: [[1, 1]] }]);
    expect(seqs(a.push(key[1]))).toEqual([4, 5]);
  });

  it(`holds at most ${MAX_PENDING} frames, dropping the oldest as lost`, () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(0, true)[0]);
    const halves = Array.from({ length: MAX_PENDING + 1 }, (_, i) => unitOf(1 + i, false, 2000));
    const verdicts = halves.map((h) => a.push(h[0]));
    expect(verdicts.slice(0, MAX_PENDING).every((v) => !v.needKeyframe)).toBe(true);
    expect(verdicts[MAX_PENDING].needKeyframe).toBe(true);
    expect(a.push(halves[0][1]).units).toEqual([]); // the oldest is gone: its second half completes nothing
  });

  it("refuses a frag_count no unit within the byte budget needs, without holding it", () => {
    const a = new FrameAssembler(() => 0);
    const [first] = unitOf(1, true, 2000);
    // Not discardable: the frame it claims is lost, so a keyframe is asked for.
    expect(a.push({ ...first, fragCount: 60_000 })).toEqual({ ...NONE, needKeyframe: true });
    // Held, the forgery's count would disagree with the real frame 1 and drop it.
    expect(seqs(a.push(unitOf(1, true)[0]))).toEqual([1]);
  });

  it("drops a frame whose fragments disagree on its header", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(1, true)[0]);
    const [p0, p1] = unitOf(2, false, 2000);
    a.push(p0);
    expect(a.push({ ...p1, ts90k: p1.ts90k + 1 })).toEqual({ ...NONE, needKeyframe: true });
    expect(a.push(p1).units).toEqual([]); // nothing of frame 2 is left to complete
  });

  it(`gives up an incomplete frame after ${MAX_PENDING_AGE_MS} ms with no fragment`, () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    a.push(unitOf(1, true)[0]);
    const [p0, p1] = unitOf(2, false, 2000);
    a.push(p0);
    t = MAX_PENDING_AGE_MS + 1;
    expect(a.push(p1)).toEqual({ ...NONE, needKeyframe: true }); // frame 2 expired; this half starts it anew
  });

  it("keeps a large frame whose fragments keep arriving, however long it takes", () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    const f = unitOf(1, true, 3000); // three fragments, 400 ms apart
    expect(a.push(f[0]).units).toEqual([]);
    t = 400;
    expect(a.push(f[1]).units).toEqual([]);
    t = 800;
    expect(seqs(a.push(f[2]))).toEqual([1]);
  });

  it("resyncs on a frame_seq jump instead of dropping everything as late", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(10, true)[0]);
    // 40000 is 25546 behind 10 in serial arithmetic: no fragment is that late.
    expect(seqs(a.push(unitOf(40_000, true)[0]))).toEqual([40_000]);
    expect(seqs(a.push(unitOf(40_001, false)[0]))).toEqual([40_001]);
  });

  it("resyncs on a keyframe after a long run of late fragments", () => {
    const a = new FrameAssembler(() => 0);
    a.push(unitOf(100, true)[0]);
    for (let i = 0; i < 40; i++) expect(a.push(unitOf(90, false)[0]).units).toEqual([]);
    expect(seqs(a.push(unitOf(95, true)[0]))).toEqual([95]);
  });
});

// ── Decode stage ────────────────────────────────────────────────────────────

describe("DecodeStage", () => {
  const config = (configId: number, description = avcC()) => ({ configId, codec: "h264", description, width: 1280, height: 720, fps: 60 });
  const unit = (configId: number, frameSeq: number, keyframe: boolean) => ({
    configId,
    frameSeq,
    keyframe,
    ts90k: 0,
    captureUs: 1_000_000 + frameSeq,
    data: bytes(8, frameSeq),
  });

  function stage(opts: { now?: () => number; refuseConfigure?: boolean } = {}) {
    const decoders: FakeDecoder[] = [];
    const frames: FakeFrame[] = [];
    const keyframes: string[] = [];
    const fatal: string[] = [];
    const s = new DecodeStage({
      onFrame: (f) => frames.push(f as unknown as FakeFrame),
      requestKeyframe: (r) => keyframes.push(r),
      onFatal: (r) => fatal.push(r),
      createDecoder: (init) => {
        const d = new FakeDecoder(init);
        d.refuseConfigure = opts.refuseConfigure ?? false;
        decoders.push(d);
        return d as unknown as VideoDecoder;
      },
      isConfigSupported: async (c) => ({ supported: true, config: c }),
      now: opts.now ?? (() => 0),
    });
    return { s, decoders, frames, keyframes, fatal };
  }

  it("names the H.264 codec from the avcC", () => {
    expect(avcCodecString(avcC(0x34))).toBe("avc1.42e034");
  });

  /** An hvcC record: profile space/tier/idc byte, compat flags, six constraint bytes, level. */
  const hvcC = (profileByte: number, compat: number, constraint0: number, level: number) => {
    const r = new Uint8Array(23);
    r[0] = 1;
    r[1] = profileByte;
    new DataView(r.buffer).setUint32(2, compat);
    r[6] = constraint0;
    r[12] = level;
    r[21] = 0x03;
    return r;
  };

  it("names the HEVC codec from the hvcC", () => {
    expect(hevcCodecString(hvcC(0x01, 0x60000000, 0xb0, 93))).toBe("hvc1.1.6.L93.B0");
    expect(hevcCodecString(hvcC(0x02, 0x20000000, 0xb0, 120))).toBe("hvc1.2.4.L120.B0");
    expect(hevcCodecString(hvcC(0x21, 0x60000000, 0, 153))).toBe("hvc1.1.6.H153");
    expect(codecString({ codec: "h265", description: hvcC(0x01, 0x60000000, 0xb0, 93) })).toBe("hvc1.1.6.L93.B0");
  });

  it("names the AV1 codec from the av1C, or the vetted Main 8-bit level 5.1 string without one", () => {
    expect(av1CodecString(Uint8Array.of(0x81, 0x08, 0x0c, 0))).toBe("av01.0.08M.08");
    expect(av1CodecString(Uint8Array.of(0x81, 0x4d, 0x6c, 0))).toBe("av01.2.13M.12");
    expect(av1CodecString(Uint8Array.of(0x81, 0x11, 0x4c, 0))).toBe("av01.0.17M.10");
    expect(av1CodecString(Uint8Array.of(0x81, 0x11, 0x8c, 0))).toBe("av01.0.17H.08");
    expect(codecString({ codec: "av1", description: new Uint8Array(0) })).toBe("av01.0.13M.08");
    expect(codecString({ codec: "h264", description: avcC(0x34) })).toBe("avc1.42e034");
    expect(codecString({ codec: "vp9", description: new Uint8Array(0) })).toBe("vp9");
  });

  it("configures the decoder with the negotiated codec's string, and no description for an AV1 without one", async () => {
    const hevc = stage();
    const description = hvcC(0x01, 0x60000000, 0xb0, 93);
    hevc.s.config({ ...config(0), codec: "h265", description });
    await flush();
    expect(hevc.decoders[0].configs[0]).toMatchObject({ codec: "hvc1.1.6.L93.B0", codedWidth: 1280 });
    expect(Array.from(hevc.decoders[0].configs[0].description as Uint8Array)).toEqual(Array.from(description));

    const av1 = stage();
    av1.s.config({ ...config(0), codec: "av1", description: new Uint8Array(0) });
    await flush();
    expect(av1.decoders[0].configs[0]).toMatchObject({ codec: "av01.0.13M.08" });
    expect("description" in av1.decoders[0].configs[0]).toBe(false);
    av1.s.unit(unit(0, 1, true));
    expect(av1.decoders[0].chunks).toHaveLength(1);
  });

  it("gives the decoder the signalled colour space (BT.601 or BT.709), and none when hello_ack has none", async () => {
    const bt601 = { primaries: "smpte170m", transfer: "smpte170m", matrix: "smpte170m", fullRange: false } as const;
    const a = stage();
    a.s.config({ ...config(0), colorSpace: bt601 });
    await flush();
    expect(a.decoders[0].configs[0].colorSpace).toEqual(bt601);

    const b = stage();
    b.s.config(config(0));
    await flush();
    // Absent: the decoder reads what the bitstream signals.
    expect("colorSpace" in b.decoders[0].configs[0]).toBe(false);
  });

  it("reconfigures the same decoder for a stream-resolution change and carries on at its keyframe (#1034)", async () => {
    const { s, decoders, keyframes, fatal } = stage();
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, true));
    // An ABR rung or the stream-size lever: a new config_id with a new size.
    s.config({ ...config(1), width: 1920, height: 1080 });
    await flush();
    s.unit(unit(1, 2, false)); // a delta of the new stream ahead of its keyframe is no use
    s.unit(unit(1, 3, true));
    s.unit(unit(1, 4, false));
    expect(decoders).toHaveLength(1);
    expect(decoders[0].configs.map((c) => [c.codedWidth, c.codedHeight])).toEqual([[1280, 720], [1920, 1080]]);
    expect(decoders[0].chunks.map((c) => c.type)).toEqual(["key", "key", "delta"]);
    expect(fatal).toEqual([]);
    expect(keyframes.length).toBeLessThanOrEqual(1);
  });

  it("drops a delta before the first keyframe and asks for one, once", async () => {
    const { s, decoders, keyframes } = stage();
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, false));
    s.unit(unit(0, 2, false));
    expect(decoders[0].chunks).toHaveLength(0);
    expect(keyframes).toEqual(["startup"]);
  });

  it("holds a unit for an unseen config_id until its hello_ack, then reconfigures", async () => {
    const { s, decoders, frames } = stage();
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, true));
    s.unit(unit(1, 2, true)); // overtook its hello_ack
    s.unit(unit(0, 3, false)); // an old-config straggler
    expect(decoders[0].chunks.map((c) => c.timestamp)).toEqual([1_000_001, 1_000_003]);
    const next = avcC(0x28);
    s.config(config(1, next));
    await flush();
    expect(decoders[0].configs).toHaveLength(2);
    expect(Array.from(decoders[0].configs[1].description as Uint8Array)).toEqual(Array.from(next));
    expect(decoders[0].configs[1].codec).toBe("avc1.42e028");
    expect(decoders[0].chunks.map((c) => c.timestamp)).toEqual([1_000_001, 1_000_003, 1_000_002]);
    s.unit(unit(0, 4, false)); // older than the current configuration: dropped
    expect(decoders[0].chunks).toHaveLength(3);
    expect(frames).toHaveLength(3);
  });

  it("gives up on held units after a second and asks for a keyframe", async () => {
    let t = 0;
    const { s, keyframes } = stage({ now: () => t });
    s.config(config(0));
    await flush();
    s.unit(unit(3, 1, true));
    t = HOLD_MS + 1;
    s.unit(unit(3, 2, false));
    expect(keyframes).toEqual(["decode_error"]);
  });

  it("reports a configure() that throws as unsupported, with no keyframe loop", async () => {
    const { s, keyframes, fatal } = stage({ refuseConfigure: true });
    s.config(config(0));
    await flush();
    for (let i = 0; i < 5; i++) s.unit(unit(0, i, i === 0));
    expect(fatal).toEqual(["decoder_unsupported"]);
    expect(keyframes).toEqual([]);
  });

  it("reports a configuration whose description cannot be read as unsupported", async () => {
    const { s, fatal } = stage();
    s.config({ ...config(0, new Uint8Array(3)), codec: "h265" });
    await flush();
    expect(fatal).toEqual(["decoder_unsupported"]);
  });

  it("does not skip after a short burst of decoder backlog", async () => {
    let t = 0;
    const { s, decoders, keyframes } = stage({ now: () => t });
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, true));
    decoders[0].decodeQueueSize = MAX_DECODE_QUEUE + 3;
    s.unit(unit(0, 2, false));
    t = BACKLOG_MS - 10;
    s.unit(unit(0, 3, false));
    decoders[0].decodeQueueSize = 0; // drained by itself
    t = BACKLOG_MS * 3;
    s.unit(unit(0, 4, false));
    expect(decoders[0].chunks.map((c) => c.type)).toEqual(["key", "delta", "delta", "delta"]);
    expect(keyframes).toEqual([]);
  });

  it(`skips to a keyframe after ${BACKLOG_MS} ms of backlog, once until that keyframe decodes`, async () => {
    let t = 0;
    const { s, decoders, keyframes } = stage({ now: () => t });
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, true));
    const d = decoders[0];
    d.holdOutput = true;
    d.decodeQueueSize = MAX_DECODE_QUEUE + 1;
    s.unit(unit(0, 2, false)); // backlog starts
    t = BACKLOG_MS + 1;
    s.unit(unit(0, 3, false)); // sustained: skip
    s.unit(unit(0, 4, false));
    expect(d.chunks.map((c) => c.type)).toEqual(["key", "delta"]);
    expect(keyframes).toEqual(["late"]);
    s.unit(unit(0, 5, true)); // the keyframe it asked for, still in the queue
    t = 200;
    s.unit(unit(0, 6, false)); // backlog again from here
    t = 200 + BACKLOG_MS + 10;
    s.unit(unit(0, 7, false)); // sustained, but that keyframe has not decoded: no second skip
    expect(keyframes).toEqual(["late"]);
    expect(d.chunks.map((c) => c.type)).toEqual(["key", "delta", "key", "delta", "delta"]);
    d.emit(); // the keyframe decodes: armed again
    t = 200 + BACKLOG_MS + 20;
    s.unit(unit(0, 8, false)); // the same backlog, still going: now it skips
    expect(keyframes).toEqual(["late", "late"]);
  });

  it(`never queues more than ${MAX_DECODE_QUEUE_HARD} frames in a decoder that never outputs`, async () => {
    const { s, decoders, keyframes } = stage(); // the clock stands still: only the hard cap can act
    s.config(config(0));
    await flush();
    const d = decoders[0];
    d.holdOutput = true;
    d.queueFollowsWaiting = true;
    let most = 0;
    for (let i = 0; i < 300; i++) {
      s.unit(unit(0, i, i % 30 === 0)); // a keyframe every 30 units
      most = Math.max(most, d.decodeQueueSize);
    }
    expect(most).toBe(MAX_DECODE_QUEUE_HARD);
    expect(d.chunks.length).toBe(MAX_DECODE_QUEUE_HARD);
    expect(keyframes.every((k) => k === "late")).toBe(true);
    expect(keyframes.length).toBeGreaterThan(0);
  });

  it("re-creates a failed decoder and resumes at a keyframe", async () => {
    const { s, decoders, keyframes, frames } = stage();
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, true));
    decoders[0].fail();
    await flush();
    expect(decoders).toHaveLength(2);
    expect(decoders[1].configs).toHaveLength(1);
    expect(keyframes).toEqual(["decode_error"]);
    s.unit(unit(0, 2, false)); // a delta cannot start the new decoder
    expect(keyframes).toEqual(["decode_error"]); // and asks for no second keyframe
    s.unit(unit(0, 3, true));
    expect(decoders[1].chunks.map((c) => c.type)).toEqual(["key"]);
    expect(frames).toHaveLength(2);
  });

  it(`goes terminal after ${MAX_DECODER_RESTARTS} failures in a row`, async () => {
    const { s, decoders, fatal } = stage();
    s.config(config(0));
    await flush();
    for (let i = 0; i <= MAX_DECODER_RESTARTS; i++) {
      decoders[decoders.length - 1].fail();
      await flush();
    }
    expect(fatal).toEqual(["decode_error"]);
    expect(decoders).toHaveLength(MAX_DECODER_RESTARTS + 1);
  });
});

// ── Presenter ───────────────────────────────────────────────────────────────

describe("createWebGpuPresenter", () => {
  function fakeGpu(importFails: boolean) {
    const calls: string[] = [];
    let lose!: (info: { reason: string; message: string }) => void;
    const listeners: Record<string, (e: unknown) => void> = {};
    const viewports: number[][] = [];
    const crops: number[][] = [];
    const samplers: string[] = [];
    const pass = { setPipeline() {}, setBindGroup() {}, setViewport: (...v: number[]) => void viewports.push(v.slice(0, 4)), draw() {}, end() {} };
    const device = {
      lost: new Promise((r) => (lose = r)),
      addEventListener: (type: string, fn: (e: unknown) => void) => void (listeners[type] = fn),
      queue: {
        submit: () => calls.push("submit"),
        copyExternalImageToTexture: () => calls.push("copy"),
        writeBuffer: (_b: unknown, _o: number, d: Float32Array) => void crops.push([...d]),
      },
      createBuffer: () => ({}),
      createShaderModule: () => ({}),
      createRenderPipeline: () => ({ getBindGroupLayout: () => ({}) }),
      createSampler: (d: { magFilter: string }) => d.magFilter,
      importExternalTexture: () => {
        calls.push("import");
        if (importFails) throw new Error("Failed to import texture from video");
        return {};
      },
      createTexture: (d: { size: number[] }) => ({ width: d.size[0], height: d.size[1], createView: () => ({}), destroy() {} }),
      createBindGroup: (d: { entries: { resource: unknown }[] }) => void samplers.push(d.entries[0].resource as string),
      createCommandEncoder: () => ({ beginRenderPass: () => pass, finish: () => ({}) }),
      destroy() {},
    };
    vi.stubGlobal("navigator", {
      gpu: { requestAdapter: async () => ({ requestDevice: async () => device }), getPreferredCanvasFormat: () => "bgra8unorm" },
    });
    vi.stubGlobal("GPUTextureUsage", { TEXTURE_BINDING: 4, COPY_DST: 2, RENDER_ATTACHMENT: 16 });
    vi.stubGlobal("GPUBufferUsage", { UNIFORM: 64, COPY_DST: 8 });
    vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const canvas = {
      width: 1600,
      height: 1000,
      dataset: {} as Record<string, string>,
      getContext: () => ({ configure() {}, getCurrentTexture: () => ({ createView: () => ({}) }) }),
    } as unknown as HTMLCanvasElement;
    return { calls, canvas, lose, viewports, crops, samplers, uncaptured: (message: string) => listeners.uncapturederror({ error: { message } }) };
  }

  it("falls back to copying each frame once an import fails, and closes every frame", async () => {
    const g = fakeGpu(true);
    const p = (await createWebGpuPresenter(g.canvas, () => {}))!;
    const [a, b] = [fakeFrame(), fakeFrame()];
    expect(p.present(a as unknown as VideoFrame)).toBe(true);
    expect(p.present(b as unknown as VideoFrame)).toBe(true);
    expect(g.calls).toEqual(["import", "copy", "submit", "copy", "submit"]);
    expect(a.closed && b.closed).toBe(true);
  });

  it("stops drawing and reports once when the device is lost", async () => {
    const g = fakeGpu(false);
    const fatal: string[] = [];
    const p = (await createWebGpuPresenter(g.canvas, (m) => fatal.push(m)))!;
    expect(p.present(fakeFrame() as unknown as VideoFrame)).toBe(true);
    g.lose({ reason: "unknown", message: "GPU reset" });
    await flush();
    const after = fakeFrame();
    expect(p.present(after as unknown as VideoFrame)).toBe(false);
    expect(after.closed).toBe(true);
    g.uncaptured("another");
    expect(fatal).toHaveLength(1);
    expect(fatal[0]).toMatch(/lost/);
  });

  it("reports an uncaptured WebGPU error and draws nothing after it", async () => {
    const g = fakeGpu(false);
    const fatal: string[] = [];
    const p = (await createWebGpuPresenter(g.canvas, (m) => fatal.push(m)))!;
    g.uncaptured("validation failed");
    expect(p.present(fakeFrame() as unknown as VideoFrame)).toBe(false);
    expect(fatal).toEqual(["WebGPU error: validation failed"]);
  });

  describe("the presenter's scaling modes (#1034)", () => {
    // A 16:9 frame in a 16:10 canvas, the fake's 1600x1000.
    const draw = async (mode?: string) => {
      const g = fakeGpu(false);
      if (mode) g.canvas.dataset.scalingMode = mode;
      const p = (await createWebGpuPresenter(g.canvas, () => {}))!;
      expect(p.present(fakeFrame() as unknown as VideoFrame)).toBe(true);
      return { viewport: g.viewports[0], crop: g.crops[0], sampler: g.samplers[0] };
    };

    it("contain letterboxes the whole frame, and is the default", async () => {
      const want = { viewport: [0, 50, 1600, 900], crop: [1, 1, 0, 0], sampler: "linear" };
      expect(await draw("contain")).toEqual(want);
      expect(await draw()).toEqual(want);
    });

    it("cover fills the canvas and crops the frame's sides evenly", async () => {
      const d = await draw("cover");
      expect(d.viewport).toEqual([0, 0, 1600, 1000]);
      expect(d.crop.map((v) => +v.toFixed(4))).toEqual([0.9, 1, 0.05, 0]);
    });

    it("stretch fills the canvas with the whole frame", async () => {
      expect(await draw("stretch")).toEqual({ viewport: [0, 0, 1600, 1000], crop: [1, 1, 0, 0], sampler: "linear" });
    });

    it("integer is contain with nearest sampling, as the <video> draws it", async () => {
      expect(await draw("integer")).toEqual({ viewport: [0, 50, 1600, 900], crop: [1, 1, 0, 0], sampler: "nearest" });
    });

    it("follows a mode change between two frames", async () => {
      const g = fakeGpu(false);
      const p = (await createWebGpuPresenter(g.canvas, () => {}))!;
      p.present(fakeFrame() as unknown as VideoFrame);
      g.canvas.dataset.scalingMode = "stretch";
      p.present(fakeFrame() as unknown as VideoFrame);
      expect(g.viewports).toEqual([[0, 50, 1600, 900], [0, 0, 1600, 1000]]);
    });

    it("crops top and bottom when the frame is taller than the canvas", () => {
      expect(presentGeometry("cover", 1000, 500, 1000, 1000).crop).toEqual([1, 0.5, 0, 0.25]);
    });
  });
});

describe("containRect", () => {
  it("letterboxes and pillarboxes", () => {
    expect(containRect(1920, 1200, 1920, 1080)).toEqual({ x: 0, y: 60, w: 1920, h: 1080 });
    expect(containRect(1000, 1000, 2000, 1000)).toEqual({ x: 0, y: 250, w: 1000, h: 500 });
  });
});
