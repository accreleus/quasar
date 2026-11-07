import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  ControlDecoder,
  ControlProtocolError,
  encodeControl,
  parseVideoDatagram,
  serialAfter,
} from "./framing";
import { FrameAssembler } from "./reassembly";
import { DecodeStage, HOLD_MS, avcCodecString } from "./decodeStage";
import { containRect, createWebGpuPresenter, type Presenter } from "./presenter";
import { CONNECT_DEADLINE_MS, WebTransportMedia } from "./webTransportMedia";
import { createWebTransportRuntime } from "../pages/app/webTransportRuntime";
import type { WebTransportCoords } from "../api/types";

// ── The agent's side, as node-agent/src/webtransport/media.rs packetizes it ──

const HEADER = 25;
const MAX_DATAGRAM = 1148;

interface UnitSpec {
  configId: number;
  frameSeq: number;
  keyframe: boolean;
  data: Uint8Array;
  captureUs?: number;
  discardable?: boolean;
}

/** `media::packetize`: 25-byte header, in-order byte ranges of the unit. */
function packetize(u: UnitSpec, pktSeq = { n: 0 }): Uint8Array[] {
  const room = MAX_DATAGRAM - HEADER;
  const frags: Uint8Array[] = [];
  for (let at = 0; at < u.data.length; at += room) frags.push(u.data.subarray(at, at + room));
  if (frags.length === 0) frags.push(new Uint8Array(0));
  return frags.map((frag, index) => {
    const d = new Uint8Array(HEADER + frag.length);
    const v = new DataView(d.buffer);
    d[0] = 0x01;
    v.setUint32(1, pktSeq.n++, true);
    d[5] = (u.keyframe ? 1 : 0) | (u.discardable ? 2 : 0);
    d[6] = u.configId;
    v.setUint16(7, u.frameSeq, true);
    v.setUint16(9, index, true);
    v.setUint16(11, frags.length, true);
    v.setUint32(13, u.frameSeq * 1500, true);
    v.setBigUint64(17, BigInt(u.captureUs ?? 1_762_400_000_000_000 + u.frameSeq * 16_667), true);
    d.set(frag, HEADER);
    return d;
  });
}

function bytes(n: number, seed: number): Uint8Array {
  return Uint8Array.from({ length: n }, (_, i) => (i * 7 + seed) & 0xff);
}

/** An avcC: version 1, constrained baseline (0x42 0xe0), level 3.1, 4-byte NAL lengths. */
function avcC(level = 0x1f): Uint8Array {
  return Uint8Array.from([1, 0x42, 0xe0, level, 0xff, 0xe1, 0, 0]);
}
const b64 = (u: Uint8Array) => btoa(String.fromCharCode(...u));

function helloAck(configId: number, description: Uint8Array, width = 1920, height = 1080) {
  return {
    type: "hello_ack",
    wt_proto: 1,
    config_id: configId,
    video: { codec: "h264", description_b64: b64(description), width, height, fps: 60 },
    audio: null,
    mic: null,
    fec: null,
  };
}

/** A scripted agent behind a fake browser `WebTransport`. */
class FakeAgent {
  urls: string[] = [];
  options: WebTransportOptions[] = [];
  received: Record<string, unknown>[] = [];
  /** What the agent answers each hello with, in order; the last repeats. */
  replies: Record<string, unknown>[][] = [];
  closes: (WebTransportCloseInfo | undefined)[] = [];
  private dgram: ReadableStreamDefaultController<Uint8Array> | null = null;
  private ctl: ReadableStreamDefaultController<Uint8Array> | null = null;
  private resolveClosed: ((i: WebTransportCloseInfo) => void) | null = null;
  private hellos = 0;

