// sessionRuntime — the lifecycle and races that were unreachable while this
// lived inside SessionPage's mount effect. No renderer, no router, no
// providers: a fake transport stands in for RTCPeerConnection / WebSocket /
// RTCDataChannel, which cannot exist in jsdom at all.
//
// What is deliberately NOT re-proved here: the per-tick ordering, which is
// covered without any fakes in telemetrySink.test.ts. These tests are about
// wiring and lifetime — above all the reconnect handoff, where getting `bye`
// wrong either kills the session on every reconnect or orphans host sessions.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import {
  createSessionRuntime,
  type SessionRuntime,
  type SessionRuntimeDeps,
  type TransportFactoryOptions,
  type TransportLike,
} from "./sessionRuntime";
import type { TelemetrySnapshot } from "../../webrtc/telemetry";
import type { ICEServer } from "../../api/types";

// ── fakes ───────────────────────────────────────────────────────────────────

class FakeChannel {
  readyState: RTCDataChannelState = "open";
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  send(data: string) {
    this.sent.push(data);
  }
}

class FakeTransport implements TransportLike {
  videoReceiver: RTCRtpReceiver | null = { kind: "video" } as unknown as RTCRtpReceiver;
  onAudioTrack: ((s: MediaStream) => void) | null = null;
  onWebRtcStateChange:
    | ((kind: "ice" | "connection", from: string, to: string) => void)
    | null = null;
  onSignalingOpen: (() => void) | null = null;
  closedWith: (boolean | undefined)[] = [];
  recoverCalls = 0;
  flowingCalls = 0;
  cancelCalls = 0;
  micSlot = true;

  constructor(readonly opts: TransportFactoryOptions) {}

  getStats = vi.fn(async () => ({}) as RTCStatsReport);
  hasAbsCaptureTimeExtension = vi.fn(() => true);
  hasMicSlot = () => this.micSlot;
  attachMicTrack = vi.fn(async () => {});
  detachMicTrack = vi.fn(async () => {});
  cancelRecovery = () => {
    this.cancelCalls++;
  };
  recoverMediaPath = () => {
    this.recoverCalls++;
  };
  mediaPathFlowing = () => {
    this.flowingCalls++;
  };
  close = (notifyPeer?: boolean) => {
    this.closedWith.push(notifyPeer);
  };

  // ── drive the runtime from the outside ──
  fireTrack(stream = {} as MediaStream) {
    this.opts.onTrack(stream);
  }
  fireStatus(msg: string) {
    this.opts.onStatus(msg);
  }
  fireSignalingOpen() {
    this.onSignalingOpen?.();
  }
  fireIce(to: string) {
    this.onWebRtcStateChange?.("ice", "checking", to);
  }
  fireChannel(ch: FakeChannel) {
    this.opts.onChannel(ch as unknown as RTCDataChannel);
  }
  fireRecovery(phase: string, message = "…") {
    this.opts.onRecoveryState({ phase, message } as never);
  }
}

function snap(over: Partial<TelemetrySnapshot> = {}): TelemetrySnapshot {
  return {
    fps: 60,
    bitrateKbps: 8000,
    rttMs: 20,
    jbMs: null,
    decodeMs: null,
    packetsLost: 0,
    framesDropped: 0,
    freezeCount: 0,
    presentFps: 60,
    presentSdMs: 4,
    playoutTargetMs: 30,
    encodeMs: null,
    networkMs: null,
    decodeDisplayMs: null,
    inputMetrics: null,
    displayRefreshHz: 60,
    clientHealth: "smooth",
    clientHealthReason: "",
    negotiatedCodec: "h264",
    framesDecodedTotal: 1000,
    bytesReceivedTotal: 500_000,
    g2gMs: 45,
    ...over,
  } as TelemetrySnapshot;
}

