import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ControlDecoder, ControlProtocolError, encodeControl, parseVideoDatagram, serialAfter } from "./framing";
import { FrameAssembler, INITIAL_RTT_MS, KEY_REASK_MS, MAX_PENDING, MAX_PENDING_AGE_MS, RTT_SLACK_MS } from "./reassembly";
import {
  BACKLOG_MS,
  DecodeStage,
  HOLD_MS,
  MAX_DECODE_QUEUE,
  MAX_DECODE_QUEUE_HARD,
  MAX_DECODER_RESTARTS,
  avcCodecString,
} from "./decodeStage";
import { containRect, createWebGpuPresenter } from "./presenter";
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
    expect(a.push(unitOf(3, false)[0])).toEqual(NONE); // whole, but after the hole; asked once already
    t = 20;
    expect(seqs(a.push({ ...two[1], retransmit: true }))).toEqual([2, 3]);
    expect(a.rttMs).toBe(20);
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

  it("follows a round trip that rises from 2 to 150 ms, sampling a resend that lands after its frame was given up", () => {
    let t = 0;
    const a = new FrameAssembler(() => t);
    a.push(unitOf(1, true)[0]);
    const two = unitOf(2, false, 2000);
    a.push(two[1]);
    t = 2;
    expect(seqs(a.push({ ...two[0], retransmit: true }))).toEqual([2]);
    expect(a.rttMs).toBe(2);

    // The path slows to 150 ms: the first loss after it cannot be saved.
    const three = unitOf(3, false, 2000);
    t = 10;
    a.push(three[1]);
    t = 22;
    expect(a.push(unitOf(4, false)[0]).nacks).toEqual([{ frameSeq: 3, frags: [[0, 0]] }]);
    t = 34;
    expect(a.push(unitOf(5, false)[0]).needKeyframe).toBe(true);
    t = 160; // its resend, after the frame was given up, still measures the path
    a.push({ ...three[0], retransmit: true });
    expect(a.rttMs).toBe(150);

    // The next loss waits for its resend instead of going late.
    t = 170;
    expect(seqs(a.push(unitOf(6, true)[0]))).toEqual([6]);
    const seven = unitOf(7, false, 2000);
    a.push(seven[1]);
    t = 319;
    expect(a.push(unitOf(8, false)[0])).toEqual(NONE);
    t = 320;
    expect(a.push({ ...seven[0], retransmit: true })).toEqual({ ...NONE, units: [expect.objectContaining({ frameSeq: 7 }), expect.objectContaining({ frameSeq: 8 })] });

    // And repeats and lateness keep to it when the resend never comes.
    const nine = unitOf(9, false, 2000);
    t = 400;
    a.push(nine[1]);
    t = 400 + 150 + RTT_SLACK_MS - 1;
    expect(a.tick().nacks).toEqual([]);
    t += 1;
    expect(a.tick().nacks).toEqual([{ frameSeq: 9, frags: [[0, 0]] }]);
    t += 150 + RTT_SLACK_MS - 1;
    expect(a.tick().needKeyframe).toBe(false);
    t += 1;
    expect(a.tick().needKeyframe).toBe(true);
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
    const pass = { setPipeline() {}, setBindGroup() {}, setViewport() {}, draw() {}, end() {} };
    const device = {
      lost: new Promise((r) => (lose = r)),
      addEventListener: (type: string, fn: (e: unknown) => void) => void (listeners[type] = fn),
      queue: { submit: () => calls.push("submit"), copyExternalImageToTexture: () => calls.push("copy") },
      createShaderModule: () => ({}),
      createRenderPipeline: () => ({ getBindGroupLayout: () => ({}) }),
      createSampler: () => ({}),
      importExternalTexture: () => {
        calls.push("import");
        if (importFails) throw new Error("Failed to import texture from video");
        return {};
      },
      createTexture: (d: { size: number[] }) => ({ width: d.size[0], height: d.size[1], createView: () => ({}), destroy() {} }),
      createBindGroup: () => ({}),
      createCommandEncoder: () => ({ beginRenderPass: () => pass, finish: () => ({}) }),
      destroy() {},
    };
    vi.stubGlobal("navigator", {
      gpu: { requestAdapter: async () => ({ requestDevice: async () => device }), getPreferredCanvasFormat: () => "bgra8unorm" },
    });
    vi.stubGlobal("GPUTextureUsage", { TEXTURE_BINDING: 4, COPY_DST: 2, RENDER_ATTACHMENT: 16 });
    vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const canvas = {
      width: 1600,
      height: 1000,
      getContext: () => ({ configure() {}, getCurrentTexture: () => ({ createView: () => ({}) }) }),
    } as unknown as HTMLCanvasElement;
    return { calls, canvas, lose, uncaptured: (message: string) => listeners.uncapturederror({ error: { message } }) };
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
});

describe("containRect", () => {
  it("letterboxes and pillarboxes", () => {
    expect(containRect(1920, 1200, 1920, 1080)).toEqual({ x: 0, y: 60, w: 1920, h: 1080 });
    expect(containRect(1000, 1000, 2000, 1000)).toEqual({ x: 0, y: 250, w: 1000, h: 500 });
  });
});
