// Audio and the microphone over WebTransport (#1033): framing, A/V sync on timestamps, the
// worklet queue, the decode and encode paths, and the runtime wiring, against fakes.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createWebTransportRuntime } from "../pages/app/webTransportRuntime";
import { MicCaptureError } from "../webrtc/mic";
import { AudioOut, LATE_RUN_RESET, type ContextLike } from "./audioOut";
import { AudioRing, SLACK_FRAMES } from "./audioRing";
import { AudioSync, MAX_TARGET_MS, MIN_TARGET_MS } from "./audioSync";
import { CHANNEL_AUDIO, encodeMicDatagram, parseAudioDatagram } from "./framing";
import { MicOut } from "./micOut";
import type { Presenter } from "./presenter";
import { avcC, COORDS, FakeAgent, FakeChunk, FakeDecoder, flush, helloAck } from "./testSupport";
import { WebTransportMedia } from "./webTransportMedia";
import type { AudioStreams, MediaSink } from "./transport";

const OPUS = { codec: "opus", channels: 2, sample_rate: 48000, ptime_ms: 10 };
const MIC = { codec: "opus", channels: 1, sample_rate: 48000, ptime_ms: 10 };

/** `audio::datagram` in node-agent/src/webtransport/audio.rs: 19-byte header, then Opus. */
function audioDatagram(packetSeq: number, ts48k: number, captureUs: number, opus: Uint8Array): Uint8Array {
  const d = new Uint8Array(19 + opus.length);
  const v = new DataView(d.buffer);
  d[0] = CHANNEL_AUDIO;
  v.setUint32(1, 77, true);
  v.setUint16(5, packetSeq, true);
  v.setUint32(7, ts48k, true);
  v.setBigUint64(11, BigInt(captureUs), true);
  d.set(opus, 19);
  return d;
}