function harness(opts: { mint?: () => Promise<unknown>; iceServers?: ICEServer[] } = {}) {
  let transport!: FakeTransport;
  let emit!: (s: TelemetrySnapshot) => void;

  const telemetry = {
    onUpdate: vi.fn((fn: (s: TelemetrySnapshot) => void) => {
      emit = fn;
    }),
    start: vi.fn(),
    stop: vi.fn(),
    setDecodeFailed: vi.fn(),
    clockOffsetMs: vi.fn(() => null),
  };
  const tracer = {
    start: vi.fn(),
    stop: vi.fn(),
    emitPlayoutChanged: vi.fn(),
    emitFreezeDetected: vi.fn(),
    emitWebRtcStateChanged: vi.fn(),
    emitBenchWindow: vi.fn(),
  };
  const capture = {
    cleanup: vi.fn(),
    getMetrics: vi.fn(() => null),
    syncKeyboardLock: vi.fn(),
    engage: vi.fn(async () => ({ mode: "pointer-lock" })),
    release: vi.fn(),
  };
  let captureArgs: Record<string, unknown> | null = null;

  const callbacks = {
    onSummonOverlay: vi.fn(),
    onDisconnectSuspected: vi.fn(),
    onReplacementSignaling: vi.fn(),
    onReconnectFailed: vi.fn(),
    onSessionTakenOver: vi.fn(),
  };

  const deps: Partial<SessionRuntimeDeps> = {
    createTransport: (o) => {
      transport = new FakeTransport(o);
      return transport;
    },
    createTelemetry: () => telemetry,
    createTracer: () => tracer,
    setupCapture: ((args: Record<string, unknown>) => {
      captureArgs = args;
      return capture;
    }) as never,
    mintSignalingToken: (opts.mint ?? (async () => ({
      signaling: { url: "wss://new", token: "tok-2" },
    }))) as never,
    loadBench: vi.fn(async () => ({ BenchController: class {} }) as never),
    isBenchModeEnabled: () => false,
  };

  const video = videoEl();
  const runtime: SessionRuntime = createSessionRuntime({
    sessionId: "sess-1",
    authToken: "auth-1",
    signaling: {
      url: "wss://old",
      token: "tok-1",
      replacement: false,
      iceServers: opts.iceServers,
    },
    videoEl: video,
    callbacks,
    deps,
  });

  return {
    runtime,
    video,
    callbacks,
    telemetry,
    tracer,
    capture,
    get captureArgs() {
      return captureArgs;
    },
    get transport() {
      return transport;
    },
    emit: (s: TelemetrySnapshot) => emit(s),
  };
}

/** jsdom does not implement HTMLMediaElement.play, and the runtime calls it on
 *  every incoming track. */
function videoEl(): HTMLVideoElement {
  const el = document.createElement("video");
  el.play = vi.fn(async () => {});
  return el;
}

const flush = () => Promise.resolve().then(() => undefined);

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

// ── tests ───────────────────────────────────────────────────────────────────

describe("channel activation", () => {
  it("activates immediately when the channel is already open", () => {
    // The channel may already be "open" when ondatachannel fires; wiring onopen
    // after the fact would silently miss it.
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());

    expect(h.runtime.getSnapshot().channelOpen).toBe(true);
    expect(h.telemetry.start).toHaveBeenCalledTimes(1);
  });

  it("waits for onopen when the channel is still connecting", () => {
    const h = harness();
    h.runtime.start();
    const ch = new FakeChannel();
    ch.readyState = "connecting";
    h.transport.fireChannel(ch);
    expect(h.runtime.getSnapshot().channelOpen).toBe(false);

    ch.readyState = "open";
    ch.onopen?.();
    expect(h.runtime.getSnapshot().channelOpen).toBe(true);
  });

  it("sends input as JSON only while the channel is open", () => {
    const h = harness();
    h.runtime.start();
    const ch = new FakeChannel();
    h.transport.fireChannel(ch);

    const sendInput = h.captureArgs!.sendInput as (m: Record<string, unknown>) => void;
    sendInput({ type: "key", code: 30 });
    expect(ch.sent).toEqual(['{"type":"key","code":30}']);

    ch.readyState = "closed";
    sendInput({ type: "key", code: 31 });
    expect(ch.sent).toHaveLength(1);
  });

  it("reports capture transitions as two distinct booleans", () => {
    // inputCaptured is what the chrome reflects; pointerLocked is the real lock
    // that decides whether an on-screen control can be tapped at all.
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());

    const onCaptureChange = h.captureArgs!.onCaptureChange as (s: {
      captured: boolean;
      pointerLocked: boolean;
    }) => void;
    onCaptureChange({ captured: true, pointerLocked: false });

    expect(h.runtime.getSnapshot()).toMatchObject({ inputCaptured: true, pointerLocked: false });
    expect(h.runtime.isCaptured()).toBe(true); // synchronous, for gesture handlers
  });
});