  create = (url: string, options: WebTransportOptions): WebTransport => {
    this.urls.push(url);
    this.options.push(options);
    const decoder = new ControlDecoder();
    let resolveClosed!: (i: WebTransportCloseInfo) => void;
    const closed = new Promise<WebTransportCloseInfo>((r) => (resolveClosed = r));
    this.resolveClosed = resolveClosed;
    const agent = this;
    const wt = {
      ready: Promise.resolve(),
      closed,
      datagrams: {
        readable: new ReadableStream<Uint8Array>({ start: (c) => void (agent.dgram = c) }),
      },
      async createBidirectionalStream() {
        return {
          readable: new ReadableStream<Uint8Array>({ start: (c) => void (agent.ctl = c) }),
          writable: new WritableStream<Uint8Array>({
            write(chunk) {
              for (const m of decoder.push(chunk)) {
                agent.received.push(m);
                if (m.type === "hello") agent.onHello();
              }
            },
          }),
        };
      },
      close(info?: WebTransportCloseInfo) {
        agent.closes.push(info);
        resolveClosed(info ?? { closeCode: 0, reason: "" });
      },
    };
    return wt as unknown as WebTransport;
  };

  private onHello(): void {
    const script = this.replies[Math.min(this.hellos++, this.replies.length - 1)] ?? [];
    for (const m of script) {
      // The agent's refusal racing its own teardown: the page reads an error, not the answer.
      if ((m as { type?: string }).type === "teardown") this.ctl?.error(new Error("Connection lost"));
      else this.control(m);
    }
  }

  control(m: object): void {
    this.ctl?.enqueue(encodeControl(m));
  }

  datagrams(ds: Uint8Array[]): void {
    for (const d of ds) this.dgram?.enqueue(d);
  }

  /** The agent closes the session with a WebTransport close code. */
  closeWith(code: number): void {
    this.resolveClosed?.({ closeCode: code, reason: "" });
  }

  sent(type: string): Record<string, unknown>[] {
    return this.received.filter((m) => m.type === type);
  }
}

const COORDS: WebTransportCoords = {
  url: "https://agent.invalid:4433/wt",
  token: "tok-1",
  expires_at: new Date(Date.now() + 60_000).toISOString(),
  cert_hashes: [{ algorithm: "sha-256", value: b64(Uint8Array.from([1, 2, 3, 4])) }],
};

// ── A fake WebCodecs ────────────────────────────────────────────────────────

class FakeChunk {
  type: string;
  timestamp: number;
  data: Uint8Array;
  constructor(init: { type: string; timestamp: number; data: Uint8Array }) {
    this.type = init.type;
    this.timestamp = init.timestamp;
    this.data = new Uint8Array(init.data);
  }
}

interface FakeFrame {
  timestamp: number;
  closed: boolean;
  close(): void;
}

class FakeDecoder {
  state = "unconfigured";
  configs: VideoDecoderConfig[] = [];
  chunks: FakeChunk[] = [];
  constructor(private init: VideoDecoderInit) {}
  configure(c: VideoDecoderConfig) {
    this.configs.push(c);
    this.state = "configured";
  }
  decode(chunk: FakeChunk) {
    this.chunks.push(chunk);
    const frame: FakeFrame = { timestamp: chunk.timestamp, closed: false, close() { this.closed = true; } };
    this.init.output(frame as unknown as VideoFrame);
  }
  close() {
    this.state = "closed";
  }
}

const flush = () => new Promise((r) => setTimeout(r, 0));