beforeEach(() => {
  nextSeq = 0;
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
  vi.stubGlobal("EncodedAudioChunk", FakeChunk);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("audio framing", () => {
  it("reads the agent's 0x02 header and keeps the Opus packet as a view", () => {
    const opus = Uint8Array.from([0xf4, 1, 2, 3]);
    const p = parseAudioDatagram(audioDatagram(0xfffe, 0x01020304, 1_762_400_000_123_456, opus))!;
    expect(p).toMatchObject({ packetSeq: 0xfffe, ts48k: 0x01020304, captureUs: 1_762_400_000_123_456 });
    expect(Array.from(p.opus)).toEqual([0xf4, 1, 2, 3]);
  });

  it("drops another channel, a bare header and an empty packet", () => {
    expect(parseAudioDatagram(Uint8Array.from([0x01, ...new Array(30).fill(0)]))).toBeNull();
    expect(parseAudioDatagram(audioDatagram(1, 1, 1, new Uint8Array(0)))).toBeNull();
    expect(parseAudioDatagram(new Uint8Array(5))).toBeNull();
  });

  it("writes the microphone datagram as §Microphone: channel, u16 seq, u32 ts, Opus", () => {
    const d = encodeMicDatagram(0x10001, 0x1_0000_0002, Uint8Array.from([9, 8]));
    expect(Array.from(d)).toEqual([0x05, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00, 9, 8]);
  });
});

describe("AudioSync", () => {
  it("fixes the delay floor from the smallest arrival − capture and forgets it after 10 s", () => {
    const s = new AudioSync();
    s.noteAudio(1_000_000, 1_130); // 130 ms
    s.noteAudio(1_010_000, 1_141); // 131 ms
    s.noteAudio(1_020_000, 1_150); // 130 ms
    s.noteAudio(1_030_000, 1_165); // 135 ms
    expect(s.playAtMs(1_040_000, 0)).toBe(1_040 + 130 + 50);
    // Ten seconds on, only the new, slower path is left.
    s.noteAudio(11_000_000, 11_160);
    expect(s.playAtMs(11_000_000, 0)).toBe(11_000 + 160 + 50);
  });

  it("plays audio when its video is shown: D follows the median video latency", () => {
    const s = new AudioSync();
    s.noteAudio(2_000_000, 2_100);
    expect(s.playAtMs(2_000_000)).toBe(2_000 + 100 + 50);
    for (let i = 0; i < 20; i++) s.noteVideo(2_000_000 + i * 16_667, 2_000 + i * 16.667 + 100 + 80);
    s.noteVideo(2_400_000, 2_400 + 100 + 900); // one slow keyframe is not the median
    expect(s.videoLatencyMs()).toBeCloseTo(80, 0);
    expect(s.targetDelayMs()).toBeCloseTo(80, 0);
    expect(s.avOffsetMs()).toBeCloseTo(0, 0);
  });

  it("never plays sooner than the jitter floor or the output device allows, nor later than the cap", () => {
    const s = new AudioSync();
    s.noteAudio(1_000_000, 1_100);
    for (let i = 0; i < 10; i++) s.noteVideo(1_000_000, 1_000 + 100 + 5);
    expect(s.targetDelayMs()).toBe(MIN_TARGET_MS);
    expect(s.targetDelayMs(70)).toBe(70);
    expect(s.avOffsetMs(70)).toBeCloseTo(65, 0);
    for (let i = 0; i < 200; i++) s.noteVideo(1_000_000, 1_000 + 100 + 4_000);
    expect(s.targetDelayMs()).toBe(MAX_TARGET_MS);
  });

  it("adds the jitter the path shows to the playout delay, so a bunching link gets a deeper buffer", () => {
    const s = new AudioSync();
    for (let i = 0; i < 100; i++) s.noteAudio(1_000_000 + i * 10_000, 1_000 + i * 10 + 100);
    expect(s.jitterMs()).toBe(0);
    expect(s.targetDelayMs(25)).toBe(50); // the default until video has set one
    // A mobile link: one packet in five held back 60 ms and delivered with the next.
    for (let i = 100; i < 300; i++) s.noteAudio(1_000_000 + i * 10_000, 1_000 + i * 10 + 100 + (i % 5 === 0 ? 60 : 0));
    expect(s.jitterMs()).toBe(60);
    expect(s.targetDelayMs(25)).toBe(85);
  });

  it("forgets the floor and everything measured against it on reset", () => {
    const s = new AudioSync();
    s.noteAudio(1_000_000, 1_100);
    s.noteVideo(1_000_000, 1_180);
    s.reset();
    expect(s.playAtMs(1_000_000)).toBeNull();
    expect(s.videoLatencyMs()).toBeNull();
    s.noteAudio(2_000_000, 2_400);
    expect(s.playAtMs(2_000_000)).toBe(2_000 + 400 + 50);
  });

  it("has no time to play at until an audio packet has fixed the floor", () => {
    const s = new AudioSync();
    s.noteVideo(1_000_000, 1_200);
    expect(s.playAtMs(1_000_000)).toBeNull();
    expect(s.videoLatencyMs()).toBeNull();
  });
});

describe("AudioRing", () => {
  const chunk = (frames: number, v: number): Float32Array[] => [new Float32Array(frames).fill(v), new Float32Array(frames).fill(v)];
  const quantum = (ring: AudioRing, frame: number, n = 128): Float32Array => {
    const out = [new Float32Array(n), new Float32Array(n)];
    ring.render(out, frame);
    return out[0];
  };

  it("plays a chunk at its frame and silence around it", () => {
    const ring = new AudioRing();
    ring.push(1_000, chunk(480, 0.5), 0);
    expect(Array.from(quantum(ring, 872).slice(120))).toEqual([0, 0, 0, 0, 0, 0, 0, 0]);
    const q = quantum(ring, 1_000);
    expect(q.every((x) => x === 0.5)).toBe(true);
    expect(ring.stats.underrunFrames).toBe(0);
  });

  it("plays chunks back to back within the slack, so jittery timestamps make no gap or overlap", () => {
    const ring = new AudioRing();
    ring.push(0, chunk(480, 1), 0);
    ring.push(480 + 100, chunk(480, 2), 0); // 100 frames late: inside the slack
    ring.push(960 + 100 - 90, chunk(480, 3), 0); // 10 frames late against the second's end, once it ran early
    const out: number[] = [];
    for (let f = 0; f < 1_280; f += 128) out.push(...quantum(ring, f));
    expect(out.slice(470, 490)).toEqual([...new Array(10).fill(1), ...new Array(10).fill(2)]);
    expect(out.slice(950, 970)).toEqual([...new Array(10).fill(2), ...new Array(10).fill(3)]);
    expect(ring.stats.underrunFrames).toBe(0);
  });

  it("leaves a real gap as silence and counts it as an underrun once audio has started", () => {
    const ring = new AudioRing();
    ring.push(0, chunk(128, 1), 0);
    ring.push(128 + SLACK_FRAMES + 128, chunk(128, 2), 0);
    quantum(ring, 0);
    const gap = quantum(ring, 128);
    expect(gap.every((x) => x === 0)).toBe(true);
    expect(ring.stats.underrunFrames).toBe(128);
  });

  it("orders chunks that arrive out of order and drops the ones that are already past", () => {
    const ring = new AudioRing();
    ring.push(480, chunk(480, 2), 0);
    ring.push(0, chunk(480, 1), 0);
    ring.push(0, chunk(100, 9), 5_000); // wholly past the render position
    expect(ring.stats.dropped).toBe(1);
    const out: number[] = [];
    for (let f = 0; f < 1_024; f += 128) out.push(...quantum(ring, f));
    expect(out[0]).toBe(1);
    expect(out[479]).toBe(1);
    expect(out[480]).toBe(2);
  });

  it("skips the head of a chunk that is late, and an overlap plays once", () => {
    const ring = new AudioRing();
    const ramp = [Float32Array.from({ length: 480 }, (_, i) => i), Float32Array.from({ length: 480 }, (_, i) => i)];
    ring.push(0, ramp, 0);
    // Rendering reaches it 200 frames late: its first 200 frames are not played.
    expect(quantum(ring, 200, 4)[0]).toBe(200);
    // A duplicate of the same packet: its overlap with what played is dropped.
    ring.push(0, ramp, 0);
    const rest = quantum(ring, 204, 4);
    expect(Array.from(rest)).toEqual([204, 205, 206, 207]);
  });

  it("holds at most a second of audio", () => {
    const ring = new AudioRing(SLACK_FRAMES, 1_000);
    ring.push(0, chunk(600, 1), 0);
    ring.push(600, chunk(600, 1), 0);
    expect(ring.stats.dropped).toBe(1);
  });
});

// ── AudioOut: decode, schedule, mute ────────────────────────────────────────

class FakeAudioDecoder {
  state = "unconfigured";
  chunks: { timestamp: number; data: Uint8Array }[] = [];
  configs: AudioDecoderConfig[] = [];
  constructor(readonly init: AudioDecoderInit) {}
  configure(c: AudioDecoderConfig) {
    this.configs.push(c);
    this.state = "configured";
  }
  decode(c: { timestamp: number; data: Uint8Array }) {
    this.chunks.push(c);
  }
  close() {
    this.state = "closed";
  }
  /** What the decoder outputs for a chunk. */
  emit(timestamp: number) {
    this.init.output(fakeAudioData(timestamp) as unknown as AudioData);
  }
  fail() {
    this.state = "closed";
    this.init.error(new DOMException("decode", "EncodingError"));
  }
}

function fakeAudioData(timestamp: number) {
  return {
    timestamp,
    numberOfFrames: 480,
    numberOfChannels: 2,
    closed: false,
    copyTo(plane: Float32Array, o: { planeIndex: number }) {
      plane.fill(o.planeIndex + 1);
    },
    close() {
      this.closed = true;
    },
  };
}

function setupAudioOut(opts: { running?: boolean; supported?: boolean } = {}) {
  let t = 5_100;
  const posted: { start: number; planes: Float32Array[] }[] = [];
  const gains: number[] = [];
  const ctx: ContextLike & { resume: ReturnType<typeof vi.fn>; state: string } = {
    state: opts.running === false ? "suspended" : "running",
    sampleRate: 48_000,
    currentTime: 3,
    outputLatency: 0.01,
    baseLatency: 0.005,
    getOutputTimestamp: () => ({ contextTime: 10, performanceTime: 1_000 }),
    onstatechange: null,
    resume: vi.fn(async () => {
      ctx.state = "running";
    }),
    close: vi.fn(async () => {}),
    addOutput: async () => ({
      port: { postMessage: (m: { start: number; planes: Float32Array[] }) => posted.push(m) } as unknown as MessagePort,
      setGain: (g: number) => gains.push(g),
    }),
  };
  const decoders: FakeAudioDecoder[] = [];
  const createContext = vi.fn(() => ctx);
  const out = new AudioOut({
    createContext,
    createDecoder: (init) => {
      const d = new FakeAudioDecoder(init);
      decoders.push(d);
      return d as unknown as AudioDecoder;
    },
    isConfigSupported: async (c) => ({ supported: opts.supported ?? true, config: c }),
    now: () => t,
  });
  return { out, ctx, decoders, posted, gains, createContext, at: (ms: number) => void (t = ms) };
}

const STEREO = { channels: 2, sampleRate: 48_000, ptimeMs: 10 };
let nextSeq = 0;
/** The next packet in `packet_seq` order unless a number is given. */
const packet = (captureUs: number, packetSeq = nextSeq++) => ({ packetSeq, ts48k: 0, captureUs, opus: Uint8Array.from([0xf4, 1, 2]) });

describe("AudioOut", () => {
  it("configures WebCodecs Opus from the hello_ack and decodes each packet under its capture time", async () => {
    const { out, decoders } = setupAudioOut();
    await out.start(STEREO);
    expect(decoders[0].configs).toEqual([{ codec: "opus", sampleRate: 48_000, numberOfChannels: 2 }]);
    out.packet(packet(5_000_000));
    expect(decoders[0].chunks).toHaveLength(1);
    expect(decoders[0].chunks[0].timestamp).toBe(5_000_000);
    expect(Array.from(decoders[0].chunks[0].data)).toEqual([0xf4, 1, 2]);
  });

  it("starts each chunk at the render frame its capture time maps to, one audio-thread timeline", async () => {
    const { out, decoders, posted } = setupAudioOut();
    await out.start(STEREO);
    out.packet(packet(5_000_000)); // arrives 100 ms after capture: the delay floor
    decoders[0].emit(5_000_000);
    // playAt = 5000 + 100 (floor) + max(50 default, 40, 10 + 15) = 5150 ms; the speaker
    // timeline says 1000 ms is context time 10 s: (10 + 4.15 s) at 48 kHz.
    expect(posted).toHaveLength(1);
    expect(posted[0].start).toBe(Math.round((10 + 4.15) * 48_000));
    expect(posted[0].planes).toHaveLength(2);
    expect(posted[0].planes[0][0]).toBe(1);
    expect(posted[0].planes[1][0]).toBe(2);
  });

  it("delays audio by the video's capture→present latency so the two are heard together", async () => {
    const { out, decoders, posted, at } = setupAudioOut();
    await out.start(STEREO);
    out.packet(packet(5_000_000));
    for (let i = 0; i < 30; i++) {
      at(5_000 + i * 16 + 100 + 90); // frames shown 90 ms past the floor
      out.noteVideo(5_000_000 + i * 16_000);
    }
    at(5_100);
    decoders[0].emit(5_000_000);
    expect(posted[0].start).toBe(Math.round((10 + (5_000 + 100 + 90 - 1_000) / 1_000) * 48_000));
    expect(out.stats().avOffsetMs).toBeCloseTo(0, 0);
    expect(out.stats().videoLatencyMs).toBeCloseTo(90, 0);
  });

  it("closes the decoded data it was given, played or not", async () => {
    const { out, decoders } = setupAudioOut();
    await out.start(STEREO);
    const data = fakeAudioData(5_000_000);
    // No packet has fixed the delay floor: nothing can be scheduled, and nothing leaks.
    decoders[0].init.output(data as unknown as AudioData);
    expect(data.closed).toBe(true);
  });

  it("decodes nothing while the context is suspended, which would play it stale later", async () => {
    const { out, decoders, ctx } = setupAudioOut({ running: false });
    await out.start(STEREO);
    out.packet(packet(5_000_000));
    expect(decoders[0].chunks).toHaveLength(0);
    ctx.state = "running";
    out.packet(packet(5_010_000));
    expect(decoders[0].chunks).toHaveLength(1);
  });

  it("starts muted, unmutes and resumes on release, and mutes again on hold", async () => {
    const { out, ctx, gains } = setupAudioOut({ running: false });
    await out.start(STEREO);
    expect(gains).toEqual([0]);
    out.release();
    expect(gains).toEqual([0, 1]);
    expect(ctx.resume).toHaveBeenCalled();
    out.hold();
    expect(gains).toEqual([0, 1, 0]);
  });

  it("restarts a decoder that errors, and gives up on audio after three in a row without ending anything", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const { out, decoders } = setupAudioOut();
    await out.start(STEREO);
    for (let i = 0; i < 3; i++) decoders[decoders.length - 1].fail();
    expect(decoders).toHaveLength(4);
    decoders[3].fail();
    expect(decoders).toHaveLength(4);
    expect(() => out.packet(packet(5_000_000))).not.toThrow();
  });

  it("stays silent, without a context, when the browser cannot decode Opus", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const { out, createContext } = setupAudioOut({ supported: false });
    await out.start(STEREO);
    out.packet(packet(5_000_000));
    expect(createContext).not.toHaveBeenCalled();
  });

  it("schedules each chunk from its own packet's capture time, so a lost packet moves no other", async () => {
    const { out, decoders, posted, at } = setupAudioOut();
    await out.start(STEREO);
    const frameAt = (captureUs: number) => Math.round((10 + (captureUs / 1000 + 100 + 50 - 1_000) / 1_000) * 48_000);
    // Packets 0, 1 and 3 (2 was lost). The decoder stamps its output from the first input's
    // timestamp plus the frames it has decoded, which is the same for every chunk after a loss.
    for (const [capture, seq] of [[5_000_000, 0], [5_010_000, 1], [5_030_000, 3]]) {
      at(capture / 1000 + 100); // each arrives 100 ms after its capture
      out.packet(packet(capture, seq));
    }
    at(5_100);
    for (const ts of [5_000_000, 5_010_000, 5_020_000]) decoders[0].emit(ts);
    const real = posted.filter((p) => p.planes[0][300] === 1);
    expect(real.map((p) => p.start)).toEqual([frameAt(5_000_000), frameAt(5_010_000), frameAt(5_030_000)]);
  });

  it("plays one concealed 10 ms chunk for a lost packet: the last chunk fading to silence, then a fade in", async () => {
    const { out, decoders, posted, at } = setupAudioOut();
    await out.start(STEREO);
    const frameAt = (captureUs: number) => Math.round((10 + (captureUs / 1000 + 150 - 1_000) / 1_000) * 48_000);
    at(5_100);
    out.packet(packet(5_000_000, 10));
    decoders[0].emit(5_000_000);
    at(5_140);
    out.packet(packet(5_040_000, 14)); // 11, 12 and 13 were lost
    expect(out.stats().concealed).toBe(1);
    decoders[0].emit(5_040_000);
    expect(posted).toHaveLength(3);
    const [before, hole, after] = posted;
    // The concealment sits where the packet before the next one would have been.
    expect(hole.start).toBe(frameAt(5_030_000));
    expect(hole.planes[0][0]).toBeCloseTo(1, 1);
    expect(hole.planes[0][479]).toBe(0);
    expect(hole.planes[0][240]).toBeCloseTo(0.5, 1);
    // The chunk after the gap does not start at full level.
    expect(before.planes[0][0]).toBe(1);
    expect(after.planes[0][0]).toBe(0);
    expect(after.planes[0][119]).toBeGreaterThan(0.9);
    expect(after.planes[0][300]).toBe(1);
  });

  it("drops a duplicate, a late arrival and a reordered packet by packet_seq, in 16-bit serial order", async () => {
    const { out, decoders } = setupAudioOut();
    await out.start(STEREO);
    for (const seq of [65_534, 65_535, 0, 1, 1, 65_535, 3, 2]) out.packet(packet(5_000_000 + 1_000 * ((seq + 2) % 65_536), seq));
    // 65534, 65535, 0, 1 and 3 are taken; the repeat of 1, the old 65535 and the late 2 are not.
    expect(decoders[0].chunks).toHaveLength(5);
    expect(out.stats().stale).toBe(3);
  });

  it("makes up no packets for a pause of seconds", async () => {
    const { out, decoders } = setupAudioOut();
    await out.start(STEREO);
    out.packet(packet(5_000_000, 0));
    decoders[0].emit(5_000_000);
    out.packet(packet(9_000_000, 400));
    expect(out.stats().concealed).toBe(0);
  });

  it("measures the delay floor again after a run of chunks that were due before they could be posted", async () => {
    const { out, decoders, posted, at } = setupAudioOut();
    await out.start(STEREO);
    at(5_100);
    out.packet(packet(5_000_000)); // arrives 100 ms after capture: the floor
    decoders[0].emit(5_000_000);
    expect(posted).toHaveLength(1);
    // The path steps to 400 ms (a handover): every packet is now late against the old floor.
    for (let i = 1; i <= LATE_RUN_RESET; i++) {
      const capture = 5_000_000 + i * 10_000;
      at(5_000 + i * 10 + 400);
      out.packet(packet(capture));
      decoders[0].emit(capture);
    }
    expect(out.stats().late).toBe(LATE_RUN_RESET);
    expect(posted).toHaveLength(1);
    // The next packet fixes the new floor, and is played.
    const capture = 5_000_000 + (LATE_RUN_RESET + 1) * 10_000;
    at(5_000 + (LATE_RUN_RESET + 1) * 10 + 400);
    out.packet(packet(capture));
    decoders[0].emit(capture);
    expect(posted).toHaveLength(2);
    expect(out.stats().late).toBe(LATE_RUN_RESET);
  });

  it("does not reset the floor for a few late chunks, and covers a link that keeps bunching with a deeper delay", async () => {
    const { out, decoders, posted, at } = setupAudioOut();
    await out.start(STEREO);
    at(5_100);
    out.packet(packet(5_000_000));
    decoders[0].emit(5_000_000);
    for (let i = 1; i <= 3 * LATE_RUN_RESET; i++) {
      const capture = 5_000_000 + i * 10_000;
      // One in five 120 ms late, the rest on the floor.
      at(5_000 + i * 10 + 100 + (i % 5 === 0 ? 120 : 0));
      out.packet(packet(capture));
      decoders[0].emit(capture);
    }
    // Three are late before the jitter estimate has 20 packets behind it; after that the delay
    // covers the 120 ms the link adds, and nothing is dropped.
    expect(out.stats().late).toBe(3);
    expect(posted).toHaveLength(1 + 3 * LATE_RUN_RESET - 3);
    expect(out.stats().targetDelayMs).toBeGreaterThanOrEqual(120);
  });

  it("resumes a context it finds suspended while playing, and not while held", async () => {
    const { out, ctx } = setupAudioOut({ running: false });
    await out.start(STEREO);
    expect(ctx.resume).not.toHaveBeenCalled(); // held: not asked to make sound
    ctx.onstatechange?.();
    expect(ctx.resume).not.toHaveBeenCalled();
    out.release();
    expect(ctx.resume).toHaveBeenCalledTimes(1);
    ctx.state = "suspended";
    ctx.onstatechange?.();
    expect(ctx.resume).toHaveBeenCalledTimes(2);
    ctx.state = "running";
    ctx.onstatechange?.();
    expect(ctx.resume).toHaveBeenCalledTimes(2);
  });

  it("resumes when it starts if the page already released the sound", async () => {
    const { out, ctx } = setupAudioOut({ running: false });
    out.release();
    await out.start(STEREO);
    expect(ctx.resume).toHaveBeenCalled();
  });

  it("closes its decoder and context", async () => {
    const { out, ctx, decoders } = setupAudioOut();
    await out.start(STEREO);
    out.close();
    expect(decoders[0].state).toBe("closed");
    expect(ctx.close).toHaveBeenCalled();
  });
});