describe("track / channel ordering (L2)", () => {
  it("arms the playout controller whichever arrives first", () => {
    for (const order of ["track-first", "channel-first"] as const) {
      const h = harness();
      h.runtime.start();
      if (order === "track-first") {
        h.transport.fireTrack();
        h.transport.fireChannel(new FakeChannel());
      } else {
        h.transport.fireChannel(new FakeChannel());
        h.transport.fireTrack();
      }
      // PlayoutController.start() applies its initial target and calls
      // onChange, which the runtime routes to the tracer — proof the controller
      // was armed, in both orders.
      expect(h.tracer.emitPlayoutChanged, order).toHaveBeenCalledTimes(1);
      expect(h.runtime.getSnapshot().channelOpen, order).toBe(true);
    }
  });

  it("does not arm a second controller when the track re-fires", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireTrack();
    const first = h.tracer.emitPlayoutChanged.mock.calls.length;
    h.transport.fireTrack();
    expect(h.tracer.emitPlayoutChanged.mock.calls.length).toBe(first);
  });
});

describe("telemetry wiring", () => {
  it("fans out to registered consumers and stops on unregister", () => {
    const h = harness();
    const seen: number[] = [];
    const off = h.runtime.registerTelemetry((s) => seen.push(s.fps));
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());

    h.emit(snap({ fps: 59 }));
    off();
    h.emit(snap({ fps: 61 }));
    expect(seen).toEqual([59]);
  });

  it("drives transport recovery from three stalled windows", () => {
    // The sink owns the counting; this asserts it is actually wired to the
    // transport — the path that made a media outage self-heal.
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());

    for (let i = 0; i < 3; i++) h.emit(snap({ bitrateKbps: 0, fps: 0 }));
    expect(h.transport.recoverCalls).toBe(1);

    h.emit(snap());
    expect(h.transport.flowingCalls).toBe(1);
  });

  it("latches the sticky client_unsupported verdict into the snapshot", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());

    h.emit(snap({ clientHealth: "client_unsupported" }));
    expect(h.runtime.getSnapshot().clientUnsupported).toBe(true);
    h.emit(snap({ clientHealth: "smooth" }));
    expect(h.runtime.getSnapshot().clientUnsupported).toBe(true); // does not self-heal
  });

  it("collects the end-of-session summary", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());
    h.emit(snap({ fps: 58, g2gMs: 40 }));
    h.emit(snap({ fps: 60, g2gMs: 44 }));

    expect(h.runtime.summary().fps).toEqual([58, 60]);
    expect(h.runtime.summary().latency).toEqual([40, 44]);
  });
});

describe("status routing", () => {
  it.each([
    ["signaling relay closed: 4500", true],
    ["host offline", true],
    ["ICE failed — network issue", true],
    ["connected", false],
  ])("%s → disconnect suspected: %s", (msg, expected) => {
    const h = harness();
    h.runtime.start();
    h.transport.fireStatus(msg);

    expect(h.runtime.getSnapshot().status).toBe(msg);
    expect(h.callbacks.onDisconnectSuspected).toHaveBeenCalledTimes(expected ? 1 : 0);
  });
});

