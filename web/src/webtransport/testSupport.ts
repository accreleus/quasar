// Test helpers: the agent's side of protocol/transport-webtransport.md as
// node-agent/src/webtransport/{mod,media}.rs speaks it, and a fake WebCodecs.
// Imported by tests only.

import { ControlDecoder, encodeControl, type VideoFragment } from "./framing";
import type { WebTransportCoords } from "../api/types";

const HEADER = 25;
const MAX_DATAGRAM = 1148;

export interface UnitSpec {
  configId: number;
  frameSeq: number;
  keyframe: boolean;
  data: Uint8Array;
  captureUs?: number;
  discardable?: boolean;
  ts90k?: number;
}

/** `media::packetize`: 25-byte header, in-order byte ranges of the unit. */
export function packetize(u: UnitSpec, pktSeq = { n: 0 }): Uint8Array[] {
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
    v.setUint32(13, u.ts90k ?? u.frameSeq * 1500, true);
    v.setBigUint64(17, BigInt(u.captureUs ?? 1_762_400_000_000_000 + u.frameSeq * 16_667), true);
    d.set(frag, HEADER);
    return d;
  });
}

/** `fec::repair` (node-agent/src/webtransport/fec.rs): the `0x04` datagram for fragments
 *  `first + i * stride` of `f`. */
export function repairOf(f: VideoFragment[], first: number, count: number, stride = 1): Uint8Array {
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
}

/**
 * `pyrowave::pack` (node-agent/src/webtransport/pyrowave.rs): one PyroWave frame's datagram
 * payloads, 12-byte PyroWave header first, packets packed greedily into `room` bytes, an
 * oversized block alone in filled parts, each datagram's coverage range partitioning [0, n).
 */
export function packPyroWave(frame: Uint8Array, n: number, room = MAX_DATAGRAM - HEADER): Uint8Array[] {
  const fit = room - 12;
  const v = new DataView(frame.buffer, frame.byteOffset, frame.byteLength);
  type Draft = { first: number; parts: boolean; bytes: Uint8Array[]; used: number };
  const drafts: Draft[] = [];
  for (let o = 0; o < frame.byteLength; ) {
    const w0 = v.getUint32(o, true);
    const len = w0 >>> 31 ? 8 : ((w0 >>> 16) & 0xfff) * 4;
    const start = w0 >>> 31 ? 0 : v.getUint32(o + 4, true) >>> 8;
    const p = frame.subarray(o, o + len);
    o += len;
    const last = drafts[drafts.length - 1];
    if (len > fit) drafts.push({ first: start, parts: true, bytes: [p], used: len });
    else if (last && !last.parts && last.used + len <= fit) {
      last.bytes.push(p);
      last.used += len;
    } else drafts.push({ first: drafts.length ? start : 0, parts: false, bytes: [p], used: len });
  }
  const out: Uint8Array[] = [];
  const head = (kind: number, part: number, count: number, first: number, end: number) => {
    const h = new Uint8Array(12);
    const hv = new DataView(h.buffer);
    h.set([kind, part, count]);
    hv.setUint32(4, first, true);
    hv.setUint32(8, end, true);
    return h;
  };
  const join = (...b: Uint8Array[]) => {
    const r = new Uint8Array(b.reduce((a, x) => a + x.length, 0));
    let at = 0;
    for (const x of b) (r.set(x, at), (at += x.length));
    return r;
  };
  drafts.forEach((d, i) => {
    const end = drafts[i + 1]?.first ?? n;
    if (!d.parts) {
      out.push(join(head(0, 0, 0, d.first, end), ...d.bytes));
      return;
    }
    const block = d.bytes[0];
    const count = Math.ceil(block.length / fit);
    for (let j = 0; j < count; j++) out.push(join(head(1, j, count, d.first, end), block.subarray(j * fit, (j + 1) * fit)));
  });
  return out;
}

/** `media::packetize` for a PyroWave unit: every frame keyframe + discardable, payloads from {@link packPyroWave}. */
export function pyroWaveDatagrams(
  u: { configId: number; frameSeq: number; captureUs?: number },
  payloads: Uint8Array[],
  pktSeq = { n: 0 },
): Uint8Array[] {
  return payloads.map((frag, index) => {
    const d = new Uint8Array(HEADER + frag.length);
    const v = new DataView(d.buffer);
    d[0] = 0x01;
    v.setUint32(1, pktSeq.n++, true);
    d[5] = 3;
    d[6] = u.configId;
    v.setUint16(7, u.frameSeq, true);
    v.setUint16(9, index, true);
    v.setUint16(11, payloads.length, true);
    v.setUint32(13, u.frameSeq * 1500, true);
    v.setBigUint64(17, BigInt(u.captureUs ?? 1_762_400_000_000_000 + u.frameSeq * 16_667), true);
    d.set(frag, HEADER);
    return d;
  });
}

