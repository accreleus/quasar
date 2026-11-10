import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  BYE_FLUSH_MS,
  CONNECT_DEADLINE_MS,
  HANDSHAKE_DEADLINE_MS,
  KEYFRAME_SPACING_MS,
  REMINT_MIN_INTERVAL_MS,
  TOKEN_REFRESH_AGE_MS,
  WebTransportMedia,
} from "./webTransportMedia";
import type { InputMsg } from "../input/inputSender";
import type { Presenter } from "./presenter";
import { TransportError, type MediaSink, type MediaTransport, type TransportEnd } from "./transport";
import { createWebTransportRuntime, type WebTransportRuntimeDeps } from "../pages/app/webTransportRuntime";
import type { BenchDeps } from "../bench/benchController";
import { mintTransportToken } from "../api/library";
import {
  avcC,
  b64,
  bytes,
  COORDS,
  FakeAgent,
  FakeChunk,
  FakeDecoder,
  flush,
  helloAck,
  packetize,
  repairOf,
  type FakeFrame,
} from "./testSupport";
import { parseVideoDatagram } from "./framing";
import { FEC_HOLD_MS } from "./reassembly";

beforeEach(() => {
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

vi.mock("../api/library", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/library")>()),
  mintTransportToken: vi.fn(),
}));

const NO_SINK: MediaSink = { config: () => {}, unit: () => {}, pyrowave: () => {}, end: () => {} };

function transport(agent: FakeAgent, extra: Partial<ConstructorParameters<typeof WebTransportMedia>[0]> = {}) {
  return new WebTransportMedia({
    sessionId: "s1",
    coords: COORDS,
    mintToken: async () => ({ ...COORDS, token: "tok-2" }),
    createWebTransport: agent.create,
    sleep: async () => {},
    ...extra,
  });
}

// ── The transport ───────────────────────────────────────────────────────────

describe("WebTransportMedia connect", () => {
  it("passes every certificate hash, base64-decoded, and sends the contract's hello", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const second = { algorithm: "sha-256" as const, value: b64(Uint8Array.from([9, 9])) };
    await transport(agent, { coords: { ...COORDS, cert_hashes: [...COORDS.cert_hashes, second] } }).connect(NO_SINK);
    expect(agent.last.options.serverCertificateHashes!.map((h) => Array.from(h.value as Uint8Array))).toEqual([
      [1, 2, 3, 4],
      [9, 9],
    ]);
    expect(agent.sent("hello")[0]).toEqual({ type: "hello", wt_proto: 1, token: "tok-1", session_id: "s1", client: "browser", fec: ["xor"] });
  });

  it("sends no certificate hashes for an edge URL", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    await transport(agent, { coords: { ...COORDS, cert_hashes: [] } }).connect(NO_SINK);
    expect(agent.last.options).toEqual({});
  });

  it("retries session_not_ready with the same token and mints once on auth_failed", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "session_not_ready" }], [{ type: "error", code: "auth_failed" }], [helloAck(0, avcC())]];
    const mint = vi.fn(async () => ({ ...COORDS, token: "tok-2" }));
    await transport(agent, { mintToken: mint }).connect(NO_SINK);
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-1", "tok-2"]);
    expect(mint).toHaveBeenCalledTimes(1);
  });

  it("gives up after a second auth_failed", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "auth_failed" }]];
    await expect(transport(agent).connect(NO_SINK)).rejects.toMatchObject({ code: "auth_failed" });
  });

  it("retries a connection torn down before its answer could be read", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "teardown" }], [helloAck(0, avcC())]];
    await transport(agent).connect(NO_SINK);
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-1"]);
  });

  it.each([
    [4404, "session_ended"],
    [4400, "internal"],
    [4410, "taken_over"],
  ])("ends at once on close %i during connect (%s)", async (code, refusal) => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "close", code }]];
    await expect(transport(agent).connect(NO_SINK)).rejects.toMatchObject({ code: refusal });
    expect(agent.sent("hello")).toHaveLength(1);
  });

  it("re-mints once on close 4401 during connect", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "close", code: 4401 }], [helloAck(0, avcC())]];
    const mint = vi.fn(async () => ({ ...COORDS, token: "tok-2" }));
    await transport(agent, { mintToken: mint }).connect(NO_SINK);
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-2"]);
  });

  it("does not retry a failed handshake or certificate", async () => {
    const agent = new FakeAgent();
    agent.refuseHandshake = true;
    await expect(transport(agent).connect(NO_SINK)).rejects.toMatchObject({ code: "connect_failed" });
    expect(agent.conns).toHaveLength(1);
  });

  it("stops retrying at the connect deadline", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "session_not_ready" }]];
    let now = 0;
    const t = transport(agent, { sleep: async (ms) => void (now += ms * 40), now: () => now, mintToken: async () => COORDS });
    await expect(t.connect(NO_SINK)).rejects.toMatchObject({ code: "connect_timeout" });
    expect(now).toBeGreaterThanOrEqual(CONNECT_DEADLINE_MS);
  });

  it("closes the connection on every early return, and a stale one never feeds the sink", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "error", code: "session_not_ready" }], [{ type: "teardown" }], [helloAck(0, avcC())]];
    agent.closeEndsDatagrams = false;
    const units: number[] = [];
    await transport(agent).connect({ ...NO_SINK, unit: (u) => units.push(u.frameSeq) });
    expect(agent.conns.map((c) => c.closes.length > 0)).toEqual([true, true, false]);
    agent.conns[0].datagrams(packetize({ configId: 0, frameSeq: 7, keyframe: true, data: bytes(10, 7) }));
    agent.conns[2].datagrams(packetize({ configId: 0, frameSeq: 8, keyframe: true, data: bytes(10, 8) }));
    await vi.waitFor(() => expect(units).toEqual([8]));
    await flush();
    expect(units).toEqual([8]);
  });

  it("closes with 4400 on a malformed hello_ack", async () => {
    const agent = new FakeAgent();
    agent.replies = [[{ type: "hello_ack", wt_proto: 1, config_id: 0, video: null }]];
    await expect(transport(agent).connect(NO_SINK)).rejects.toMatchObject({ code: "internal" });
    expect(agent.last.closes[0]).toMatchObject({ closeCode: 4400 });
  });

  it("refuses a PyroWave hello_ack whose picture size no PWV1 header can state", async () => {
    const agent = new FakeAgent();
    const ack = helloAck(0, avcC(), 1_000_000, 1_000_000);
    agent.replies = [[{ ...ack, video: { ...ack.video, codec: "pyrowave" } }]];
    await expect(transport(agent, { pyrowave: true, ticker: () => () => {} }).connect(NO_SINK)).rejects.toMatchObject({ code: "internal" });
    expect(agent.last.closes[0]).toMatchObject({ closeCode: 4400 });
  });

  it("re-mints an old token after a failed attempt, by its own clock, and never twice within 10 s", async () => {
    const agent = new FakeAgent();
    const notReady = [{ type: "error", code: "session_not_ready" }];
    agent.replies = [...Array.from({ length: 24 }, () => notReady), [{ type: "error", code: "auth_failed" }]];
    let now = 0;
    const mint = vi.fn(async () => ({ ...COORDS, token: `tok-m${mint.mock.calls.length}` }));
    // The server's expiry is long past on this page's wall clock: it must not matter.
    const t = transport(agent, {
      coords: { ...COORDS, expires_at: "2000-01-01T00:00:00Z" },
      mintToken: mint,
      now: () => now,
      sleep: async (ms) => void (now += ms * 8), // 2 s per retry
    });
    // The auth_failed comes 2 s after the age-based mint: inside REMINT_MIN_INTERVAL_MS, so no second mint.
    await expect(t.connect(NO_SINK)).rejects.toMatchObject({ code: "auth_failed" });
    const tokens = agent.sent("hello").map((h) => h.token);
    expect(tokens.indexOf("tok-m1")).toBe(Math.ceil(TOKEN_REFRESH_AGE_MS / 2_000));
    expect(tokens.slice(0, tokens.indexOf("tok-m1")).every((tok) => tok === "tok-1")).toBe(true);
    expect(mint).toHaveBeenCalledTimes(1);
    expect(REMINT_MIN_INTERVAL_MS).toBeGreaterThan(2_000);
  });
});