describe("recovery and the replacement handoff (L4)", () => {
  it("mints once, tells the page, and then closes WITHOUT bye", async () => {
    // The whole point: the host session and its app must survive a reconnect.
    const h = harness();
    h.runtime.start();
    h.transport.fireRecovery("failed");
    h.transport.fireRecovery("failed"); // a second failed tick must not re-mint
    await flush();
    await flush();

    expect(h.callbacks.onReplacementSignaling).toHaveBeenCalledTimes(1);
    expect(h.callbacks.onReplacementSignaling).toHaveBeenCalledWith({
      url: "wss://new",
      token: "tok-2",
      // #509: a mint response with no ice_servers key reads as none configured.
      iceServers: [],
    });

    h.runtime.destroy();
    expect(h.transport.closedWith).toEqual([false]);
  });

  // #526 / L6 — the displacement loop, at the escalation point that caused it.
  it("does NOT mint when the session was taken over (L6)", async () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireRecovery("superseded", "This session was opened in another tab or window");
    await flush();
    await flush();

    expect(h.callbacks.onSessionTakenOver).toHaveBeenCalledTimes(1);
    expect(h.callbacks.onReplacementSignaling).not.toHaveBeenCalled();
    expect(h.callbacks.onReconnectFailed).not.toHaveBeenCalled();
    expect(h.runtime.getSnapshot().recovery).toMatchObject({ phase: "superseded" });
  });

  // The other half of the same claim: an ordinary terminal failure — a network
  // blip that exhausted bounded recovery — still escalates exactly as before.
  it("still mints on an ordinary terminal failure (L6 does not disarm L3)", async () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireRecovery("failed");
    await flush();
    await flush();

    expect(h.callbacks.onReplacementSignaling).toHaveBeenCalledTimes(1);
    expect(h.callbacks.onSessionTakenOver).not.toHaveBeenCalled();
  });

  it("publishes non-terminal recovery phases without minting", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireRecovery("reconnecting", "Recovering connection");

    expect(h.runtime.getSnapshot().recovery).toMatchObject({ phase: "reconnecting" });
    expect(h.callbacks.onReplacementSignaling).not.toHaveBeenCalled();
  });

  it("treats a malformed envelope as a failed reconnect, not a crash", async () => {
    // apiFetch resolves a bodyless 2xx to undefined; the old destructure threw a
    // TypeError inside .then and printed it at the user verbatim.
    const h = harness({ mint: async () => undefined });
    h.runtime.start();
    h.transport.fireRecovery("failed");
    await flush();
    await flush();

    expect(h.callbacks.onReconnectFailed).toHaveBeenCalledWith(undefined);
    expect(h.callbacks.onReplacementSignaling).not.toHaveBeenCalled();
    expect(h.runtime.getSnapshot().status).toBe(
      "Reconnect failed — this session can no longer be resumed",
    );
  });

  it("still sends bye when the mint failed — that session is not being handed off", async () => {
    const h = harness({ mint: async () => undefined });
    h.runtime.start();
    h.transport.fireRecovery("failed");
    await flush();
    await flush();

    h.runtime.destroy();
    expect(h.transport.closedWith).toEqual([true]);
  });
});