export function bytes(n: number, seed: number): Uint8Array {
  return Uint8Array.from({ length: n }, (_, i) => (i * 7 + seed) & 0xff);
}

/** An avcC: version 1, constrained baseline (0x42 0xe0), the given level, 4-byte NAL lengths. */
export function avcC(level = 0x1f): Uint8Array {
  return Uint8Array.from([1, 0x42, 0xe0, level, 0xff, 0xe1, 0, 0]);
}

export const b64 = (u: Uint8Array) => btoa(String.fromCharCode(...u));

export function helloAck(configId: number, description: Uint8Array, width = 1920, height = 1080) {
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

export const COORDS: WebTransportCoords = {
  url: "https://agent.invalid:4433/wt",
  token: "tok-1",
  expires_at: "2026-11-06T12:01:00Z",
  cert_hashes: [{ algorithm: "sha-256", value: b64(Uint8Array.from([1, 2, 3, 4])) }],
};

/** One scripted reply step. Besides control messages: `teardown` fails the connection with
 *  no close code (Chrome's view of the agent's refusal racing its teardown); `close` closes
 *  it with a WebTransport close code. */
export type Step = Record<string, unknown> | { type: "teardown" } | { type: "close"; code: number };

export interface FakeConnection {
  options: WebTransportOptions;
  /** Every close() the client made on it. */
  closes: (WebTransportCloseInfo | undefined)[];
  datagrams(ds: Uint8Array[]): void;
  control(m: object): void;
  closeWith(code: number): void;
}

/** A scripted agent behind a fake browser `WebTransport`; every connection is kept. */
export class FakeAgent {
  conns: FakeConnection[] = [];
  received: Record<string, unknown>[] = [];
  /** Every datagram the client sent, across connections. */
  datagramsSent: Uint8Array[] = [];
  /** What the agent answers each hello with, in order; the last repeats. */
  replies: Step[][] = [];
  /** `ready` rejects: the handshake or the certificate failed. */
  refuseHandshake = false;
  /** `close()` does not settle `closed`, as a connection that never finishes closing. */
  closeNeverSettles = false;
  /** Control-stream writes fail from here on, as on a stream the agent reset. */
  controlReset = false;
  /** Park the client at one await of its connect until `release()`; `answer` means no reply. */
  hold: "ready" | "stream" | "write" | "answer" | null = null;
  /** Off: datagrams already in flight still arrive after close(), the case a stale attempt must ignore. */
  closeEndsDatagrams = true;
  /** The await the client is parked at, once it gets there. */
  reached: string | null = null;
  private gate: { promise: Promise<void>; open: () => void } | null = null;
  /** The reply held back by `hold = "answer"`, sent by `release()`. */
  private heldReply: (() => void) | null = null;
  private hellos = 0;

  private parked(stage: "ready" | "stream" | "write"): Promise<void> {
    if (this.hold !== stage) return Promise.resolve();
    this.reached = stage;
    let open!: () => void;
    const promise = new Promise<void>((r) => (open = r));
    this.gate = { promise, open };
    return promise;
  }

  release(): void {
    this.gate?.open();
    this.heldReply?.();
    this.heldReply = null;
  }

  create = (url: string, options: WebTransportOptions): WebTransport => {
    void url;
    const agent = this;
    const decoder = new ControlDecoder();
    let dgram: ReadableStreamDefaultController<Uint8Array> | null = null;
    let ctl: ReadableStreamDefaultController<Uint8Array> | null = null;
    let settle!: { resolve: (i: WebTransportCloseInfo) => void; reject: (e: Error) => void };
    const closed = new Promise<WebTransportCloseInfo>((resolve, reject) => (settle = { resolve, reject }));
    const conn: FakeConnection = {
      options,
      closes: [],
      datagrams: (ds) => ds.forEach((d) => dgram?.enqueue(d)),
      control: (m) => {
        try {
          ctl?.enqueue(encodeControl(m));
        } catch {
          /* the stream was closed: the message is lost, as on a closed connection */
        }
      },
      closeWith: (code) => {
        settle.resolve({ closeCode: code, reason: "" });
        try {
          ctl?.close();
        } catch {
          /* already closed */
        }
      },
    };
    this.conns.push(conn);
    const onHello = () => {
      if (this.hold === "answer" && !this.heldReply) {
        this.reached = "answer";
        this.heldReply = onHello;
        return;
      }
      const script = this.replies[Math.min(this.hellos++, this.replies.length - 1)] ?? [];
      for (const step of script) {
        if (step.type === "teardown") {
          ctl?.error(new Error("Connection lost"));
          settle.reject(new Error("Connection lost"));
        } else if (step.type === "close") conn.closeWith((step as { code: number }).code);
        else conn.control(step);
      }
    };
    let rejectReady: (e: Error) => void = () => {};
    const ready = this.refuseHandshake
      ? Promise.reject(new Error("Opening handshake failed."))
      : new Promise<void>((resolve, reject) => {
          rejectReady = reject;
          void this.parked("ready").then(resolve);
        });
    const wt = {
      ready,
      closed,
      datagrams: {
        readable: new ReadableStream<Uint8Array>({ start: (c) => void (dgram = c) }),
        writable: new WritableStream<Uint8Array>({ write: (chunk) => void agent.datagramsSent.push(chunk) }),
      },
      async createBidirectionalStream() {
        await agent.parked("stream");
        return {
          readable: new ReadableStream<Uint8Array>({ start: (c) => void (ctl = c) }),
          writable: new WritableStream<Uint8Array>({
            async write(chunk) {
              await agent.parked("write");
              if (agent.controlReset) throw new Error("stream reset");
              for (const m of decoder.push(chunk)) {
                agent.received.push(m);
                if (m.type === "hello") onHello();
              }
            },
          }),
        };
      },
      // As a browser does: closing ends the handshake and the connection's streams.
      close(info?: WebTransportCloseInfo) {
        conn.closes.push(info);
        rejectReady(new Error("closed"));
        if (!agent.closeNeverSettles) settle.resolve(info ?? { closeCode: 0, reason: "" });
        for (const c of agent.closeEndsDatagrams ? [ctl, dgram] : [ctl]) {
          try {
            c?.close();
          } catch {
            /* already closed */
          }
        }
      },
    };
    return wt as unknown as WebTransport;
  };

  get last(): FakeConnection {
    return this.conns[this.conns.length - 1];
  }

  sent(type: string): Record<string, unknown>[] {
    return this.received.filter((m) => m.type === type);
  }
}

export class FakeChunk {
  type: string;
  timestamp: number;
  data: Uint8Array;
  constructor(init: { type: string; timestamp: number; data: Uint8Array }) {
    this.type = init.type;
    this.timestamp = init.timestamp;
    this.data = new Uint8Array(init.data);
  }
}

export interface FakeFrame {
  timestamp: number;
  displayWidth: number;
  displayHeight: number;
  closed: boolean;
  close(): void;
}

export function fakeFrame(timestamp = 0): FakeFrame {
  return { timestamp, displayWidth: 1920, displayHeight: 1080, closed: false, close() { this.closed = true; } };
}

export class FakeDecoder {
  state = "unconfigured";
  decodeQueueSize = 0;
  configs: VideoDecoderConfig[] = [];
  chunks: FakeChunk[] = [];
  /** configure() throws, as a browser does for a configuration it cannot take. */
  refuseConfigure = false;
  /** Decoded frames wait for `emit()`: a decoder that has not caught up. */
  holdOutput = false;
  /** `decodeQueueSize` follows the frames waiting, as a real decoder's does; off, a test sets it. */
  queueFollowsWaiting = false;
  private waiting: FakeChunk[] = [];
  constructor(readonly init: VideoDecoderInit) {}
  configure(c: VideoDecoderConfig) {
    if (this.refuseConfigure) throw new DOMException("Unsupported configuration", "NotSupportedError");
    this.configs.push(c);
    this.state = "configured";
  }
  decode(chunk: FakeChunk) {
    this.chunks.push(chunk);
    if (this.holdOutput) {
      this.waiting.push(chunk);
      if (this.queueFollowsWaiting) this.decodeQueueSize = this.waiting.length;
    }
    else this.init.output(fakeFrame(chunk.timestamp) as unknown as VideoFrame);
  }
  emit() {
    for (const c of this.waiting.splice(0)) this.init.output(fakeFrame(c.timestamp) as unknown as VideoFrame);
    if (this.queueFollowsWaiting) this.decodeQueueSize = 0;
  }
  /** The decoder failing the way WebCodecs reports it: error callback, then closed. */
  fail() {
    this.state = "closed";
    this.init.error(new DOMException("Decoding error", "EncodingError"));
  }
  close() {
    this.state = "closed";
  }
}

export const flush = () => new Promise((r) => setTimeout(r, 0));