// ── MicOut: capture → Opus → 0x05 ────────────────────────────────────────────

function setupMic() {
  const sent: Uint8Array[] = [];
  const encoders: FakeEncoder[] = [];
  class FakeEncoder {
    state = "unconfigured";
    config: AudioEncoderConfig | null = null;
    encoded: unknown[] = [];
    constructor(readonly init: AudioEncoderInit) {}
    configure(c: AudioEncoderConfig) {
      this.config = c;
      this.state = "configured";
    }
    encode(data: { timestamp: number }) {
      this.encoded.push(data);
      const opus = Uint8Array.from([0xf8, data.timestamp & 0xff]);
      this.init.output({
        byteLength: opus.length,
        timestamp: data.timestamp,
        copyTo: (to: Uint8Array) => to.set(opus),
      } as unknown as EncodedAudioChunk, undefined);
    }
    close() {
      this.state = "closed";
    }
  }
  let push!: (d: { timestamp: number; close(): void }) => void;
  const cancelled = vi.fn();
  const processor = () => ({
    readable: new ReadableStream<AudioData>({
      start: (c) =>
        void (push = (d) => {
          try {
            c.enqueue(d as unknown as AudioData);
          } catch {
            /* cancelled: nothing reads it */
          }
        }),
      cancel: cancelled,
    }),
  });
  const mic = new MicOut({
    send: (d) => {
      sent.push(d);
      return true;
    },
    createEncoder: (init) => {
      const e = new FakeEncoder(init);
      encoders.push(e);
      return e as unknown as AudioEncoder;
    },
    createProcessor: processor,
  });
  return { mic, sent, encoders, push: (d: { timestamp: number; close(): void }) => push(d), cancelled };
}