describe("teardown levels (L3, L5)", () => {
  it("channel close stops the peripherals but leaves the transport alive", () => {
    // A dead channel is exactly when recovery escalates and mints a replacement
    // token, so closing the transport here would strand the session.
    const h = harness();
    h.runtime.start();
    const ch = new FakeChannel();
    h.transport.fireChannel(ch);

    ch.onclose?.();

    expect(h.telemetry.stop).toHaveBeenCalledTimes(1);
    expect(h.capture.cleanup).toHaveBeenCalledTimes(1);
    expect(h.tracer.stop).toHaveBeenCalledTimes(1);
    expect(h.transport.closedWith).toEqual([]); // transport untouched
    expect(h.runtime.getSnapshot()).toMatchObject({
      channelOpen: false,
      inputCaptured: false,
      pointerLocked: false,
    });
  });

  it("destroy after a channel close does not double-stop the peripherals", () => {
    const h = harness();
    h.runtime.start();
    const ch = new FakeChannel();
    h.transport.fireChannel(ch);
    ch.onclose?.();
    h.runtime.destroy();

    expect(h.telemetry.stop).toHaveBeenCalledTimes(1);
    expect(h.transport.closedWith).toEqual([true]);
  });

  it("destroy before anything connects closes with bye and does not throw", () => {
    const h = harness();
    h.runtime.start();
    expect(() => h.runtime.destroy()).not.toThrow();
    expect(h.transport.closedWith).toEqual([true]);
  });

  it("is idempotent", () => {
    const h = harness();
    h.runtime.start();
    h.runtime.destroy();
    h.runtime.destroy();
    expect(h.transport.closedWith).toEqual([true]);
  });

  it("publishes nothing after destroy", () => {
    const h = harness();
    h.runtime.start();
    const listener = vi.fn();
    h.runtime.subscribe(listener);
    h.runtime.destroy();
    listener.mockClear();

    h.transport.fireStatus("late message");
    expect(listener).not.toHaveBeenCalled();
  });

  it("start is idempotent and does not build a second transport", () => {
    const h = harness();
    h.runtime.start();
    const first = h.transport;
    h.runtime.start();
    expect(h.transport).toBe(first);
  });
});

describe("transport commands", () => {
  it("forwards capture, mic and recovery commands, and is safe before start", async () => {
    const h = harness();
    expect(h.runtime.isCaptured()).toBe(false);
    expect(h.runtime.hasMicSlot()).toBe(false); // no transport yet
    await expect(h.runtime.attachMicTrack({} as MediaStreamTrack)).resolves.toBeUndefined();

    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());
    expect(h.runtime.hasMicSlot()).toBe(true);

    h.runtime.release();
    expect(h.capture.release).toHaveBeenCalledTimes(1);
    h.runtime.syncKeyboardLock();
    expect(h.capture.syncKeyboardLock).toHaveBeenCalledTimes(1);
    h.runtime.cancelRecovery();
    expect(h.transport.cancelCalls).toBe(1);
  });

  it("offers no engage handle until the channel activates", () => {
    const h = harness();
    h.runtime.start();
    expect(h.runtime.engage()).toBeNull();

    h.transport.fireChannel(new FakeChannel());
    expect(h.runtime.engage()).not.toBeNull();
  });

  // ── ICE servers (#509) ────────────────────────────────────────────────────

  it("hands the transport an empty ICE list when the deployment configures none", () => {
    const h = harness();
    h.runtime.start();
    // The pre-#509 behaviour, pinned: host candidates only, never undefined
    // reaching the RTCPeerConnection constructor.
    expect(h.transport.opts.iceServers).toEqual([]);
  });

  it("hands the transport the deployment's ICE servers", () => {
    const servers: ICEServer[] = [
      { urls: ["stun:stun.example.net:3478"] },
      { urls: ["turn:turn.example.net:3478"], username: "u", credential: "c" },
    ];
    const h = harness({ iceServers: servers });
    h.runtime.start();
    expect(h.transport.opts.iceServers).toEqual(servers);
  });

  it("carries the replacement mint's ICE servers, not the launch's", async () => {
    const h = harness({
      iceServers: [{ urls: ["stun:old.example.net:3478"] }],
      mint: async () => ({
        signaling: {
          url: "wss://new",
          token: "tok-2",
          ice_servers: [{ urls: ["stun:new.example.net:3478"] }],
        },
      }),
    });
    h.runtime.start();
    h.transport.fireRecovery("failed", "gave up");
    await flush();

    expect(h.callbacks.onReplacementSignaling).toHaveBeenCalledWith({
      url: "wss://new",
      token: "tok-2",
      iceServers: [{ urls: ["stun:new.example.net:3478"] }],
    });
  });
});

