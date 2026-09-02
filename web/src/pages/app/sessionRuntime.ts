/**
 * sessionRuntime — one live session's transport generation as an addressable
 * object: signaling transport, tracer, input capture, telemetry (via
 * telemetrySink), playout controller, bench instrument, and the
 * recovery/replacement-token flow. No React imports, so its races test with a
 * fake transport.
 *
 * The laws a caller depends on:
 *
 *   L1  One instance = one transport generation. New signaling coords never
 *       mutate a runtime; the page destroys and recreates.
 *       `signaling.replacement` maps to requestOfferOnOpen.
 *   L2  onTrack and onChannel have no guaranteed order. Playout is armed in
 *       onTrack; telemetry must read it lazily — passing the value at
 *       construction freezes playout_target_ms whenever the channel activates
 *       first, corrupting the σ-vs-buffer correlation.
 *   L3  Channel close is not destroy. `onChannelGone` stops peripherals but
 *       leaves the transport and its RecoveryController alive — a dead channel
 *       is when recovery escalates to `failed` and mints a replacement token.
 *       Collapsing the two makes a media outage never reconnect.
 *   L4  The handoff latch decides `bye`. A successful mint latches `handoff`
 *       before telling the page, so destroy() closes with notifyPeer=false and
 *       the host session survives the reconnect; an ordinary unmount says bye.
 *       Backwards, this kills the session every reconnect or orphans host
 *       sessions on unmount.
 *   L5  destroy() is idempotent and final: afterwards no callback fires and no
 *       snapshot transition happens (late continuations are guarded).
 *   L6  Not every terminal phase escalates (#526, amends L3). `superseded`
 *       (WS close 4410: a later attach took this session's signaling) is
 *       terminal WITHOUT a mint — minting re-attaches and the two tabs displace
 *       each other forever. L3 reads: a dead channel escalates unless the close
 *       code says someone else owns the session now.
 */

import { mintSignalingToken } from "../../api/library";
import { ApiError } from "../../api/client";
import type { ICEServer } from "../../api/types";
import { setupCapture } from "../../input/capture";
import { QuasarSession } from "../../webrtc/session";
import type { RecoveryState } from "../../webrtc/recovery";
import { PlayoutController, resolveInitialPlayoutMs } from "../../webrtc/playout";
import {
  SessionTelemetry,
  ABS_CAPTURE_TIME_URI,
  type TelemetrySnapshot,
} from "../../webrtc/telemetry";
import { TraceEventEmitter } from "../../webrtc/traceEvents";
import type { TransportState } from "./launchStall";
import { isBenchModeEnabled } from "../../bench/benchFlag";
import type { BenchController } from "../../bench/benchController";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { createTelemetrySink, type SessionSummaryAccumulator } from "./telemetrySink";

// ── The snapshot the page renders from ──────────────────────────────────────

export interface SessionRuntimeSnapshot {
  status: string;
  channelOpen: boolean;
  /** What the session chrome reflects (strip actions hide, drawer says
   *  "Release input"). Can be true while pointerLocked is false on a device
   *  with no Pointer Lock API — see input/capture.ts. */
  inputCaptured: boolean;
  /** The REAL lock: a locked pointer cannot tap an on-screen control, which is
   *  what gates the floating session-menu button. */
  pointerLocked: boolean;
  recovery: RecoveryState | null;
  /** Sticky for the session — a hard decode failure does not self-heal. */
  clientUnsupported: boolean;
  displayRefreshHz: number | null;
  /** #482: last video-PC ICE state, null until the first transition. Lets the
   *  launch screen tell a transport failure from a scheduling one. */
  iceState: TransportState | null;
  /**
   * The launch screen's four steps (loaderPhases.ts). Latched: each says "this
   * has happened", not "this is true now", so a mid-launch blip cannot walk the
   * rail backwards. The fourth is `channelOpen` above — the input DataChannel
   * opening is the same event.
   */
  wsOpen: boolean;
  pcConnected: boolean;
  firstFrame: boolean;
}

const IDLE_SNAPSHOT: SessionRuntimeSnapshot = {
  status: "connecting…",
  channelOpen: false,
  inputCaptured: false,
  pointerLocked: false,
  recovery: null,
  clientUnsupported: false,
  displayRefreshHz: null,
  iceState: null,
  wsOpen: false,
  pcConnected: false,
  firstFrame: false,
};