describe("WebTransportMedia close during connect", () => {
  it.each(["ready", "stream", "write", "answer"] as const)(
    "closes the connection when closed while connect awaits %s, and never resolves with it",
    async (stage) => {
      const agent = new FakeAgent();
      agent.replies = [[helloAck(0, avcC())]];
      agent.hold = stage;
      const t = transport(agent);
      const sinkCalls: string[] = [];
      const connecting = t.connect({ ...NO_SINK, config: () => sinkCalls.push("config") });
      await vi.waitFor(() => expect(agent.reached).toBe(stage));
      t.close();
      expect(agent.last.closes.length).toBeGreaterThan(0); // at once, not when the agent next answers
      agent.release();
      await expect(connecting).rejects.toMatchObject({ code: "closed" });
      expect(agent.conns).toHaveLength(1);
      expect(agent.last.closes.length).toBeGreaterThan(0);
      expect(sinkCalls).toEqual([]);
    },
  );
});

describe("WebTransportMedia close as the answer lands", () => {
  it("does not publish a connection whose hello_ack arrives with close()", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    agent.hold = "answer";
    const t = transport(agent);
    const sinkCalls: string[] = [];
    const connecting = t.connect({ ...NO_SINK, config: () => sinkCalls.push("config") });
    await vi.waitFor(() => expect(agent.reached).toBe("answer"));
    agent.release(); // the hello_ack is queued on the stream...
    t.close(); // ...and the page closes before the attempt reads it
    await expect(connecting).rejects.toMatchObject({ code: "closed" });
    expect(sinkCalls).toEqual([]);
    expect(agent.last.closes.length).toBeGreaterThan(0);
  });
});

describe("WebTransportMedia keyframe requests", () => {
  it(`sends at most one per ${KEYFRAME_SPACING_MS} ms, with the last asked sent at the end of the window`, async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    let now = 1_000;
    const t = transport(agent, { now: () => now });
    const counted: string[] = [];
    await t.connect({ ...NO_SINK, keyframeRequested: (r) => void counted.push(r) });
    vi.useFakeTimers();
    t.requestKeyframe("loss");
    t.requestKeyframe("late");
    t.requestKeyframe("decode_error");
    await vi.advanceTimersByTimeAsync(0);
    expect(agent.sent("keyframe_request").map((k) => k.reason)).toEqual(["loss"]);
    now += KEYFRAME_SPACING_MS;
    await vi.advanceTimersByTimeAsync(KEYFRAME_SPACING_MS);
    expect(agent.sent("keyframe_request").map((k) => k.reason)).toEqual(["loss", "decode_error"]);
    // Telemetry counts what went on the wire, not what was asked.
    expect(counted).toEqual(["loss", "decode_error"]);
  });
});