beforeEach(() => {
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
});
afterEach(() => {
  vi.unstubAllGlobals();
});

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
  it("rebuilds a unit from fragments in any order", () => {
    const data = bytes(3000, 3);
    const frags = packetize({ configId: 0, frameSeq: 9, keyframe: true, data }).map((d) => parseVideoDatagram(d)!);
    expect(frags).toHaveLength(3);
    const a = new FrameAssembler();
    expect(a.push(frags[2]).unit).toBeNull();
    expect(a.push(frags[0]).unit).toBeNull();
    const { unit } = a.push(frags[1]);
    expect(unit?.frameSeq).toBe(9);
    expect(Array.from(unit!.data)).toEqual(Array.from(data));
  });

  it("asks for a keyframe once a referenced frame is lost, and drops deltas until it comes", () => {
    const a = new FrameAssembler();
    const frag = (frameSeq: number, keyframe: boolean, n = 1, discardable = false) =>
      packetize({ configId: 0, frameSeq, keyframe, data: bytes(n, frameSeq), discardable }).map((d) => parseVideoDatagram(d)!);
    expect(a.push(frag(1, true)[0]).unit?.frameSeq).toBe(1);
    const lost = frag(2, false, 2000); // two fragments; only the first arrives
    expect(a.push(lost[0]).unit).toBeNull();
    expect(a.push(frag(3, false)[0])).toEqual({ unit: null, needKeyframe: true });
    expect(a.push(frag(4, false)[0]).unit).toBeNull();
    expect(a.push(lost[1]).unit).toBeNull(); // late: older than a completed frame
    expect(a.push(frag(5, true)[0]).unit?.frameSeq).toBe(5);
    expect(a.push(frag(6, false)[0]).unit?.frameSeq).toBe(6);
  });

  it("loses a discardable frame without a keyframe", () => {
    const a = new FrameAssembler();
    const frag = (frameSeq: number, keyframe: boolean, n = 1, discardable = false) =>
      packetize({ configId: 0, frameSeq, keyframe, data: bytes(n, frameSeq), discardable }).map((d) => parseVideoDatagram(d)!);
    a.push(frag(1, true)[0]);
    a.push(frag(2, false, 2000, true)[0]);
    expect(a.push(frag(3, false)[0]).unit?.frameSeq).toBe(3);
  });
});

// ── Decode stage ────────────────────────────────────────────────────────────

describe("DecodeStage", () => {
  const config = (configId: number, description = avcC()) => ({
    configId,
    codec: "h264",
    description,
    width: 1280,
    height: 720,
    fps: 60,
  });
  const unit = (configId: number, frameSeq: number, keyframe: boolean) => ({
    configId,
    frameSeq,
    keyframe,
    ts90k: 0,
    captureUs: 1_000_000 + frameSeq,
    data: bytes(8, frameSeq),
  });

  function stage(now = () => 0) {
    const decoders: FakeDecoder[] = [];
    const frames: FakeFrame[] = [];
    const keyframes: string[] = [];
    const s = new DecodeStage({
      onFrame: (f) => frames.push(f as unknown as FakeFrame),
      requestKeyframe: (r) => keyframes.push(r),
      onUnsupported: () => keyframes.push("unsupported"),
      createDecoder: (init) => {
        const d = new FakeDecoder(init);
        decoders.push(d);
        return d as unknown as VideoDecoder;
      },
      isConfigSupported: async (c) => ({ supported: true, config: c }),
      now,
    });
    return { s, decoders, frames, keyframes };
  }

  it("names the H.264 codec from the avcC", () => {
    expect(avcCodecString(avcC(0x34))).toBe("avc1.42e034");
  });

  it("drops a delta before the first keyframe and asks for one", async () => {
    const { s, decoders, keyframes } = stage();
    s.config(config(0));
    await flush();
    s.unit(unit(0, 1, false));
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
    const { s, keyframes } = stage(() => t);
    s.config(config(0));
    await flush();
    s.unit(unit(3, 1, true));
    t = HOLD_MS + 1;
    s.unit(unit(3, 2, false));
    expect(keyframes).toEqual(["decode_error"]);
  });
});