export { IDLE_SNAPSHOT };

/** ICE states that mean the media path is up. */
const ICE_UP: readonly string[] = ["connected", "completed"];

const TRANSPORT_STATES: readonly string[] = [
  "new",
  "checking",
  "connected",
  "completed",
  "failed",
  "disconnected",
  "closed",
];

/** Narrow, don't assert: an unrecognised state becomes null ("no ICE info" to
 *  resolveStall) rather than a bogus value that could pass for a verdict. */
function asTransportState(value: string): TransportState | null {
  return TRANSPORT_STATES.includes(value) ? (value as TransportState) : null;
}

// ── What the runtime needs from the page ────────────────────────────────────

export interface SessionRuntimeCallbacks {
  /** Ctrl+Alt+Shift+Q — capture.ts releases the lock itself, then asks the page
   *  to open the drawer. Esc and Ctrl+Alt+Shift+Z only release; they must NOT
   *  open it. */
  onSummonOverlay(): void;
  /** Keyboard Lock was refused (capture.ts fires this once per capture
   *  instance). Optional and purely informational: the page raises the
   *  explanatory toast; the runtime keeps working with browser-owned Esc. */
  onKeyboardLockRefused?(error: unknown): void;
  /** A disconnect signature was seen on the status channel. The host-lost poll
   *  stays on the page: hostLost is page state and the poller is page-owned. */
  onDisconnectSuspected(): void;
  /** Recovery failed and a replacement token was minted. The page re-seats its
   *  signaling coords, which destroys this runtime and creates the next. */
  onReplacementSignaling(coords: { url: string; token: string; iceServers: ICEServer[] }): void;
  /** The mint failed. `serverDetail` is SERVER-authored text only (ApiError
   *  .message); any other throw is a client-side fault the user can do nothing
   *  with, and is reported to the console instead. */
  onReconnectFailed(serverDetail?: string): void;
  /** #526 — a later attach took this session over (recovery phase
   *  `superseded`). The page posts its terminal verdict; there is nothing to
   *  retry, and retrying is the bug. */
  onSessionTakenOver(): void;
}

// ── Seams (production defaults; a test substitutes the browser boundary) ────

/** The transport surface the runtime uses. QuasarSession satisfies it; a test
 *  supplies a fake, because RTCPeerConnection/WebSocket/RTCDataChannel cannot
 *  exist in jsdom at all. */
export interface TransportLike {
  videoReceiver: RTCRtpReceiver | null;
  onAudioTrack: ((stream: MediaStream) => void) | null;
  /** The signalling WebSocket opened. Optional so a fake need not carry it. */
  onSignalingOpen?: (() => void) | null;
  onWebRtcStateChange:
    | ((kind: "ice" | "connection", from: string, to: string) => void)
    | null;
  getStats(): Promise<RTCStatsReport>;
  hasAbsCaptureTimeExtension(uri: string): boolean;
  hasMicSlot(): boolean;
  attachMicTrack(track: MediaStreamTrack): Promise<void>;
  detachMicTrack(): Promise<void>;
  cancelRecovery(): void;
  recoverMediaPath(): void;
  mediaPathFlowing(): void;
  close(notifyPeer?: boolean): void;
}

export interface TransportHandlers {
  onTrack(stream: MediaStream): void;
  onStatus(message: string): void;
  onChannel(channel: RTCDataChannel): void;
  onRecoveryState(state: RecoveryState): void;
}

export interface TransportFactoryOptions extends TransportHandlers {
  url: string;
  token: string;
  initialPlayoutMs: number;
  requestOfferOnOpen: boolean;
  /** #509: the deployment's STUN/TURN servers, carried with the coords they
   *  arrived on. Empty is the LAN default. */
  iceServers: ICEServer[];
}

export interface SessionRuntimeDeps {
  createTransport(opts: TransportFactoryOptions): TransportLike;
  createTelemetry(args: TelemetryFactoryArgs): TelemetryLike;
  createTracer(sessionId: string, authToken: string): TracerLike;
  setupCapture: typeof setupCapture;
  mintSignalingToken: typeof mintSignalingToken;
  loadBench(): Promise<{ BenchController: typeof BenchController }>;
  isBenchModeEnabled(): boolean;
}