describe("WebTransportMedia loss recovery", () => {
  it("repeats a NACK and gives the frame up on its timer when no datagram follows", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const t = transport(agent);
    await t.connect(NO_SINK);
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }, pkt));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(2000, 1) }, pkt).slice(1));
    await vi.waitFor(() => expect(agent.sent("nack")).toHaveLength(1));
    // Nothing more arrives: a still picture. The repeat and the lateness come off the timer.
    await vi.waitFor(() => expect(agent.sent("nack")).toHaveLength(2));
    await vi.waitFor(() => expect(agent.sent("keyframe_request")).toEqual([{ type: "keyframe_request", reason: "late" }]));
    t.close();
  });

  it(`sends a NACK held for FEC ${FEC_HOLD_MS} ms after the gap showed, with no datagram to carry it`, async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    let now = 1_000;
    const t = transport(agent, { now: () => now });
    await t.connect(NO_SINK);
    vi.useFakeTimers();
    const pkt = { n: 0 };
    const key = packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(3000, 0) }, pkt);
    // A whole frame and its repair (the agent is sending FEC), then a frame with a gap.
    agent.last.datagrams([...key, repairOf(key.map((d) => parseVideoDatagram(d)!), 0, 3)]);
    const gapped = packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(3000, 1) }, pkt);
    agent.last.datagrams([gapped[0], gapped[2]]);
    await vi.advanceTimersByTimeAsync(0);
    expect(agent.sent("nack")).toEqual([]);
    now += FEC_HOLD_MS;
    await vi.advanceTimersByTimeAsync(FEC_HOLD_MS);
    expect(agent.sent("nack")).toEqual([{ type: "nack", frame_seq: 1, frags: [[1, 1]] }]);
    t.close();
  });
});

describe("WebTransportMedia receiver feedback", () => {
  it("reports the datagrams that arrived on the control stream, in the contract's shape", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const t = transport(agent);
    await t.connect(NO_SINK);
    const pkt = { n: 7 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(3000, 0) }, pkt));
    await vi.waitFor(() => expect(agent.sent("feedback")).toHaveLength(1));
    const [fb] = agent.sent("feedback");
    expect(fb).toMatchObject({ seq: 1, first_pkt_seq: 7, count: pkt.n - 7, lost: 0 });
    expect(typeof fb.arrivals_b64).toBe("string");
    t.close();
  });

  it("reports on the transport's ticker, the Worker clock a hidden tab keeps running", async () => {
    const periods: number[] = [];
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const t = transport(agent, {
      ticker: (ms) => {
        periods.push(ms);
        return () => {};
      },
    });
    await t.connect(NO_SINK);
    expect(periods).toEqual([1000, 50]);
    t.close();
  });
});

// ── The runtime, end to end against the fake agent ─────────────────────────