describe("MicOut", () => {
  it("encodes 10 ms mono Opus and sends each packet as a numbered 0x05 datagram", async () => {
    const { mic, sent, encoders, push } = setupMic();
    mic.start({} as MediaStreamTrack);
    expect(encoders[0].config).toMatchObject({
      codec: "opus",
      sampleRate: 48_000,
      numberOfChannels: 1,
      opus: { frameDuration: 10_000 },
    });
    const frames = [10_000, 20_000, 30_000].map((timestamp) => ({ timestamp, close: vi.fn() }));
    frames.forEach(push);
    await vi.waitFor(() => expect(sent).toHaveLength(3));
    expect(sent.map((d) => d[0])).toEqual([0x05, 0x05, 0x05]);
    expect(sent.map((d) => new DataView(d.buffer).getUint16(1, true))).toEqual([0, 1, 2]);
    // µs on the capture clock → 48 kHz ticks.
    expect(sent.map((d) => new DataView(d.buffer).getUint32(3, true))).toEqual([480, 960, 1_440]);
    expect(Array.from(sent[0].subarray(7))).toEqual([0xf8, 10_000 & 0xff]);
    expect(frames.every((f) => f.close.mock.calls.length === 1)).toBe(true);
  });

  it("releases the capture reader and the encoder on stop, and sends nothing after", async () => {
    const { mic, sent, encoders, push, cancelled } = setupMic();
    mic.start({} as MediaStreamTrack);
    mic.stop();
    expect(encoders[0].state).toBe("closed");
    await vi.waitFor(() => expect(cancelled).toHaveBeenCalled());
    push({ timestamp: 1, close: vi.fn() });
    await flush();
    expect(sent).toHaveLength(0);
  });

  it("restarting replaces the previous capture", () => {
    const { mic, encoders } = setupMic();
    mic.start({} as MediaStreamTrack);
    mic.start({} as MediaStreamTrack);
    expect(encoders[0].state).toBe("closed");
    expect(encoders).toHaveLength(2);
  });

  it("refuses with the mic's own error kind where the browser has no AudioEncoder or track processor", () => {
    const mic = new MicOut({ send: () => true });
    try {
      mic.start({} as MediaStreamTrack);
      expect.unreachable();
    } catch (err) {
      expect(err).toBeInstanceOf(MicCaptureError);
      expect((err as MicCaptureError).detail.kind).toBe("unsupported");
    }
  });
});