export interface TelemetryFactoryArgs {
  videoEl: HTMLVideoElement;
  getStats: () => Promise<RTCStatsReport>;
  channel: RTCDataChannel;
  sessionId: string;
  authToken: string;
  playoutTargetMs: () => number;
  getCaptureMetrics: Parameters<typeof setupCapture> extends never ? never : () => unknown;
  tierFps?: number;
  hasAbsCaptureTime: () => boolean;
}

export interface TelemetryLike {
  onUpdate(fn: (snap: TelemetrySnapshot) => void): void;
  start(): void;
  stop(): void;
  setDecodeFailed(failed: boolean): void;
  clockOffsetMs(): number | null;
}

export interface TracerLike {
  start(): void;
  stop(): void;
  emitPlayoutChanged(from: number, to: number, reason: string): void;
  emitFreezeDetected(gapMs: number | null, isHidden: boolean): void;
  emitWebRtcStateChanged(kind: "ice" | "connection", from: string, to: string): void;
  emitBenchWindow(payload: unknown): void;
}

const productionDeps: SessionRuntimeDeps = {
  createTransport: (o) =>
    new QuasarSession(
      o.url,
      o.token,
      o.onTrack,
      o.onStatus,
      o.onChannel,
      o.initialPlayoutMs,
      o.onRecoveryState,
      o.requestOfferOnOpen,
      o.iceServers,
    ) as unknown as TransportLike,
  createTelemetry: (a) =>
    new SessionTelemetry(
      a.videoEl,
      a.getStats,
      a.channel,
      a.sessionId,
      a.authToken,
      a.playoutTargetMs,
      a.getCaptureMetrics as never,
      a.tierFps,
      a.hasAbsCaptureTime,
    ) as unknown as TelemetryLike,
  createTracer: (sessionId, authToken) =>
    new TraceEventEmitter(sessionId, authToken) as unknown as TracerLike,
  setupCapture,
  mintSignalingToken,
  loadBench: () => import("../../bench/benchController"),
  isBenchModeEnabled,
};

// ── Config + public surface ─────────────────────────────────────────────────

export interface SessionRuntimeConfig {
  sessionId: string;
  authToken: string;
  /** #509: `iceServers` travels with the url/token it was minted alongside —
   *  a replacement mint may return a different list than the launch. */
  signaling: { url: string; token: string; replacement: boolean; iceServers?: ICEServer[] };
  /** The page renders the <video>; the runtime is handed the node. */
  videoEl: HTMLVideoElement;
  playout0Ms?: number;
  /** `?playout=` — when present the adaptive controller stays off, because the
   *  measurement instrument is absolute. */
  playoutOverrideMs?: number | null;
  /** The launched profile's fps, for the client-health classifier's per-frame
   *  budget. Undefined when unknown. */
  tierFps?: number;
  callbacks: SessionRuntimeCallbacks;
  deps?: Partial<SessionRuntimeDeps>;
}

export interface EngageResult {
  mode: string;
  [k: string]: unknown;
}

export interface SessionRuntime {
  getSnapshot(): SessionRuntimeSnapshot;
  subscribe(listener: () => void): () => void;
  start(): void;
  destroy(): void;
  /** null until the input channel activates — exactly when every control that
   *  offers capture is disabled. */
  engage(): ReturnType<ReturnType<typeof setupCapture>["engage"]> | null;
  release(): void;
  syncKeyboardLock(): void;
  /** SYNCHRONOUS truth for gesture handlers, which must not read last-rendered
   *  state. This is why the runtime is an object and not a hook. */
  isCaptured(): boolean;
  cancelRecovery(): void;
  hasMicSlot(): boolean;
  attachMicTrack(track: MediaStreamTrack): Promise<void>;
  detachMicTrack(): Promise<void>;
  /** Stable for the runtime's lifetime; subscribing never causes a snapshot
   *  transition, keeping the video/input subtree off the 1 Hz telemetry path. */
  registerTelemetry(fn: (snap: TelemetrySnapshot) => void): () => void;
  summary(): SessionSummaryAccumulator;
}