describe("createWebTransportRuntime", () => {
  function setup(opts: { draws?: boolean; noWebGpu?: boolean; deps?: Partial<WebTransportRuntimeDeps> } = {}) {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const decoders: FakeDecoder[] = [];
    const presented: FakeFrame[] = [];
    let presenterFatal: ((m: string) => void) | null = null;
    const presenter: Presenter = {
      present: (f) => {
        presented.push(f as unknown as FakeFrame);
        f.close();
        return opts.draws ?? true;
      },
      destroy: vi.fn(),
    };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async (_c, onFatal) => {
          presenterFatal = onFatal;
          return opts.noWebGpu ? null : presenter;
        },
        decoder: {
          createDecoder: (init) => {
            const d = new FakeDecoder(init);
            decoders.push(d);
            return d as unknown as VideoDecoder;
          },
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
        // No network from a unit test: the trace and the stats posts go nowhere.
        createTracer: () => ({ start() {}, stop() {}, emitBenchWindow() {} }),
        telemetry: { fetch: async () => new Response("{}") },
        ...opts.deps,
      },
    });
    return { agent, decoders, presented, presenter, callbacks, rt, presenterFatal: () => presenterFatal! };
  }

  it("in bench mode gives the marker reader each decoded frame before the presenter takes it, and posts its windows through the trace (#1035)", async () => {
    const read: { width: number; closed: boolean }[] = [];
    const windows: unknown[] = [];
    const tracer = { start: vi.fn(), stop: vi.fn(), emitBenchWindow: (p: unknown) => void windows.push(p) };
    class FakeBench {
      constructor(private readonly deps: BenchDeps) {}
      start() {}
      // The real one flushes its last window on stop.
      stop() {
        this.deps.emitWindow({ n: read.length } as never);
      }
      handle() {
        return { enabled: true };
      }
      onFrame() {
        const src = this.deps.source!()!;
        read.push({ width: src.width, closed: (src.image as unknown as FakeFrame).closed });
      }
    }
    const { agent, presented, rt } = setup({
      deps: {
        isBenchModeEnabled: () => true,
        loadBench: async () => ({ BenchController: FakeBench as never }),
        createTracer: () => tracer,
      },
    });
    rt.start();
    await vi.waitFor(() => expect((window as unknown as Record<string, unknown>).__qBench).toEqual({ enabled: true }));
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(900, 0) }, pkt));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(900, 1) }, pkt));
    await vi.waitFor(() => expect(presented).toHaveLength(2));
    // Read at the frame's own size while it was still open; the presenter closed it afterwards.
    expect(read).toEqual([{ width: 1920, closed: false }, { width: 1920, closed: false }]);
    expect(tracer.start).toHaveBeenCalledTimes(1);
    rt.destroy();
    expect(windows).toEqual([{ n: 2 }]);
    expect(tracer.stop).toHaveBeenCalledTimes(1);
    delete (window as unknown as Record<string, unknown>).__qBench;
  });

  it("keeps presenting when the bench instrument throws", async () => {
    class BrokenBench {
      start() {}
      stop() {}
      handle() {
        return { enabled: true };
      }
      onFrame() {
        throw new Error("marker decode failed");
      }
    }
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const { agent, presented, rt } = setup({
      deps: { isBenchModeEnabled: () => true, loadBench: async () => ({ BenchController: BrokenBench as never }) },
    });
    rt.start();
    await vi.waitFor(() => expect((window as unknown as Record<string, unknown>).__qBench).toEqual({ enabled: true }));
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(900, 0) }, pkt));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(900, 1) }, pkt));
    await vi.waitFor(() => expect(presented).toHaveLength(2));
    expect(presented.every((f) => f.closed)).toBe(true);
    // Said once, not per frame.
    expect(warn.mock.calls.filter((c) => c[0] === "bench:")).toHaveLength(1);
    rt.destroy();
    delete (window as unknown as Record<string, unknown>).__qBench;
  });

  it("opens the first telemetry window at connect, and reports a fragment it had to NACK as lost (#1035)", async () => {
    // The telemetry's clock, so the windows are exact whatever the real timer does.
    let now = 0;
    const snaps: { fps: number; packetsLost: number }[] = [];
    const { agent, presented, rt } = setup({ deps: { telemetry: { nowMs: () => now, fetch: async () => new Response("{}") } } });
    now = 5_000; // made, then five seconds getting a presenter, a token and a connection
    rt.registerTelemetry((s) => snaps.push(s));
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }, pkt));
    const one = packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(3000, 1) }, pkt);
    agent.last.datagrams(one.slice(1)); // its first fragment is lost
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 2, keyframe: false, data: bytes(10, 2) }, pkt));
    await vi.waitFor(() => expect(agent.sent("nack")).toHaveLength(1));
    const resent = one[0].slice();
    resent[5] |= 4;
    agent.last.datagrams([resent]);
    await vi.waitFor(() => expect(presented).toHaveLength(3));
    now = 6_000;
    await vi.waitFor(() => expect(snaps.length).toBeGreaterThan(0), { timeout: 3_000 });
    // Three frames in the one second since the ticks started: not three over six.
    expect(snaps[0].fps).toBeCloseTo(3, 6);
    expect(snaps[0].packetsLost).toBe(1);
    rt.destroy();
  });

  it("starts no bench instrument for an ordinary session", async () => {
    const loadBench = vi.fn();
    const { rt } = setup({ deps: { loadBench } });
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(loadBench).not.toHaveBeenCalled();
    rt.destroy();
  });

  it("connects with the contract's hello, decodes reassembled frames and presents each one", async () => {
    const { agent, decoders, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const units = [
      { configId: 0, frameSeq: 0, keyframe: true, data: bytes(5000, 0) },
      { configId: 0, frameSeq: 1, keyframe: false, data: bytes(700, 1) },
      { configId: 0, frameSeq: 2, keyframe: false, data: bytes(2400, 2) },
    ];
    const pkt = { n: 0 };
    // Fragments of a frame arrive reversed: datagrams are unordered.
    for (const u of units) agent.last.datagrams(packetize(u, pkt).reverse());
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
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(100, 0) }, pkt));
    await vi.waitFor(() => expect(presented).toHaveLength(1));
    agent.last.datagrams(packetize({ configId: 1, frameSeq: 1, keyframe: true, data: bytes(100, 1) }, pkt));
    await flush();
    expect(presented).toHaveLength(1);
    agent.last.control(helloAck(1, avcC(0x28), 1280, 720));
    await vi.waitFor(() => expect(presented).toHaveLength(2));
    expect(decoders[0].configs[1]).toMatchObject({ codec: "avc1.42e028", codedWidth: 1280, codedHeight: 720 });
    expect(decoders[0].chunks[1].type).toBe("key");
  });

  it("NACKs a lost fragment, presents the frame on its retransmission, and asks for a keyframe on nack_miss", async () => {
    const { agent, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }, pkt));
    const one = packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(3000, 1) }, pkt);
    agent.last.datagrams(one.slice(1));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 2, keyframe: false, data: bytes(10, 2) }, pkt));
    await vi.waitFor(() => expect(agent.sent("nack")).toEqual([{ type: "nack", frame_seq: 1, frags: [[0, 0]] }]));
    expect(presented).toHaveLength(1);
    const resent = one[0].slice();
    resent[5] |= 4;
    agent.last.datagrams([resent]);
    await vi.waitFor(() => expect(presented).toHaveLength(3));

    const four = packetize({ configId: 0, frameSeq: 3, keyframe: false, data: bytes(2000, 3) }, pkt);
    agent.last.datagrams(four.slice(0, 1));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 4, keyframe: false, data: bytes(10, 4) }, pkt));
    await vi.waitFor(() => expect(agent.sent("nack")).toHaveLength(2));
    agent.last.control({ type: "nack_miss", frame_seq: 3, frags: [[1, 1]] });
    await vi.waitFor(() => expect(agent.sent("keyframe_request")).toEqual([{ type: "keyframe_request", reason: "late" }]));
    expect(presented).toHaveLength(3);
  });

  it("treats close 4410 as a takeover", async () => {
    const { agent, callbacks, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4410);
    await vi.waitFor(() => expect(callbacks.onSessionTakenOver).toHaveBeenCalled());
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
    expect(callbacks.onFallback).not.toHaveBeenCalled();
    expect(rt.getSnapshot().channelOpen).toBe(false);
  });

  it("ends the view with its reason when the session ends mid-stream (close 4404)", async () => {
    const { agent, callbacks, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }));
    await vi.waitFor(() => expect(presented).toHaveLength(1));
    agent.last.closeWith(4404);
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
    expect(callbacks.onDisconnectSuspected).toHaveBeenCalled();
    expect(callbacks.onTransportFailed.mock.calls[0][0]).toMatch(/session ended/);
    expect(callbacks.onFallback).not.toHaveBeenCalled();
    expect(rt.getSnapshot()).toMatchObject({ channelOpen: false, recovery: { phase: "failed" } });
  });

  it("reports a browser with no WebGPU adapter", async () => {
    const { callbacks, rt } = setup({ noWebGpu: true });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("decoder_unsupported"));
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
  });

  it("reports a presenter failure, and counts no frame it did not draw", async () => {
    const { agent, callbacks, rt, presented, presenterFatal } = setup({ draws: false });
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }));
    await vi.waitFor(() => expect(presented).toHaveLength(1));
    expect(rt.getSnapshot().firstFrame).toBe(false);
    vi.spyOn(console, "warn").mockImplementation(() => {});
    presenterFatal()("the GPU device was lost");
    // The fallback follows the close (control-api.md §Transport fallback, "Close first").
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("internal"));
  });

  it("reports a presenter that cannot be made, and builds no transport", async () => {
    const agent = new FakeAgent();
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const createTransport = vi.fn();
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: { createTransport, createPresenter: () => Promise.reject(new Error("requestDevice failed")) },
    });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("internal"));
    expect(createTransport).not.toHaveBeenCalled();
    expect(agent.conns).toHaveLength(0);
  });

  it("does not connect when the presenter fails while it is being made", async () => {
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const createTransport = vi.fn();
    const destroy = vi.fn();
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: {
        createTransport,
        createPresenter: async (_c, onFatal) => {
          onFatal("the GPU device was lost");
          return { present: () => false, destroy };
        },
      },
    });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledTimes(1));
    await flush();
    expect(createTransport).not.toHaveBeenCalled();
    expect(destroy).toHaveBeenCalled();
  });

  it("calls the page back for nothing once destroyed", async () => {
    let refuse!: (e: unknown) => void;
    let sink!: MediaSink;
    const fake: MediaTransport = {
      connect: (s) => {
        sink = s;
        return new Promise((_, reject) => (refuse = reject));
      },
      requestKeyframe: () => {},
      sendMic: () => false,
      codecFallback: () => {},
      input: { reliable: () => {}, datagram: () => false },
      close: () => {},
    };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    const presenter: Presenter = { present: () => true, destroy: () => {} };
    const make = () =>
      createWebTransportRuntime({
        sessionId: "s1",
        authToken: "auth",
        coords: COORDS,
        canvas: document.createElement("canvas"),
        callbacks,
        deps: { createTransport: () => fake, createPresenter: async () => presenter },
      });
    for (const code of ["taken_over", "session_ended", "auth_failed"]) {
      const rt = make();
      rt.start();
      await vi.waitFor(() => expect(refuse).toBeDefined());
      rt.destroy();
      refuse(new TransportError(code));
      sink.end({ code: 4410, reason: "" });
      sink.end({ code: 4404, reason: "" });
      await flush();
      refuse = undefined as never;
    }
    expect(callbacks.onSessionTakenOver).not.toHaveBeenCalled();
    expect(callbacks.onDisconnectSuspected).not.toHaveBeenCalled();
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
    expect(callbacks.onFallback).not.toHaveBeenCalled();
  });

  it("falls back, not fails, when the browser cannot decode the stream", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    const presenter: Presenter = { present: () => true, destroy: () => {} };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async () => presenter,
        decoder: { isConfigSupported: async (c) => ({ supported: false, config: c }) },
      },
    });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("decoder_unsupported"));
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
  });

  it("asks for one keyframe when the tab becomes visible again", async () => {
    const { agent, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const vis = vi.spyOn(document, "visibilityState", "get");
    vis.mockReturnValue("hidden");
    document.dispatchEvent(new Event("visibilitychange"));
    vis.mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.waitFor(() => expect(agent.sent("keyframe_request")).toEqual([{ type: "keyframe_request", reason: "loss" }]));
    rt.destroy();
  });
});