// The launch screen's four steps are these signals and nothing else — a status
// string it does not recognise must never move the rail (#482).
describe("launch signals", () => {
  it("starts with every launch signal false", () => {
    const h = harness();
    h.runtime.start();
    expect(h.runtime.getSnapshot()).toMatchObject({
      wsOpen: false,
      pcConnected: false,
      firstFrame: false,
      channelOpen: false,
    });
  });

  it("records the signalling socket, the ICE path, the first frame and the channel", () => {
    const h = harness();
    h.runtime.start();

    h.transport.fireSignalingOpen();
    expect(h.runtime.getSnapshot().wsOpen).toBe(true);

    h.transport.fireIce("connected");
    expect(h.runtime.getSnapshot().pcConnected).toBe(true);

    h.video.dispatchEvent(new Event("loadeddata"));
    expect(h.runtime.getSnapshot().firstFrame).toBe(true);

    h.transport.fireChannel(new FakeChannel());
    expect(h.runtime.getSnapshot().channelOpen).toBe(true);
  });

  // A blip must not walk the rail backwards mid-launch: the loader's steps are
  // "has this happened", not "is this true now".
  it("keeps the ICE signal latched across a disconnect", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireIce("completed");
    h.transport.fireIce("disconnected");
    expect(h.runtime.getSnapshot().pcConnected).toBe(true);
    expect(h.runtime.getSnapshot().iceState).toBe("disconnected");
  });

  it("stops listening for the first frame once the runtime is destroyed", () => {
    const h = harness();
    h.runtime.start();
    h.runtime.destroy();
    h.video.dispatchEvent(new Event("loadeddata"));
    expect(h.runtime.getSnapshot().firstFrame).toBe(false);
  });

  // jsdom has no rVFC; the runtime prefers it where it exists, so the handle it
  // returns has to be cancelled too — removing the `loadeddata` listener leaves
  // a pending frame callback armed on a destroyed session's element.
  it("cancels a pending video-frame callback on destroy", () => {
    const h = harness();
    const cancelVideoFrameCallback = vi.fn();
    const requestVideoFrameCallback = vi.fn(() => 77);
    Object.assign(h.video, { requestVideoFrameCallback, cancelVideoFrameCallback });

    h.runtime.start();
    expect(requestVideoFrameCallback).toHaveBeenCalledTimes(1);

    h.runtime.destroy();
    expect(cancelVideoFrameCallback).toHaveBeenCalledWith(77);
  });
});

describe("snapshot notification dedupe (#78)", () => {
  // The telemetry sink re-reports displayRefreshHz on every ~1 Hz snapshot.
  // Before the dedupe, each report notified subscribers even when nothing
  // changed — SessionPage re-rendered every tick, and the HUD's per-render
  // measure() replayed the open shelf pane's entry animation as a visible
  // flash. A no-op write must not notify.
  it("does not notify subscribers when a telemetry tick changes nothing", () => {
    const h = harness();
    h.runtime.start();
    h.transport.fireChannel(new FakeChannel());
    const notify = vi.fn();
    h.runtime.subscribe(notify);

    h.emit(snap({ displayRefreshHz: 60 }));
    const afterFirst = notify.mock.calls.length;
    expect(h.runtime.getSnapshot().displayRefreshHz).toBe(60);

    h.emit(snap({ displayRefreshHz: 60 }));
    h.emit(snap({ displayRefreshHz: 60 }));
    expect(notify.mock.calls.length).toBe(afterFirst);

    h.emit(snap({ displayRefreshHz: 59 }));
    expect(notify.mock.calls.length).toBe(afterFirst + 1);
    expect(h.runtime.getSnapshot().displayRefreshHz).toBe(59);
  });
});