export function createSessionRuntime(cfg: SessionRuntimeConfig): SessionRuntime {
  const deps: SessionRuntimeDeps = { ...productionDeps, ...cfg.deps };

  let snapshot: SessionRuntimeSnapshot = { ...IDLE_SNAPSHOT };
  const listeners = new Set<() => void>();

  let transport: TransportLike | null = null;
  let tracer: TracerLike | null = null;
  let telemetry: TelemetryLike | null = null;
  let playout: PlayoutController | null = null;
  let bench: BenchController | null = null;
  let captureCleanup: (() => void) | null = null;
  let captureKbSync: (() => void) | null = null;
  let captureEngage: ReturnType<typeof setupCapture>["engage"] | null = null;
  let captureRelease: (() => void) | null = null;
  let audioCleanup: (() => void) | null = null;
  let firstFrameCleanup: (() => void) | null = null;
  let captured = false;

  let started = false;
  let destroyed = false;
  /** L4 — latched before the page is told, never cleared. */
  let handoff = false;
  /** Per-instance dedupe, so an old and a new runtime cannot double-mint. */
  let mintInFlight = false;

  const telemetrySubs = new Set<(snap: TelemetrySnapshot) => void>();

  const hasOverride = cfg.playoutOverrideMs != null;
  const initialPlayoutMs = resolveInitialPlayoutMs(
    cfg.playout0Ms,
    hasOverride ? `?playout=${cfg.playoutOverrideMs}` : undefined,
  );

  const sink = createTelemetrySink({
    fanOut: (snap) => {
      for (const fn of telemetrySubs) fn(snap);
    },
    playoutSample: (sample) => playout?.sample(sample),
    recoverMediaPath: () => transport?.recoverMediaPath(),
    mediaPathFlowing: () => transport?.mediaPathFlowing(),
    emitFreezeDetected: (gapMs, isHidden) => tracer?.emitFreezeDetected(gapMs, isHidden),
    setDecodeFailed: () => telemetry?.setDecodeFailed(true),
    onClientUnsupported: () => set({ clientUnsupported: true }),
    onDisplayRefreshHz: (hz) => set({ displayRefreshHz: hz }),
    now: () => performance.now(),
    isHidden: () => typeof document !== "undefined" && document.visibilityState === "hidden",
  });

  function set(next: Partial<SessionRuntimeSnapshot>): void {
    if (destroyed) return; // L5
    // A no-op write must not notify: the telemetry sink re-reports
    // displayRefreshHz every snapshot (~1 Hz), and an unconditional notify
    // re-rendered SessionPage each tick — which replayed the HUD shelf's
    // entry animation through Hud's per-render measure() (#78).
    let changed = false;
    for (const k in next) {
      const key = k as keyof SessionRuntimeSnapshot;
      if (!Object.is(snapshot[key], next[key])) {
        changed = true;
        break;
      }
    }
    if (!changed) return;
    snapshot = { ...snapshot, ...next };
    for (const listener of [...listeners]) listener();
  }

  /** L3 — peripherals only. The transport and its recovery controller live on. */
  function onChannelGone(): void {
    set({ channelOpen: false, pointerLocked: false, inputCaptured: false });
    captured = false;
    captureCleanup?.();
    captureCleanup = null;
    captureKbSync = null;
    captureEngage = null;
    captureRelease = null;
    // Bench stops before telemetry: its final flush reads the clock offset off
    // the telemetry instance. __qBench stays on window for the harness.
    bench?.stop();
    bench = null;
    telemetry?.stop();
    telemetry = null;
    playout?.stop();
    playout = null;
    tracer?.stop();
    tracer = null;
  }

  function activateChannel(ch: RTCDataChannel): void {
    if (destroyed) return;
    set({ channelOpen: true });

    const sendInput = (msg: Record<string, unknown>) => {
      if (ch.readyState === "open") ch.send(JSON.stringify(msg));
    };

    const capture = deps.setupCapture({
      videoEl: cfg.videoEl,
      sendInput,
      onCaptureChange: ({ captured: isCaptured, pointerLocked: locked }) => {
        captured = isCaptured;
        set({ inputCaptured: isCaptured, pointerLocked: locked });
      },
      channel: ch,
      // Keyboard Lock captures Escape for the game, but only in fullscreen;
      // capture.ts reads live fullscreen state through this.
      isFullscreen: () => document.fullscreenElement != null,
      onSummonOverlay: () => cfg.callbacks.onSummonOverlay(),
      onKeyboardLockRefused: (error) => cfg.callbacks.onKeyboardLockRefused?.(error),
    });
    captureCleanup = capture.cleanup;
    captureKbSync = capture.syncKeyboardLock;
    captureEngage = capture.engage;
    captureRelease = capture.release;

    const tm = deps.createTelemetry({
      videoEl: cfg.videoEl,
      getStats: () => transport!.getStats(),
      channel: ch,
      sessionId: cfg.sessionId,
      authToken: cfg.authToken,
      // L2 — lazy on purpose. Reads the live controller if it exists yet.
      playoutTargetMs: () => playout?.currentMs() ?? initialPlayoutMs,
      getCaptureMetrics: capture.getMetrics as never,
      tierFps: cfg.tierFps,
      hasAbsCaptureTime: () => transport!.hasAbsCaptureTimeExtension(ABS_CAPTURE_TIME_URI),
    });
    tm.onUpdate((snap) => sink.push(snap));
    tm.start();
    telemetry = tm;

    // ?bench=1 only — it reads back pixels every displayed frame, so it must
    // never start for an ordinary session.
    if (deps.isBenchModeEnabled() && bench === null) {
      void deps
        .loadBench()
        .then(({ BenchController: Ctor }) => {
          // channel may have closed while the chunk was in flight; arming onto
          // a dead session would leak an RVFC loop nothing stops
          if (destroyed || ch.readyState !== "open" || bench !== null) return;
          const b = new Ctor({
            video: cfg.videoEl,
            sendInput,
            emitWindow: (payload: unknown) => tracer?.emitBenchWindow(payload),
            getClockOffsetMs: () => tm.clockOffsetMs(),
            getStats: () => transport!.getStats(),
          } as never);
          b.start();
          bench = b;
          (window as unknown as Record<string, unknown>)["__qBench"] = b.handle();
        })
        .catch((err: unknown) => {
          reportBestEffortFailure("silent-debug", "bench: load instrument", err);
        });
    }
  }

  function handleRecovery(next: RecoveryState): void {
    set({ recovery: next });
    // L6 — `superseded` must never reach the mint below; the controller has
    // already latched itself stopped, so this notification arrives once.
    if (next.phase === "superseded") {
      if (!destroyed) cfg.callbacks.onSessionTakenOver();
      return;
    }
    if (next.phase !== "failed" || mintInFlight || destroyed) return;
    mintInFlight = true;
    void deps
      .mintSignalingToken(cfg.authToken, cfg.sessionId)
      .then((res) => {
        // apiFetch resolves a bodyless 2xx to undefined; a malformed reconnect
        // response is a failed reconnect, not a TypeError at the user.
        const signaling = res?.signaling;
        if (!signaling?.url || !signaling?.token) throw new Error("malformed signaling envelope");
        if (destroyed) return;
        handoff = true; // L4 — latch BEFORE the page re-seats coords
        cfg.callbacks.onReplacementSignaling({
          url: signaling.url,
          token: signaling.token,
          // #509: from the mint that produced these coords, not the launch
          iceServers: signaling.ice_servers ?? [],
        });
      })
      .catch((err: unknown) => {
        // only server-authored text is ever shown
        const detail = err instanceof ApiError ? err.message : undefined;
        if (!(err instanceof ApiError)) {
          reportBestEffortFailure("silent-debug", "session: mint replacement signaling token", err);
        }
        if (destroyed) return;
        set({ status: "Reconnect failed — this session can no longer be resumed" });
        cfg.callbacks.onReconnectFailed(detail);
      })
      .finally(() => {
        mintInFlight = false;
      });
  }

  return {
    getSnapshot: () => snapshot,

    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },

    start() {
      if (started || destroyed) return;
      started = true;

      // First decoded frame. `loadeddata` is the portable signal; rVFC fires on
      // the first presented frame and is preferred where it exists. Both are
      // one-shot, and `set` is a no-op after destroy (L5).
      const onFirstFrame = () => set({ firstFrame: true });
      cfg.videoEl.addEventListener("loadeddata", onFirstFrame, { once: true });
      const video = cfg.videoEl as HTMLVideoElement & {
        requestVideoFrameCallback?: (cb: () => void) => number;
        cancelVideoFrameCallback?: (handle: number) => void;
      };
      const rvfcHandle =
        typeof video.requestVideoFrameCallback === "function"
          ? video.requestVideoFrameCallback(onFirstFrame)
          : null;
      firstFrameCleanup = () => {
        cfg.videoEl.removeEventListener("loadeddata", onFirstFrame);
        // A pending rVFC is not cancelled by removing the listener; left armed it
        // keeps firing at a destroyed session's element.
        if (rvfcHandle !== null) video.cancelVideoFrameCallback?.(rvfcHandle);
      };

      const tr = deps.createTracer(cfg.sessionId, cfg.authToken);
      tr.start();
      tracer = tr;

      transport = deps.createTransport({
        url: cfg.signaling.url,
        token: cfg.signaling.token,
        initialPlayoutMs,
        requestOfferOnOpen: cfg.signaling.replacement,
        // A missing key means the same thing as an empty list: no ICE servers
        // configured, host candidates only (control-api.md #509).
        iceServers: cfg.signaling.iceServers ?? [],

        onTrack: (stream) => {
          cfg.videoEl.srcObject = stream;
          // No Cast/AirPlay probing on a WebRTC-fed element.
          cfg.videoEl.disableRemotePlayback = true;
          void cfg.videoEl.play();
          // L2 — armed here, where videoReceiver is guaranteed set.
          if (!hasOverride && transport?.videoReceiver && !playout) {
            let prevPlayoutMs = initialPlayoutMs;
            const ctl = new PlayoutController({
              receiver: transport.videoReceiver,
              playout0Ms: initialPlayoutMs,
              onChange: (ms) => {
                const from = prevPlayoutMs;
                prevPlayoutMs = ms;
                tracer?.emitPlayoutChanged(from, ms, ms > from ? "degrade" : "recover");
              },
            });
            ctl.start();
            playout = ctl;
          }
        },

        onStatus: (msg) => {
          set({ status: msg });
          // The signaling relay closes with 4500 when the host goes offline;
          // ICE also fails. Either is enough to warrant a poll.
          const isDisconnect =
            msg.includes("4500") ||
            msg.includes("host offline") ||
            msg === "ICE failed — network issue";
          if (isDisconnect) cfg.callbacks.onDisconnectSuspected();
        },

        onChannel: (ch) => {
          // The channel may ALREADY be open when ondatachannel fires — check
          // readyState before wiring onopen, or the event is silently missed.
          if (ch.readyState === "open") activateChannel(ch);
          else ch.onopen = () => activateChannel(ch);
          ch.onclose = () => onChannelGone();
        },

        onRecoveryState: handleRecovery,
      });

      transport.onSignalingOpen = () => set({ wsOpen: true });

      transport.onWebRtcStateChange = (kind, from, to) => {
        tracer?.emitWebRtcStateChanged(kind, from, to);
        // Latched: `connected` may be followed by `disconnected` while recovery
        // works, and the launch step must not un-happen.
        if (kind === "ice" && ICE_UP.includes(to)) set({ pcConnected: true });
        // #482: the launch screen needs the ICE state to tell "nobody picked
        // this session up" apart from "the host is running it and media cannot
        // reach you". This callback already exists for the tracer, so the state
        // costs nothing extra to surface.
        if (kind === "ice") set({ iceState: asTransportState(to) });
      };

      // #304: audio arrives on a separate PeerConnection. Attach it to a hidden
      // <audio> element so it plays without coupling the video PC's jitter
      // buffer — the whole point of the split.
      transport.onAudioTrack = (stream) => {
        const audio = document.createElement("audio");
        audio.autoplay = true;
        audio.srcObject = stream;
        audio.style.display = "none";
        document.body.appendChild(audio);
        audioCleanup = () => {
          audio.srcObject = null;
          audio.remove();
        };
      };
    },

    destroy() {
      if (destroyed) return;
      onChannelGone();
      destroyed = true; // after onChannelGone, so its snapshot reset publishes
      audioCleanup?.();
      audioCleanup = null;
      firstFrameCleanup?.();
      firstFrameCleanup = null;
      // L4 — a replacement rebuilds the browser transports while deliberately
      // leaving the host session and its app alive; an ordinary unmount says bye.
      transport?.close(!handoff);
      transport = null;
    },

    engage: () => captureEngage?.() ?? null,
    release: () => captureRelease?.(),
    syncKeyboardLock: () => captureKbSync?.(),
    isCaptured: () => captured,

    cancelRecovery: () => transport?.cancelRecovery(),
    hasMicSlot: () => transport?.hasMicSlot() ?? false,
    attachMicTrack: (track) => transport?.attachMicTrack(track) ?? Promise.resolve(),
    detachMicTrack: () => transport?.detachMicTrack() ?? Promise.resolve(),

    registerTelemetry(fn) {
      telemetrySubs.add(fn);
      return () => {
        telemetrySubs.delete(fn);
      };
    },

    summary: () => sink.summary(),
  };
}