describe("WebTransportMedia close once connected", () => {
  async function connected(agent: FakeAgent, sink: Partial<MediaSink> = {}) {
    agent.replies = [[helloAck(0, avcC())]];
    const t = transport(agent, { ticker: () => () => {} });
    await t.connect({ ...NO_SINK, ...sink });
    return t;
  }
  const keyframe = (frameSeq: number) => packetize({ configId: 0, frameSeq, keyframe: true, data: bytes(10, frameSeq) });

  it.each([
    ["reset", (a: FakeAgent) => void (a.controlReset = true)],
    ["blocked", (a: FakeAgent) => void (a.hold = "write")],
  ])("closes the session though its control stream is %s and the bye cannot leave", async (_, breakControl) => {
    vi.useFakeTimers();
    const agent = new FakeAgent();
    const t = await connected(agent);
    breakControl(agent);
    t.close();
    await vi.advanceTimersByTimeAsync(BYE_FLUSH_MS);
    expect(agent.last.closes).toEqual([{ closeCode: 0, reason: "" }]);
  });

  it("feeds the sink nothing after close(), from datagrams already in flight", async () => {
    const agent = new FakeAgent();
    agent.closeEndsDatagrams = false;
    const units: number[] = [];
    const t = await connected(agent, { unit: (u) => units.push(u.frameSeq) });
    agent.last.datagrams(keyframe(1));
    await vi.waitFor(() => expect(units).toEqual([1]));
    t.close();
    agent.last.datagrams(keyframe(2));
    await flush();
    expect(units).toEqual([1]);
  });

  it("ends the connection when the datagram path throws, not left up with no video", async () => {
    const agent = new FakeAgent();
    const ends: TransportEnd[] = [];
    await connected(agent, {
      unit: () => {
        throw new Error("boom");
      },
      end: (e) => ends.push(e),
    });
    agent.last.datagrams(keyframe(1));
    await vi.waitFor(() => expect(ends).toEqual([{ code: 4400, reason: "boom" }]));
  });
});