// ── The transport carries audio in and the microphone out ───────────────────

describe("WebTransportMedia audio", () => {
  const withAudio = (extra: Record<string, unknown> = {}) => ({ ...helloAck(0, avcC()), audio: OPUS, mic: MIC, ...extra });
  const make = (agent: FakeAgent) =>
    new WebTransportMedia({
      sessionId: "s1",
      coords: COORDS,
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
      sleep: async () => {},
      ticker: () => () => {},
    } as ConstructorParameters<typeof WebTransportMedia>[0]);

  it("tells the sink what the hello_ack carries before any datagram, and null for what it lacks", async () => {
    for (const [ack, want] of [
      [withAudio(), { audio: { channels: 2, sampleRate: 48_000, ptimeMs: 10 }, mic: { channels: 1, sampleRate: 48_000, ptimeMs: 10 } }],
      [helloAck(0, avcC()), { audio: null, mic: null }],
      [withAudio({ mic: null }), { audio: { channels: 2, sampleRate: 48_000, ptimeMs: 10 }, mic: null }],
    ] as [Record<string, unknown>, AudioStreams][]) {
      const agent = new FakeAgent();
      agent.replies = [[ack]];
      const order: string[] = [];
      const streams: AudioStreams[] = [];
      await make(agent).connect({
        streams: (s) => (order.push("streams"), streams.push(s)),
        config: () => order.push("config"),
        unit: () => {},
        end: () => {},
      });
      expect(streams).toEqual([want]);
      expect(order).toEqual(["streams", "config"]);
    }
  });

  it("hands audio datagrams to the sink in arrival order, whole, and skips what is not audio", async () => {
    const agent = new FakeAgent();
    agent.replies = [[withAudio()]];
    const got: number[] = [];
    await make(agent).connect({
      audio: (p) => got.push(p.packetSeq),
      config: () => {},
      unit: () => {},
      end: () => {},
    });
    agent.last.datagrams([
      audioDatagram(2, 960, 2_000_000, Uint8Array.from([1])),
      audioDatagram(1, 480, 1_000_000, Uint8Array.from([1])),
      new Uint8Array([0x7f, 1, 2, 3]),
      audioDatagram(3, 1_440, 3_000_000, new Uint8Array(0)),
      audioDatagram(4, 1_920, 4_000_000, Uint8Array.from([1])),
    ]);
    await vi.waitFor(() => expect(got).toEqual([2, 1, 4]));
  });

  it("sends a microphone datagram on the datagram writer, within the size the path allows", async () => {
    const agent = new FakeAgent();
    agent.replies = [[withAudio()]];
    const t = make(agent);
    expect(t.sendMic(new Uint8Array(10))).toBe(false); // not connected
    await t.connect({ config: () => {}, unit: () => {}, end: () => {} } as MediaSink);
    expect(t.sendMic(encodeMicDatagram(0, 0, Uint8Array.from([1, 2])))).toBe(true);
    expect(t.sendMic(new Uint8Array(1_149))).toBe(false);
    await vi.waitFor(() => expect(agent.datagramsSent).toHaveLength(1));
    expect(Array.from(agent.datagramsSent[0])).toEqual([0x05, 0, 0, 0, 0, 0, 0, 1, 2]);
  });
});

