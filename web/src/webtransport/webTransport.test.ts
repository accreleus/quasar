import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  CONNECT_DEADLINE_MS,
  KEYFRAME_SPACING_MS,
  REMINT_MIN_INTERVAL_MS,
  TOKEN_REFRESH_AGE_MS,
  WebTransportMedia,
} from "./webTransportMedia";
import type { Presenter } from "./presenter";
import { TransportError, type MediaSink, type MediaTransport } from "./transport";
import { createWebTransportRuntime } from "../pages/app/webTransportRuntime";
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
  type FakeFrame,
} from "./testSupport";

beforeEach(() => {
  vi.stubGlobal("EncodedVideoChunk", FakeChunk);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const NO_SINK: MediaSink = { config: () => {}, unit: () => {}, end: () => {} };

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
    expect(agent.sent("hello")[0]).toEqual({ type: "hello", wt_proto: 1, token: "tok-1", session_id: "s1", client: "browser" });
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
      const connecting = t.connect({ config: () => sinkCalls.push("config"), unit: () => {}, end: () => {} });
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
    const connecting = t.connect({ config: () => sinkCalls.push("config"), unit: () => {}, end: () => {} });
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
    await t.connect(NO_SINK);
    vi.useFakeTimers();
    t.requestKeyframe("loss");
    t.requestKeyframe("late");
    t.requestKeyframe("decode_error");
    await vi.advanceTimersByTimeAsync(0);
    expect(agent.sent("keyframe_request").map((k) => k.reason)).toEqual(["loss"]);
    now += KEYFRAME_SPACING_MS;
    await vi.advanceTimersByTimeAsync(KEYFRAME_SPACING_MS);
    expect(agent.sent("keyframe_request").map((k) => k.reason)).toEqual(["loss", "decode_error"]);
  });
});

// ── The runtime, end to end against the fake agent ─────────────────────────

describe("createWebTransportRuntime", () => {
  function setup(opts: { draws?: boolean; noWebGpu?: boolean } = {}) {
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
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: {} as HTMLCanvasElement,
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
      },
    });
    return { agent, decoders, presented, presenter, callbacks, rt, presenterFatal: () => presenterFatal! };
  }

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

  it("asks for a keyframe on loss", async () => {
    const { agent, presented, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    const pkt = { n: 0 };
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 0, keyframe: true, data: bytes(10, 0) }, pkt));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 1, keyframe: false, data: bytes(3000, 1) }, pkt).slice(1));
    agent.last.datagrams(packetize({ configId: 0, frameSeq: 2, keyframe: false, data: bytes(10, 2) }, pkt));
    await vi.waitFor(() => expect(agent.sent("keyframe_request")).toEqual([{ type: "keyframe_request", reason: "loss" }]));
    expect(presented).toHaveLength(1);
  });

  it("treats close 4410 as a takeover", async () => {
    const { agent, callbacks, rt } = setup();
    rt.start();
    await vi.waitFor(() => expect(rt.getSnapshot().channelOpen).toBe(true));
    agent.last.closeWith(4410);
    await vi.waitFor(() => expect(callbacks.onSessionTakenOver).toHaveBeenCalled());
    expect(callbacks.onTransportFailed).not.toHaveBeenCalled();
    expect(rt.getSnapshot().channelOpen).toBe(false);
  });

  it("puts a terminal refusal on the page's failure UI", async () => {
    const { agent, callbacks, rt } = setup();
    agent.replies = [[{ type: "error", code: "auth_failed" }]];
    rt.start();
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
    expect(callbacks.onTransportFailed.mock.calls[0][0]).toMatch(/refused this device's WebTransport token.*WebRTC/);
    expect(rt.getSnapshot().recovery).toMatchObject({ phase: "failed" });
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
    expect(rt.getSnapshot()).toMatchObject({ channelOpen: false, recovery: { phase: "failed" } });
  });

  it("reports a browser with no WebGPU adapter", async () => {
    const { callbacks, rt } = setup({ noWebGpu: true });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
    expect(callbacks.onTransportFailed.mock.calls[0][0]).toMatch(/WebGPU/);
    expect(rt.getSnapshot().clientUnsupported).toBe(true);
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
    expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1);
    expect(callbacks.onTransportFailed.mock.calls[0][0]).toMatch(/GPU/);
  });

  it("reports a presenter that cannot be made, and builds no transport", async () => {
    const agent = new FakeAgent();
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const createTransport = vi.fn();
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: {} as HTMLCanvasElement,
      callbacks,
      deps: { createTransport, createPresenter: () => Promise.reject(new Error("requestDevice failed")) },
    });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
    expect(callbacks.onTransportFailed.mock.calls[0][0]).toMatch(/GPU/);
    expect(createTransport).not.toHaveBeenCalled();
    expect(agent.conns).toHaveLength(0);
  });

  it("does not connect when the presenter fails while it is being made", async () => {
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    vi.spyOn(console, "warn").mockImplementation(() => {});
    const createTransport = vi.fn();
    const destroy = vi.fn();
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: {} as HTMLCanvasElement,
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
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
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
      close: () => {},
    };
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    const presenter: Presenter = { present: () => true, destroy: () => {} };
    const make = () =>
      createWebTransportRuntime({
        sessionId: "s1",
        authToken: "auth",
        coords: COORDS,
        canvas: {} as HTMLCanvasElement,
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
  });

  it("shows only the unsupported banner when the browser cannot decode the stream", async () => {
    const agent = new FakeAgent();
    agent.replies = [[helloAck(0, avcC())]];
    const callbacks = { onDisconnectSuspected: vi.fn(), onSessionTakenOver: vi.fn(), onTransportFailed: vi.fn() };
    const presenter: Presenter = { present: () => true, destroy: () => {} };
    const rt = createWebTransportRuntime({
      sessionId: "s1",
      authToken: "auth",
      coords: COORDS,
      canvas: {} as HTMLCanvasElement,
      callbacks,
      deps: {
        createTransport: (o) => new WebTransportMedia({ ...o, createWebTransport: agent.create, sleep: async () => {} }),
        createPresenter: async () => presenter,
        decoder: { isConfigSupported: async (c) => ({ supported: false, config: c }) },
      },
    });
    rt.start();
    await vi.waitFor(() => expect(callbacks.onTransportFailed).toHaveBeenCalledTimes(1));
    expect(rt.getSnapshot()).toMatchObject({ clientUnsupported: true, recovery: null });
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