// ── Fallback to WebRTC (control-api.md §Transport fallback) ─────────────────

describe("WebTransportMedia loss-recovery timer", () => {
  /** Connects with only the recovery tick as an interval, and returns the handle of that tick. */
  async function connected(agent: FakeAgent) {
    vi.useFakeTimers();
    agent.replies = [[helloAck(0, avcC())]];
    const set = vi.spyOn(globalThis, "setInterval");
    const cleared = vi.spyOn(globalThis, "clearInterval");
    const t = transport(agent, { ticker: () => () => {} });
    await t.connect(NO_SINK);
    expect(set).toHaveBeenCalledTimes(1);
    return { t, tick: set.mock.results[0].value, cleared };
  }

  it("is stopped by close(), not left to a `closed` that a connection may never report", async () => {
    const agent = new FakeAgent();
    agent.closeNeverSettles = true;
    const { t, tick, cleared } = await connected(agent);
    t.close();
    expect(cleared).toHaveBeenCalledWith(tick);
  });

  it("is stopped when the connection closes by itself", async () => {
    const agent = new FakeAgent();
    const { tick, cleared } = await connected(agent);
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(cleared).toHaveBeenCalledWith(tick));
  });
});

describe("WebTransportMedia unreachable agent", () => {
  it("gives up on a handshake that never finishes, well before the connect deadline", async () => {
    vi.useFakeTimers();
    const agent = new FakeAgent();
    agent.hold = "ready";
    const result = transport(agent).connect(NO_SINK).catch((e: TransportError) => e);
    await vi.advanceTimersByTimeAsync(HANDSHAKE_DEADLINE_MS + 1);
    expect(await result).toMatchObject({ code: "connect_timeout" });
    expect(HANDSHAKE_DEADLINE_MS).toBeLessThan(CONNECT_DEADLINE_MS);
    expect(agent.conns[0].closes).toHaveLength(1);
  });

  it("keeps retrying a host that completed the handshake, up to the connect deadline", async () => {
    // Not ready, or an answer Chrome dropped: both are a reachable host, so neither counts toward
    // the handshake bound.
    const agent = new FakeAgent();
    agent.replies = [[{ type: "teardown" }]];
    let now = 0;
    const clock = { sleep: async (ms: number) => void (now += ms * 40), now: () => now };
    await expect(transport(agent, clock).connect(NO_SINK)).rejects.toMatchObject({ code: "connect_timeout" });
    expect(now).toBeGreaterThanOrEqual(CONNECT_DEADLINE_MS);
  });
});