// ── The runtime ────────────────────────────────────────────────────────────

class IdleEncoder {
  static made: IdleEncoder[] = [];
  state = "unconfigured";
  constructor() {
    IdleEncoder.made.push(this);
  }
  configure() {
    this.state = "configured";
  }
  encode() {}
  close() {
    this.state = "closed";
  }
}

describe("createWebTransportRuntime audio and microphone", () => {
  function setup(ack: Record<string, unknown>) {
    const agent = new FakeAgent();
    agent.replies = [[ack]];
    const presenter: Presenter = { present: (f) => (f.close(), true), destroy: vi.fn() };
    const audio = setupAudioOut();
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: { dataset: {} } as unknown as HTMLCanvasElement,
      callbacks: { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() },
      deps: {
        createTransport: (o) =>
          new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {}, ticker: () => () => {} } as ConstructorParameters<typeof WebTransportMedia>[0]),
        createPresenter: async () => presenter,
        decoder: {
          createDecoder: (init) => new FakeDecoder(init) as unknown as VideoDecoder,
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
        audio: {
          createContext: audio.createContext,
          createDecoder: (init) => {
            const d = new FakeAudioDecoder(init);
            audio.decoders.push(d);
            return d as unknown as AudioDecoder;
          },
          isConfigSupported: async (c) => ({ supported: true, config: c }),
          now: () => performance.now(),
        },
        mic: { createEncoder: () => new IdleEncoder() as unknown as AudioEncoder, createProcessor: () => ({ readable: new ReadableStream<AudioData>() }) },
      },
    });
    return { agent, rt, audio };
  }

  it("a session with a microphone grant offers the mic slot, one without does not", async () => {
    const a = setup({ ...helloAck(0, avcC()), audio: OPUS, mic: MIC });
    expect(a.rt.hasMicSlot()).toBe(false);
    a.rt.start();
    await vi.waitFor(() => expect(a.rt.getSnapshot().channelOpen).toBe(true));
    expect(a.rt.hasMicSlot()).toBe(true);
    await a.rt.attachMicTrack({} as MediaStreamTrack);
    await a.rt.detachMicTrack();
    a.rt.destroy();

    const b = setup({ ...helloAck(0, avcC()), audio: OPUS, mic: null });
    b.rt.start();
    await vi.waitFor(() => expect(b.rt.getSnapshot().channelOpen).toBe(true));
    expect(b.rt.hasMicSlot()).toBe(false);
    b.rt.destroy();
  });

  it("plays the agent's audio muted until the page releases it, and mutes again on hold", async () => {
    const { agent, rt, audio } = setup({ ...helloAck(0, avcC()), audio: OPUS, mic: null });
    rt.start();
    await vi.waitFor(() => expect(audio.gains).toEqual([0]));
    agent.last.datagrams([audioDatagram(1, 480, 5_000_000, Uint8Array.from([0xf4, 1]))]);
    await vi.waitFor(() => expect(audio.decoders[0].chunks).toHaveLength(1));
    rt.releaseAudio();
    rt.holdAudio();
    expect(audio.gains).toEqual([0, 1, 0]);
    rt.destroy();
    expect(audio.ctx.close).toHaveBeenCalled();
  });

  /** Each way the runtime ends, and the micro-task that ends it. */
  const endings: [string, (s: ReturnType<typeof setup>) => void][] = [
    ["destroy", ({ rt }) => rt.destroy()],
    ["a takeover", ({ agent }) => agent.last.closeWith(4410)],
    ["a session that ended", ({ agent }) => agent.last.closeWith(4404)],
    ["a lost connection", ({ agent }) => agent.last.closeWith(1006)],
  ];
  it.each(endings)("on %s the microphone is stopped and the audio context closed, and neither restarts", async (_name, end) => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    IdleEncoder.made.length = 0;
    const s = setup({ ...helloAck(0, avcC()), audio: OPUS, mic: MIC });
    s.rt.start();
    await vi.waitFor(() => expect(s.rt.getSnapshot().channelOpen).toBe(true));
    await vi.waitFor(() => expect(s.audio.createContext).toHaveBeenCalled());
    await s.rt.attachMicTrack({} as MediaStreamTrack);
    expect(IdleEncoder.made).toHaveLength(1);
    expect(IdleEncoder.made[0].state).toBe("configured");

    end(s);
    await vi.waitFor(() => expect(IdleEncoder.made[0].state).toBe("closed"));
    await vi.waitFor(() => expect(s.audio.ctx.close).toHaveBeenCalled());
    // The page may still hand over a track (its permission prompt was open): nothing starts.
    await s.rt.attachMicTrack({} as MediaStreamTrack);
    expect(IdleEncoder.made).toHaveLength(1);
    expect(s.rt.hasMicSlot()).toBe(false);
    s.rt.destroy();
  });

  it("a session without audio builds no audio context", async () => {
    const { rt, audio } = setup(helloAck(0, avcC()));
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    await flush();
    expect(audio.createContext).not.toHaveBeenCalled();
    rt.destroy();
  });
});