describe("createWebGpuPresenter", () => {
  it("falls back to copying each frame once an import fails, and closes every frame", async () => {
    const calls: string[] = [];
    const pass = { setPipeline() {}, setBindGroup() {}, setViewport() {}, draw() {}, end() {} };
    const device = {
      queue: { submit: () => calls.push("submit"), copyExternalImageToTexture: () => calls.push("copy") },
      createShaderModule: () => ({}),
      createRenderPipeline: () => ({ getBindGroupLayout: () => ({}) }),
      createSampler: () => ({}),
      importExternalTexture: () => {
        calls.push("import");
        throw new Error("Failed to import texture from video");
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
    const p = (await createWebGpuPresenter(canvas))!;
    const frame = () => ({ displayWidth: 1920, displayHeight: 1080, closed: false, close() { this.closed = true; } });
    const [a, b] = [frame(), frame()];
    p.present(a as unknown as VideoFrame);
    p.present(b as unknown as VideoFrame);
    expect(calls).toEqual(["import", "copy", "submit", "copy", "submit"]);
    expect(a.closed && b.closed).toBe(true);
  });
});

describe("containRect", () => {
  it("letterboxes and pillarboxes", () => {
    expect(containRect(1920, 1200, 1920, 1080)).toEqual({ x: 0, y: 60, w: 1920, h: 1080 });
    expect(containRect(1000, 1000, 2000, 1000)).toEqual({ x: 0, y: 250, w: 1000, h: 500 });
  });
});

// ── Transport and runtime, against the fake agent ──────────────────────────

describe("WebTransportMedia", () => {
  it("retries session_not_ready with the same token and mints once on auth_failed", async () => {
    const agent = new FakeAgent();
    agent.replies = [
      [{ type: "error", code: "session_not_ready" }],
      [{ type: "error", code: "auth_failed" }],
      [helloAck(0, avcC())],
    ];
    const mint = vi.fn(async () => ({ ...COORDS, token: "tok-2" }));
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: COORDS,
      mintToken: mint,
      createWebTransport: agent.create,
      sleep: async () => {},
    });
    const configs: number[] = [];
    await t.connect({ config: (c) => configs.push(c.configId), unit: () => {}, end: () => {} });
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-1", "tok-2"]);
    expect(mint).toHaveBeenCalledTimes(1);
    expect(configs).toEqual([0]);
  });

  it("retries a connection torn down before its answer could be read", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "teardown" }], [helloAck(0, avcC())]];
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: COORDS,
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
      sleep: async () => {},
    });
    await t.connect({ config: () => {}, unit: () => {}, end: () => {} });
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-1"]);
  });

  it("stops retrying at the connect deadline", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "session_not_ready" }]];
    let now = Date.now();
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: { ...COORDS, expires_at: new Date(now + 10 * CONNECT_DEADLINE_MS).toISOString() },
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
      sleep: async (ms) => void (now += ms * 40),
      now: () => now,
    });
    await expect(t.connect({ config: () => {}, unit: () => {}, end: () => {} })).rejects.toMatchObject({
      code: "connect_timeout",
    });
  });

  it("gives up after a second auth_failed", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "auth_failed" }]];
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: COORDS,
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
    });
    await expect(t.connect({ config: () => {}, unit: () => {}, end: () => {} })).rejects.toMatchObject({
      code: "auth_failed",
    });
  });

  it("sends no certificate hashes for an edge URL", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const t = new WebTransportMedia({
      sessionId: "s1",
      coords: { ...COORDS, cert_hashes: [] },
      mintToken: async () => COORDS,
      createWebTransport: agent.create,
    });
    await t.connect({ config: () => {}, unit: () => {}, end: () => {} });
    expect(agent.options[0]).toEqual({});
  });
});