describe("createWebTransportRuntime fallback", () => {
  function setup() {
    /** One per connection the runtime brought up; `sendInput` is what that capture sends with. */
    const captures: { sendInput: (m: InputMsg) => void; cleanup: ReturnType<typeof vi.fn>; engage: ReturnType<typeof vi.fn> }[] = [];
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const presenter: Presenter = { present: (f) => (f.close(), true), destroy: vi.fn() };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async () => presenter,
        decoder: {
          createDecoder: (init) => new FakeDecoder(init) as unknown as VideoDecoder,
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
        setupCapture: ((o: { sendInput: (m: InputMsg) => void }) => {
          const c = { sendInput: o.sendInput, cleanup: vi.fn(), engage: vi.fn(() => null), release: vi.fn(), syncKeyboardLock: vi.fn(), setMouse: vi.fn() };
          captures.push(c);
          return c;
        }) as never,
      },
    });
    return { agent, callbacks, rt, captures };
  }

  beforeEach(() => {
    vi.mocked(mintTransportToken).mockReset();
    vi.mocked(mintTransportToken).mockResolvedValue({
      transport_options: { webtransport: { ...COORDS, token: "tok-2" } },
    } as never);
  });

  it("connect refused: falls back with connect_failed and the connection closed", async () => {
    const { agent, callbacks, rt } = setup();
    agent.refuseHandshake = true;
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("connect_failed"));
    expect(agent.conns).toHaveLength(1);
    expect(agent.sent("hello")).toHaveLength(0);
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
    expect(rt.getSnapshot().status).toMatch(/falling back to WebRTC/);
  });

  it("connect timeout: falls back with connect_timeout", async () => {
    vi.useFakeTimers();
    const { agent, callbacks, rt } = setup();
    agent.hold = "ready";
    rt.start();
    await vi.advanceTimersByTimeAsync(HANDSHAKE_DEADLINE_MS + 1);
    expect(callbacks.onFallback).toHaveBeenCalledWith("connect_timeout");
    expect(callbacks.onFallback).toHaveBeenCalledTimes(1);
  });

  it("handshake version mismatch: falls back with unsupported_version", async () => {
    const { agent, callbacks, rt } = setup();
    agent.replies = [[{ ...helloAck(0, avcC()), wt_proto: 2 }]];
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("unsupported_version"));
    expect(agent.conns[0].closes).toHaveLength(1);
  });

  it("hello refused: falls back with the agent's reason, after one re-mint for auth_failed", async () => {
    const { agent, callbacks, rt } = setup();
    agent.replies = [[{ type: "error", code: "auth_failed" }]];
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("auth_failed"));
    expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-2"]);

    const other = setup();
    other.agent.replies = [[{ type: "error", code: "takeover_rate_limited" }]];
    other.rt.start();
    await vi.waitFor(() => expect(other.callbacks.onFallback).toHaveBeenCalledWith("takeover_rate_limited"));
  });

  it("a code outside the contract's vocabulary is reported as internal", async () => {
    const { agent, callbacks, rt } = setup();
    agent.replies = [[{ type: "error", code: "bad_request" }]];
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("internal"));
  });

  it("mid-session loss: one reconnect on a fresh token, then falls back with stream_lost", async () => {
    const { agent, callbacks, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(agent.sent("hello").map((h) => h.token)).toEqual(["tok-1", "tok-2"]));
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(callbacks.onFallback).not.toHaveBeenCalled();
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("stream_lost"));
    expect(callbacks.onFallback).toHaveBeenCalledTimes(1);
  });

  it("a reconnect that cannot be made reports stream_lost, whatever refused it", async () => {
    const { agent, callbacks, rt } = setup();
    vi.mocked(mintTransportToken).mockRejectedValue(new Error("409"));
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("stream_lost"));

    const refused = setup();
    refused.rt.start();
    await vi.waitFor(() => expect(refused.rt.getSnapshot().channelOpen).toBe(true));
    refused.agent.replies = [[{ type: "error", code: "takeover_rate_limited" }]];
    refused.agent.last.closeWith(4400);
    await vi.waitFor(() => expect(refused.callbacks.onFallback).toHaveBeenCalledWith("stream_lost"));
    expect(refused.callbacks.onFallback).toHaveBeenCalledTimes(1);
  });

  it("opens no connection from a reconnect whose mint was in flight when the view was destroyed", async () => {
    const { agent, callbacks, rt } = setup();
    let release!: () => void;
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    vi.mocked(mintTransportToken).mockReturnValue(
      new Promise((r) => (release = () => r({ transport_options: { webtransport: { ...COORDS, token: "tok-2" } } } as never))),
    );
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(release).toBeDefined());
    rt.destroy();
    release();
    await flush();
    expect(agent.conns).toHaveLength(1);
    expect(callbacks.onFallback).not.toHaveBeenCalled();
  });

  it("opens no connection from a reconnect whose mint was in flight when the view failed", async () => {
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    let fatal!: (m: string) => void;
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn(), onFallback: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async (_c, onFatal) => ((fatal = onFatal), { present: () => true, destroy: () => {} }),
        decoder: {
          createDecoder: (init) => new FakeDecoder(init) as unknown as VideoDecoder,
          isConfigSupported: async (c) => ({ supported: true, config: c }),
        },
      },
    });
    let release!: () => void;
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    vi.mocked(mintTransportToken).mockReturnValue(
      new Promise((r) => (release = () => r({ transport_options: { webtransport: COORDS } } as never))),
    );
    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(release).toBeDefined());
    fatal("the GPU device was lost");
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("internal"));
    release();
    await flush();
    expect(agent.conns).toHaveLength(1);
    expect(callbacks.onFallback).toHaveBeenCalledTimes(1);
  });

  it("does not ask the page to fall back until the WebTransport session has been told to close (#1250)", async () => {
    let sink!: MediaSink;
    let closed!: () => void;
    const onFallback = vi.fn();
    const fake: MediaTransport = {
      connect: async (s) => void (sink = s),
      requestKeyframe: () => {},
      sendMic: () => false,
      codecFallback: () => {},
      input: { reliable: () => {}, datagram: () => false },
      // A close that is still flushing its bye, or waiting out its bound.
      close: () => new Promise<void>((r) => (closed = r)),
    };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks: { onDisconnectSuspected: () => {}, onSessionTakenOver: () => {}, onTransportFailed: () => {}, onFallback },
      deps: { createTransport: () => fake, createPresenter: async () => ({ present: () => true, destroy: () => {} }) },
    });
    rt.start();
    await vi.waitFor(() => expect(sink).toBeDefined());
    vi.mocked(mintTransportToken).mockRejectedValue(new Error("409"));
    sink.end({ code: null, reason: "" });
    await vi.waitFor(() => expect(closed).toBeDefined());
    await flush();
    expect(onFallback).not.toHaveBeenCalled();
    closed();
    await vi.waitFor(() => expect(onFallback).toHaveBeenCalledTimes(1));
  });

  it("closes WebTransport before it asks the page to fall back", async () => {
    let sink!: MediaSink;
    const order: string[] = [];
    const fake: MediaTransport = {
      connect: async (s) => void (sink = s),
      requestKeyframe: () => {},
      sendMic: () => false,
      codecFallback: () => {},
      input: { reliable: () => {}, datagram: () => false },
      close: () => void order.push("close"),
    };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: document.createElement("canvas"),
      callbacks: {
        onDisconnectSuspected: () => {},
        onSessionTakenOver: () => {},
        onTransportFailed: () => {},
        onFallback: () => void order.push("fallback"),
      },
      deps: { createTransport: () => fake, createPresenter: async () => ({ present: () => true, destroy: () => {} }) },
    });
    rt.start();
    await vi.waitFor(() => expect(sink).toBeDefined());
    vi.mocked(mintTransportToken).mockRejectedValue(new Error("409"));
    sink.end({ code: null, reason: "" });
    await vi.waitFor(() => expect(order).toContain("fallback"));
    expect(order.indexOf("close")).toBeLessThan(order.indexOf("fallback"));
  });

  it("does not fall back when the session ended or another device took it", async () => {
    for (const code of [4404, 4410]) {
      const { agent, callbacks, rt } = setup();
      rt.start();
      await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
      agent.last.closeWith(code);
      await vi.waitFor(() => expect(callbacks.onTransportFailed.mock.calls.length + callbacks.onSessionTakenOver.mock.calls.length).toBe(1));
      expect(callbacks.onFallback).not.toHaveBeenCalled();
    }
  });

  const key = { t: "k", c: "KeyA", d: 1 } as InputMsg;
  const inputs = (agent: FakeAgent) => agent.received.filter((m) => m.type === "input");

  it("rebuilds input capture on a reconnect: the old one is released, the new one sends on the new connection", async () => {
    const { agent, rt, captures } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    expect(captures).toHaveLength(1);
    captures[0].sendInput(key);
    await vi.waitFor(() => expect(inputs(agent)).toHaveLength(1));

    agent.last.closeWith(4400);
    await vi.waitFor(() => expect(captures).toHaveLength(2));
    expect(captures[0].cleanup).toHaveBeenCalledTimes(1);
    expect(captures[1].cleanup).not.toHaveBeenCalled();
    expect(agent.conns).toHaveLength(2);

    // A late handler of the old capture reaches nothing; the new one reaches the new connection.
    captures[0].sendInput(key);
    await flush();
    expect(inputs(agent)).toHaveLength(1);
    captures[1].sendInput({ t: "k", c: "KeyB", d: 1 } as InputMsg);
    await vi.waitFor(() => expect(inputs(agent)).toHaveLength(2));
    expect(inputs(agent)[1]).toEqual({ type: "input", event: { t: "k", c: "KeyB", d: 1 } });
    expect(rt.engage()).toBeNull();
    expect(captures[1].engage).toHaveBeenCalled();
    expect(captures[0].engage).not.toHaveBeenCalled();
  });

  it("tears input capture down on fallback: nothing is sent over the dead connection and the page sees no capture", async () => {
    const { agent, callbacks, rt, captures } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4400); // first loss: one reconnect
    await vi.waitFor(() => expect(captures).toHaveLength(2));
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4400); // second loss: fallback, from a connection with a live capture
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("stream_lost"));
    expect(captures[1].cleanup).toHaveBeenCalledTimes(1);
    const before = inputs(agent).length;
    captures[1].sendInput(key);
    await flush();
    expect(inputs(agent)).toHaveLength(before);
    expect(rt.engage()).toBeNull();
    expect(captures[1].engage).not.toHaveBeenCalled();
    expect(rt.getSnapshot()).toMatchObject({ inputCaptured: false, pointerLocked: false, channelOpen: false });
  });

  it("builds no capture for a connection that never came up", async () => {
    const { agent, callbacks, rt, captures } = setup();
    agent.refuseHandshake = true;
    rt.start();
    await vi.waitFor(() => expect(callbacks.onFallback).toHaveBeenCalledWith("connect_failed"));
    expect(captures).toHaveLength(0);
  });
});