describe("createWebTransportRuntime", () => {
  function setup() {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const decoders: FakeDecoder[] = [];
    const presented: FakeFrame[] = [];
    const presenter: Presenter = {
      present: (f) => {
        presented.push(f as unknown as FakeFrame);
        f.close();
      },
      destroy: vi.fn(),
    };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: {} as HTMLCanvasElement,
      callbacks,
      deps: {
        createTransport: (o) =>
          new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async () => presenter,
        decoder: {
          createDecoder: (init) => {
            const d = new FakeDecoder(init);
            decoders.push(d);
            return d as unknown as VideoDecoder;
          },
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
      },
    });
    return { agent, decoders, presented, presenter, callbacks, rt };
  }

  it("connects with the contract's hello, decodes reassembled frames and presents each one", async () => {
    const { agent, decoders, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));

    expect(agent.urls).toEqual([COORDS.url]);
    expect(Array.from(agent.options[0].serverCertificateHashes![0].value as Uint8Array)).toEqual([1, 2, 3, 4]);
    expect(agent.sent("hello")[0]).toMatchObject({ wt_proto: 1, token: "tok-1", session_id: "s1", client: "browser" });

    const units = [
      { configId: 0, frameSeq: 0, keyframe: true, data: bytes(5000, 0) },
      { configId: 0, frameSeq: 1, keyframe: false, data: bytes(700, 1) },
      { configId: 0, frameSeq: 2, keyframe: false, data: bytes(2400, 2) },
    ];
    const pkt = { n: 0 };
    // Fragments of a frame arrive reversed: datagrams are unordered.
    for (const u of units) agent.datagrams(packetize(u, pkt).reverse());
    await vi.waitFor(() => expect(presented).toHaveLength(3));

    expect(decoders[0].configs[0]).toMatchObject({ codec: "avc1.42e01f", codedWidth: 1920, codedHeight: 1080 });
    expect(decoders[0].chunks.map((c) => c.type)).toEqual(["key", "delta", "delta"]);
    decoders[0].chunks.forEach((c, i) => expect(Array.from(c.data)).toEqual(Array.from(units[i].data)));
    expect(presented.map((f) => f.timestamp)).toEqual(units.map((u) => 1_762_400_000_000_000 + u.frameSeq * 16_667));
    expect(presented.every((f) => f.closed)).toBe(true);
    expect(rt.getSnapshot()).toMatchObject({ firstFrame: true, wsOpen: true, pcConnected: true });

    rt.destroy();
    await flush();
    expect(agent.sent("bye")).toEqual([{ type: "bye", reason: "client_closed" }]);
  });

  it("re-configures the decoder on a new config_id, holding frames that beat their hello_ack", async () => {
    const { agent, decoders, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const pkt = { n: 0 };
    agent.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(100, 0) }, pkt));
    await vi.waitFor(() => expect(presented).toHaveLength(1));

    // An external resize: the keyframe under config 1 overtakes its hello_ack.
    agent.datagrams(packetize({ configId: 1, frameSeq: 1, keyframe: true, data: bytes(100, 1) }, pkt));
    await flush();
    expect(presented).toHaveLength(1);
    const resized = avcC(0x28);
    agent.control(helloAck(1, resized, 1280, 720));
    await vi.waitFor(() => expect(presented).toHaveLength(2));
    expect(decoders[0].configs).toHaveLength(2);
    expect(decoders[0].configs[1]).toMatchObject({ codec: "avc1.42e028", codedWidth: 1280, codedHeight: 720 });
    expect(decoders[0].chunks[1].type).toBe("key");
  });

  it("asks for a keyframe on loss", async () => {
    const { agent, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const pkt = { n: 0 };
    agent.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }, pkt));
    agent.datagrams(packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(3000, 1) }, pkt).slice(1));
    agent.datagrams(packetize({ configId: 0, frameSeq: 2, keyframe: false, data: bytes(10, 2) }, pkt));
    await vi.waitFor(() => expect(agent.sent("keyframe_request")).toEqual([{ type: "keyframe_request", reason: "loss" }]));
    expect(presented).toHaveLength(1);
  });

  it("treats close 4410 as a takeover", async () => {
    const { agent, callbacks, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.closeWith(4410);
    await vi.waitFor(() => expect(callbacks.onSessionTakenOver).toHaveBeenCalled());
    expect(rt.getSnapshot().channelOpen).toBe(false);
  });
});
