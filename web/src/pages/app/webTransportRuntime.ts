// The session runtime for a WebTransport session (#1031, #1032, #1033): transport -> DecodeStage
// (or PyroWaveStage) -> presenter, with input capture on the transport's input paths, game audio
// through AudioOut and the microphone through MicOut, behind the same SessionRuntime surface the
// page drives for WebRTC.

import { getSession, mintTransportToken } from "../../api/library";
import { ApiError } from "../../api/client";
import type { WebTransportCoords } from "../../api/types";
import { setupCapture } from "../../input/capture";
import { NEUTRAL_MOUSE, type MouseSettings } from "../../input/mouseScale";
import type { BenchController } from "../../bench/benchController";
import { isBenchModeEnabled } from "../../bench/benchFlag";
import type { PostedCounters, TelemetrySnapshot } from "../../webrtc/telemetry";
import { TraceEventEmitter } from "../../webrtc/traceEvents";
import { AudioOut, type AudioOutDeps } from "../../webtransport/audioOut";
import { MicOut, type MicOutDeps } from "../../webtransport/micOut";
import { DecodeStage, type DecodeFailure, type DecodeStageOptions } from "../../webtransport/decodeStage";
import { fallbackCause, fallbackReason } from "../../webtransport/fallback";
import { Playout } from "../../webtransport/playout";
import { createWebGpuPresenter, type Presenter } from "../../webtransport/presenter";
import { WtTelemetry, type WtTelemetryOptions } from "../../webtransport/telemetry";
import { PyroWaveStage, warmPyroWave, type PyroWaveStageOptions } from "../../pyrowave/stage";
import { TransportError, type MediaTransport, type TransportEnd } from "../../webtransport/transport";
import { webTransportInputSender } from "../../webtransport/inputSender";
import { WebTransportMedia } from "../../webtransport/webTransportMedia";
import {
  IDLE_SNAPSHOT,
  type SessionRuntime,
  type SessionRuntimeCallbacks,
  type SessionRuntimeSnapshot,
} from "./sessionRuntime";
import type { SessionSummaryAccumulator } from "./telemetrySink";

const CLOSE_ENDED = 4404;
const CLOSE_TAKEN_OVER = 4410;
const TELEMETRY_MS = 1_000;
/** Stats samples and the clock row are posted on WebRTC's cadence. */
const POST_MS = 5_000;
/** No video frame for this long on a visible tab ends the connection as lost. The compositor
 *  source yields a frame every interval, a still screen included (33 ms at 30 fps): 150 of them. */
export const MEDIA_SILENCE_MS = 5_000;
/** The mime form telemetry's negotiatedCodec uses, from the hello_ack's wire codec. */
const CODEC_MIME: Record<string, string> = { h264: "video/H264", h265: "video/H265", av1: "video/AV1", pyrowave: "video/pyrowave" };
/** The failure card's sentence for a failure that stops the view. Every other failure falls back
 *  to WebRTC (fallbackReason) and never reaches the card. */
export function transportFailureDetail(code: string): string {
  return code === "session_ended" ? "The session ended." : "The WebTransport connection failed.";
}

export interface WebTransportRuntimeDeps {
  createTransport(opts: {
    sessionId: string;
    coords: WebTransportCoords;
    mintToken(): Promise<WebTransportCoords>;
    pyrowave?: boolean;
  }): MediaTransport;
  createPresenter(canvas: HTMLCanvasElement, onFatal: (message: string) => void, opts?: { pyrowave?: boolean }): Promise<Presenter | null>;
  decoder?: Pick<DecodeStageOptions, "createDecoder" | "isConfigSupported">;
  audio?: AudioOutDeps;
  mic?: Partial<Omit<MicOutDeps, "send">>;
  /** Builds the PyroWave decoder's pipelines on the presenter's device; false: do not offer it. */
  warmPyroWave(device: GPUDevice): Promise<boolean>;
  loadPyroWave?: PyroWaveStageOptions["load"];
  setupCapture: typeof setupCapture;
  /** Test seams of the telemetry (its fetch and clocks). */
  telemetry?: Partial<WtTelemetryOptions>;
  createTracer(sessionId: string, authToken: string): Pick<TraceEventEmitter, "start" | "stop" | "emitBenchWindow">;
  loadBench(): Promise<{ BenchController: typeof BenchController }>;
  isBenchModeEnabled(): boolean;
}

export interface WebTransportRuntimeConfig {
  sessionId: string;
  authToken: string;
  coords: WebTransportCoords;
  canvas: HTMLCanvasElement;
  /** The launched tier's fps, for client health's per-frame budget; absent: unknown. */
  tierFps?: number;
  /** The page view's posted cumulative counters; a WebRTC fallback continues them. */
  counters?: PostedCounters;
  /** The session wants PyroWave (`session.pyrowave.wanted`): offer it in `hello` if this browser can decode it. */
  pyrowave?: boolean;
  callbacks: Pick<SessionRuntimeCallbacks, "onDisconnectSuspected" | "onSessionTakenOver"> &
    Partial<Pick<SessionRuntimeCallbacks, "onSummonOverlay" | "onKeyboardLockRefused" | "onNonStandardGamepad">> & {
    /** Terminal and not a fallback: the session ended or WebTransport cannot be used. `detail` is
     *  human copy for the failure card. */
    onTransportFailed(detail: string): void;
    /** WebTransport has been closed and the page should carry on over WebRTC.
     *  `reason` is the `transport.fallback` reason (control-api.md §Transport fallback); `cause`
     *  is the error's own text, for the HUD only (never sent anywhere). */
    onFallback(reason: string, cause: string): void;
  };
  deps?: Partial<WebTransportRuntimeDeps>;
}

const productionDeps: WebTransportRuntimeDeps = {
  createTransport: (o) => new WebTransportMedia(o),
  createPresenter: createWebGpuPresenter,
  createTracer: (sessionId, authToken) => new TraceEventEmitter(sessionId, authToken),
  loadBench: () => import("../../bench/benchController"),
  isBenchModeEnabled,
  warmPyroWave: (device) => warmPyroWave(device),
  setupCapture,
};

export function createWebTransportRuntime(cfg: WebTransportRuntimeConfig): SessionRuntime {
  const deps: WebTransportRuntimeDeps = { ...productionDeps, ...cfg.deps };
  let snapshot: SessionRuntimeSnapshot = {
    ...IDLE_SNAPSHOT,
    status: "connecting over WebTransport…",
  };
  const listeners = new Set<() => void>();
  const telemetrySubs = new Set<(snap: TelemetrySnapshot) => void>();
  let destroyed = false;
  let started = false;
  /** One media reconnect before fallback; connectivity retries leave it unspent. */
  let reconnected = false;
  let stableVideoSince: number | null = null;
  // Thirty seconds of arriving video separates a recovered stream from repeated quick failures.
  const MEDIA_RETRY_RESET_MS = 30_000;
  function noteVideo(): void {
    const now = performance.now();
    if (stableVideoSince === null || lastVideoAt === null || now - lastVideoAt > MEDIA_SILENCE_MS) stableVideoSince = now;
    lastVideoAt = now;
    if (now - stableVideoSince >= MEDIA_RETRY_RESET_MS) reconnected = false;
  }
  let recovering = false;
  /** Our own 4400 for a dead datagram reader (#1403): its reason is the cause of a later stream_lost. */
  let lostCause: string | undefined;
  let recoveryDeadline: ReturnType<typeof setTimeout> | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let wakeRetry: (() => void) | null = null;
  /** Set by the first terminal failure; nothing runs after it. */
  let failed = false;
  let transport: MediaTransport | null = null;
  let presenter: Presenter | null = null;
  let stage: DecodeStage | null = null;
  let playout: Playout | null = null;
  let pyroStage: PyroWaveStage | null = null;
  /** Whether `hello` offers PyroWave; decided once, so a reconnect offers the same. */
  let pyroOffer = false;
  let timer: ReturnType<typeof setInterval> | null = null;
  let capture: ReturnType<typeof setupCapture> | null = null;
  let inputSender: ReturnType<typeof webTransportInputSender> | null = null;
  let captured = false;
  let mouse: MouseSettings = NEUTRAL_MOUSE;
  const telemetry = new WtTelemetry({ sessionId: cfg.sessionId, authToken: cfg.authToken, profileFps: cfg.tierFps, counters: cfg.counters, ...deps.telemetry });
  let postTimer: ReturnType<typeof setInterval> | null = null;
  // The trace is HTTP, not the transport's: bench windows are posted through it as on WebRTC.
  const tracer = deps.createTracer(cfg.sessionId, cfg.authToken);
  let bench: BenchController | null = null;
  /** The decoded frame the bench marker reader reads, for the length of one `onFrame`. */
  let benchFrame: VideoFrame | null = null;
  let benchFaulted = false;
  let audioOut: AudioOut | null = null;
  /** The current AudioOut's dropout totals already given to telemetry; a new AudioOut counts from 0. */
  let audioSeen = { samples: 0, events: 0, underruns: 0 };
  /** The sound starts muted (webrtc/sessionRuntime holdAudio): nothing the app plays before the reveal is heard. */
  let audioHeld = true;
  let micSlot = false;
  let mic: MicOut | null = null;
  const startedAt = performance.now();
  /** When video last arrived; null until the stream has started. */
  let lastVideoAt: number | null = null;
  /** When the last tick ran; null until ticking starts (connecting is not a stall). */
  let lastTickAt: number | null = null;

  const fpsSeries: number[] = [];
  const latencySeries: number[] = [];

  const set = (patch: Partial<SessionRuntimeSnapshot>) => {
    if (destroyed) return;
    snapshot = { ...snapshot, ...patch };
    for (const fn of listeners) fn();
  };

  /** The last transport's close: a fallback waits for it, also when a reconnect closed it. */
  let closing: Promise<void> | void = undefined;

  const stop = () => {
    if (recoveryDeadline !== null) clearTimeout(recoveryDeadline);
    recoveryDeadline = null;
    wakeRetry?.();
    if (timer !== null) clearInterval(timer);
    timer = null;
    if (postTimer !== null) clearInterval(postTimer);
    postTimer = null;
    // Bench stops first: its last window reads the clock offset and goes out through the tracer.
    bench?.stop();
    bench = null;
    tracer.stop();
    // The last windows, and the clock row a short session has not posted yet.
    void telemetry.flush();
    document.removeEventListener("visibilitychange", onVisible);
    teardownInput();
    mic?.stop();
    harvestAudio();
    audioOut?.close();
    closing = transport?.close() ?? closing;
    stage?.close();
    playout?.close();
    pyroStage?.close();
    presenter?.destroy();
    return closing;
  };

  // A failure the contract falls back on hands the session to WebRTC; the rest end this view.
  const fail = (code: string, terminal = false, message?: string, err?: unknown) => {
    if (failed || destroyed) return;
    failed = true;
    const closed = stop();
    const reason = terminal ? null : fallbackReason(code);
    if (reason) {
      const cause = fallbackCause(code, err);
      console.warn("webtransport: falling back to WebRTC:", cause);
      // WebRTC's own input path takes over; nothing is left capturing for this connection.
      set({ status: "falling back to WebRTC…", channelOpen: false, inputCaptured: false, pointerLocked: false });
      // Close first (control-api.md §Transport fallback): the page opens WebRTC only once the
      // WebTransport session has been told to close, which close() bounds to a short wait.
      void Promise.resolve(closed).then(() => {
        if (!destroyed) cfg.callbacks.onFallback(reason, cause);
      });
      return;
    }
    const detail = message ?? transportFailureDetail(code);
    // Before the reveal the loader shows the page's failure card; after it, `recovery`
    // `failed` puts the reason on the session's existing recovery banner.
    set({ status: detail, channelOpen: false, inputCaptured: false, pointerLocked: false, recovery: { phase: "failed", attempt: 0, maxAttempts: 0, message: detail } });
    cfg.callbacks.onTransportFailed(detail);
  };

  // A hidden tab's decoder falls behind; one keyframe when it shows again resyncs it.
  function onVisible(): void {
    if (document.visibilityState !== "visible") return;
    // A tab back from hidden gets the whole bound again.
    if (lastVideoAt !== null) lastVideoAt = performance.now();
    stableVideoSince = null;
    telemetry.breakCadence();
    playout?.reset();
    transport?.requestKeyframe("loss");
  }

  /** Gives telemetry the dropouts the current player counted since the last call, once it is measuring. */
  const harvestAudio = () => {
    const audio = audioOut?.stats();
    if (audio?.measured) {
      telemetry.addAudioDropouts(audio.concealedSamples - audioSeen.samples, audio.concealmentEvents - audioSeen.events);
      // Silence runs are cumulative per player, and a player that has not played audio has none.
      if (audio.buffer) telemetry.addAudioUnderruns(audio.buffer.underruns - audioSeen.underruns);
      audioSeen = {
        samples: audio.concealedSamples,
        events: audio.concealmentEvents,
        underruns: audio.buffer?.underruns ?? audioSeen.underruns,
      };
    }
    telemetry.setAudioBuffer(audio?.buffer?.minMs ?? null, audio?.buffer?.targetMs ?? null);
    return audio;
  };

  const tick = () => {
    // A late tick is a stalled page, not a silent connection: its datagrams are still queued.
    // Only the stall itself is excused, or a page whose ticks always run late would never fire.
    const now = performance.now();
    if (lastVideoAt !== null && lastTickAt !== null && now - lastTickAt > 2 * TELEMETRY_MS) {
      lastVideoAt = Math.min(now, lastVideoAt + now - lastTickAt - TELEMETRY_MS);
      stableVideoSince = null;
    }
    lastTickAt = now;
    // A connection that is up and carries no media never ends by itself.
    if (
      transport &&
      snapshot.channelOpen &&
      lastVideoAt !== null &&
      document.visibilityState === "visible" &&
      now - lastVideoAt > MEDIA_SILENCE_MS
    ) {
      onEnd(transport, { code: null, reason: "media_silent" });
      if (failed) return;
    }
    const audio = harvestAudio();
    telemetry.setPlayout(playout?.cushion() ?? 0, playout?.arrivalJitterMs() ?? null);
    const snap = telemetry.tick();
    if (audio?.avOffsetMs != null) {
      // Read by the bench and the hardware pass: how much later than its video the sound is heard.
      cfg.canvas.dataset.avOffsetMs = audio.avOffsetMs.toFixed(1);
      cfg.canvas.dataset.audioUnderrunMs = audio.underrunMs.toFixed(0);
    }
    fpsSeries.push(snap.fps);
    if (snap.g2gMs != null) latencySeries.push(snap.g2gMs);
    for (const fn of telemetrySubs) fn(snap);
  };

  /** A takeover is the page's own verdict, not a transport failure. */
  const takenOver = () => {
    if (failed || destroyed) return;
    failed = true;
    stop();
    set({ status: "taken over", channelOpen: false, inputCaptured: false, pointerLocked: false });
    cfg.callbacks.onSessionTakenOver();
  };

  async function run(): Promise<void> {
    let p: Presenter | null;
    try {
      p = await deps.createPresenter(
        cfg.canvas,
        (message) => {
          console.warn("presenter:", message);
          fail("gpu_failed", false, undefined, message);
        },
        { pyrowave: cfg.pyrowave === true },
      );
    } catch (err) {
      // requestAdapter, requestDevice or configure refused.
      console.warn("presenter:", err);
      fail("gpu_failed", false, undefined, err);
      return;
    }
    // Destroyed, or the device failed while the presenter was being made: no transport then.
    if (destroyed || failed) {
      p?.destroy();
      return;
    }
    if (!p) {
      fail("no_webgpu");
      return;
    }
    presenter = p;
    const device = p.device;
    pyroOffer =
      cfg.pyrowave === true && device !== undefined && p.presentTexture !== undefined && (await deps.warmPyroWave(device));
    if (destroyed || failed) return;
    const presented = (captureUs: number) => {
      audioOut?.noteVideo(captureUs);
      telemetry.presented(captureUs);
      if (!snapshot.firstFrame) set({ firstFrame: true, status: "streaming over WebTransport" });
    };
    if (pyroOffer && device) {
      pyroStage = new PyroWaveStage({
        device,
        present: (texture) => p.presentTexture?.(texture) ?? false,
        onPresented: (captureUs) => presented(captureUs),
        fallback: (reason) => transport?.codecFallback(reason),
        load: deps.loadPyroWave,
      });
    }
    const pl = new Playout({
      show: (frame, captureUs) => {
        readBench(frame);
        if (p.present(frame)) presented(captureUs);
      },
    });
    playout = pl;
    const s = new DecodeStage({
      ...deps.decoder,
      onFrame: (frame) => {
        const captureUs = frame.timestamp;
        telemetry.decodeOut(captureUs);
        pl.push(frame, captureUs);
      },
      requestKeyframe: (reason) => transport?.requestKeyframe(reason),
      onFatal: (reason: DecodeFailure) => fail(reason),
    });
    stage = s;
    await connectTransport(cfg.coords, false);
  }

  /** ?bench=1 only: the marker is read off the frame about to be presented, at its own size, before
   *  the presenter takes (and closes) it. A fault in the instrument must not cost the frame.
   *  The window's present_ms is therefore the presenter's submit, where a WebRTC session's is
   *  RVFC's expectedDisplayTime: an A/B between the two must say so (#1035). The stats' present
   *  cadence is not: telemetry stamps it at the display frame (#1328), the marker cannot wait for it. */
  function readBench(frame: VideoFrame): void {
    if (!bench) return;
    benchFrame = frame;
    try {
      bench.onFrame();
    } catch (err) {
      // Once: a persistent fault would otherwise log every frame.
      if (!benchFaulted) console.warn("bench:", err);
      benchFaulted = true;
    } finally {
      benchFrame = null;
    }
  }

  /** Opens a transport with `coords` and returns once it is up, or the view has failed.
   *  A `reconnecting` failure is rethrown so the recovery loop can check connectivity. */
  async function connectTransport(coords: WebTransportCoords, reconnecting: boolean): Promise<void> {
    // A mint that was in flight when the view ended must not open a connection.
    if (destroyed || failed) return;
    const t = deps.createTransport({ sessionId: cfg.sessionId, coords, mintToken: mintCoords, pyrowave: pyroOffer });
    transport = t;
    if (reconnecting) {
      telemetry.breakCadence();
      playout?.reset();
    }
    let lostOnThisConnection = 0;
    try {
      await t.connect({
        streams: (streams) => {
          micSlot = streams.mic !== null;
          // A reconnect's streams replace the previous connection's player: its dropouts so far are kept.
          harvestAudio();
          audioOut?.close();
          audioOut = null;
          audioSeen = { samples: 0, events: 0, underruns: 0 };
          if (!streams.audio || failed || destroyed) return;
          const out = (audioOut = new AudioOut(deps.audio));
          if (!audioHeld) out.release();
          // Audio must never end the session: a failure to start leaves the stream silent.
          void out.start(streams.audio).catch((err: unknown) => console.warn("audio:", err));
        },
        audio: (packet) => audioOut?.packet(packet),
        config: (c) => {
          if (destroyed || failed || t !== transport) return;
          telemetry.setCodec(CODEC_MIME[c.codec] ?? c.codec);
          if (c.switchReason) console.info(`WebTransport codec ${c.codec}: ${c.switchReason}`);
          // capture.ts scales the mouse by the stream size, which a canvas does not know.
          cfg.canvas.dataset.streamW = String(c.width);
          cfg.canvas.dataset.streamH = String(c.height);
          if (c.codec !== "pyrowave") {
            pyroStage?.leave();
            playout?.setFps(c.fps);
            stage?.config(c);
          } else if (pyroStage) {
            pyroStage.config(c);
          } else {
            // Never offered, so never expected: say so, and the agent sends the standard codec.
            t.codecFallback("decode_error");
          }
        },
        unit: (u) => {
          if (destroyed || failed || t !== transport) return;
          noteVideo();
          telemetry.bytes(u.data.length);
          telemetry.decodeIn(u.captureUs);
          stage?.unit(u);
        },
        pyrowave: (f) => {
          if (destroyed || failed || t !== transport) return;
          noteVideo();
          for (const b of f.packets) telemetry.bytes(b.byteLength);
          pyroStage?.frame(f);
        },
        keyframeRequested: () => telemetry.keyframeRequested(),
        lost: (fragments) => {
          // The count is this connection's own, from 0: a reconnect's must add, not replace.
          if (t === transport) telemetry.addLost(fragments - lostOnThisConnection);
          lostOnThisConnection = fragments;
        },
        rtt: (ms, agentWallMs) => {
          if (t === transport) telemetry.pong(ms, agentWallMs);
        },
        end: (e) => onEnd(t, e),
      });
    } catch (err) {
      if (destroyed || failed) return;
      if (reconnecting) throw err;
      const code = err instanceof TransportError ? err.code : "internal";
      if (code === "taken_over") {
        takenOver();
      } else if (code !== "closed") {
        if (code === "session_ended") cfg.callbacks.onDisconnectSuspected();
        fail(code, false, undefined, err);
      }
      return;
    }
    if (destroyed || failed || t !== transport) return;
    // hello_ack: the control stream is up, and it is what carries input on this transport.
    set({ wsOpen: true, pcConnected: true, channelOpen: true, iceState: "connected" });
    // A reconnect's connection gets the whole bound to deliver its first frame.
    if (lastVideoAt !== null) lastVideoAt = performance.now();
    startInput(t);
    if (timer === null) {
      telemetry.startWindow();
      lastTickAt = performance.now();
      timer = setInterval(tick, TELEMETRY_MS);
      postTimer = setInterval(() => void telemetry.flush(), POST_MS);
      tracer.start();
      startBench();
      document.addEventListener("visibilitychange", onVisible);
    }
  }

  /** `?bench=1` only: it reads pixels back every frame, so an ordinary session never loads it. */
  function startBench(): void {
    if (!deps.isBenchModeEnabled() || bench !== null) return;
    void deps
      .loadBench()
      .then(({ BenchController: Ctor }) => {
        if (destroyed || failed || bench !== null) return;
        const b = new Ctor({
          source: () => (benchFrame ? { image: benchFrame, width: benchFrame.displayWidth, height: benchFrame.displayHeight } : null),
          sendInput: (msg) => inputSender?.send(msg),
          emitWindow: (payload) => tracer.emitBenchWindow(payload as unknown as Record<string, unknown>),
          getClockOffsetMs: () => telemetry.clockOffsetMs(),
        });
        b.start();
        bench = b;
        (window as unknown as Record<string, unknown>)["__qBench"] = b.handle();
      })
      .catch((err: unknown) => console.warn("bench: load instrument", err));
  }

  /** Input capture belongs to one connection: built from its input paths, torn down with it. */
  function startInput(t: MediaTransport): void {
    teardownInput();
    const sender = (inputSender = webTransportInputSender(t.input));
    capture = deps.setupCapture({
      videoEl: cfg.canvas,
      sendInput: sender.send,
      channel: sender,
      onCaptureChange: ({ captured: isCaptured, pointerLocked }) => {
        captured = isCaptured;
        set({ inputCaptured: isCaptured, pointerLocked });
      },
      isFullscreen: () => document.fullscreenElement != null,
      onSummonOverlay: () => cfg.callbacks.onSummonOverlay?.(),
      onKeyboardLockRefused: (error) => cfg.callbacks.onKeyboardLockRefused?.(error),
      onNonStandardGamepad: (pad) => cfg.callbacks.onNonStandardGamepad?.(pad),
      mouse,
    });
  }

  /** Releases held keys through the connection they were sent on, so this goes before it closes. */
  function teardownInput(): void {
    capture?.cleanup();
    capture = null;
    inputSender?.dispose();
    inputSender = null;
    captured = false;
  }

  const mintCoords = async () =>
    (await mintTransportToken(cfg.authToken, cfg.sessionId)).transport_options.webtransport;

  /** One connection lost after it was up: a fresh transport token gets one try, then WebRTC. */
  function onEnd(t: MediaTransport, e: TransportEnd): void {
    if (destroyed || failed || t !== transport) return;
    if (e.code === CLOSE_TAKEN_OVER) {
      takenOver();
      return;
    }
    // The page asks the control plane why; its verdict outranks this one.
    cfg.callbacks.onDisconnectSuspected();
    if (e.code === CLOSE_ENDED) {
      fail("session_ended");
      return;
    }
    lostCause = e.code === 4400 && e.reason.startsWith("datagrams:") ? e.reason : undefined;
    stableVideoSince = null;
    transport = null; // the closed one's own `end` is then not this view's
    teardownInput();
    closing = t.close();
    // The active recovery loop notices that its replacement ended and tries again.
    set({ channelOpen: false, inputCaptured: false, pointerLocked: false });
    if (!recovering) void recoverTransport();
  }

  // Like signaling recovery, connectivity failures defer rather than spend the media retry.
  async function recoverTransport(): Promise<void> {
    recovering = true;
    set({ status: "reconnecting over WebTransport…", channelOpen: false, inputCaptured: false, pointerLocked: false,
      recovery: { phase: "reconnecting", attempt: 0, maxAttempts: 1, message: "Connection lost. Reconnecting…" } });
    recoveryDeadline = setTimeout(() => fail("stream_lost", true), 60_000);
    let attempt = 0;
    let connectivityFailed = false;
    let endedAtOnce = 0;
    try {
      while (!destroyed && !failed) {
        const attemptedAt = performance.now();
        try {
          const { session } = await getSession(cfg.authToken, cfg.sessionId);
          if (destroyed || failed) return;
          if (session.state !== "running") {
            fail("session_ended");
            return;
          }
          if (reconnected && !connectivityFailed) {
            fail("stream_lost", false, undefined, lostCause);
            return;
          }
          const coords = await mintCoords();
          if (destroyed || failed) return;
          await connectTransport(coords, true);
          if (destroyed || failed) return;
          if (transport === null) {
            // The replacement connected and ended at once. With the network up that is a broken
            // media path: one more try, then fall back, never a tight loop of token mints.
            if (connectivityFailed) await retryWait(Math.min(1_000 * 2 ** attempt++, 4_000), attemptedAt);
            else if (endedAtOnce++ >= 1) {
              fail("stream_lost", false, undefined, lostCause);
              return;
            }
            continue;
          }
          // A network outage is independent of the bounded retry for a broken media path.
          if (!connectivityFailed) reconnected = true;
          set({ recovery: { phase: "recovered", attempt: 1, maxAttempts: 1, message: "Connection recovered" },
            status: "streaming over WebTransport" });
          return;
        } catch (err) {
          if (destroyed || failed) return;
          if (err instanceof TransportError && err.code === "taken_over") {
            takenOver();
            return;
          }
          if ((err instanceof TransportError && err.code === "session_ended") ||
              (err instanceof ApiError && err.status === 404)) {
            fail("session_ended");
            return;
          }
          if (!(err instanceof TypeError) && !(err instanceof ApiError && err.status >= 500)) {
            // A failed QUIC path is a fallback only if the control plane is still reachable.
            try {
              const { session } = await getSession(cfg.authToken, cfg.sessionId);
              if (destroyed || failed) return;
              fail(session.state === "running" ? "stream_lost" : "session_ended", false, undefined, err);
              return;
            } catch (probeError) {
              if (probeError instanceof ApiError && probeError.status < 500) {
                fail(probeError.status === 404 ? "session_ended" : "stream_lost", true);
                return;
              }
            }
          }
          if (destroyed || failed) return;
          connectivityFailed = true;
          const t = transport;
          transport = null;
          closing = t?.close() ?? closing;
          await retryWait(Math.min(1_000 * 2 ** attempt++, 4_000), attemptedAt);
        }
      }
    } finally {
      recovering = false;
      if (recoveryDeadline !== null) clearTimeout(recoveryDeadline);
      recoveryDeadline = null;
    }
  }

  function retryWait(ms: number, attemptedAt: number): Promise<void> {
    return new Promise((resolve) => {
      const wake = () => {
        if (retryTimer !== null) clearTimeout(retryTimer);
        retryTimer = null;
        window.removeEventListener("online", online);
        wakeRetry = null;
        resolve();
      };
      const online = () => {
        // navigator.onLine is a hint; only the next successful HTTPS read proves reachability.
        if (!navigator.onLine) return;
        if (retryTimer !== null) clearTimeout(retryTimer);
        retryTimer = setTimeout(wake, Math.max(0, 1_000 - (performance.now() - attemptedAt)));
      };
      wakeRetry = wake;
      window.addEventListener("online", online);
      retryTimer = setTimeout(wake, ms);
    });
  }

  return {
    getSnapshot: () => snapshot,
    subscribe(fn) {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    start() {
      if (started || destroyed) return;
      started = true;
      void run();
    },
    destroy() {
      if (destroyed) return;
      destroyed = true;
      stop();
      listeners.clear();
      telemetrySubs.clear();
    },
    engage: () => capture?.engage() ?? null,
    release: () => capture?.release(),
    syncKeyboardLock: () => capture?.syncKeyboardLock(),
    setMouse(settings) {
      mouse = settings;
      capture?.setMouse(settings);
    },
    setVirtualPad: (pad) => capture?.setVirtualPad(pad),
    isCaptured: () => captured,
    cancelRecovery() {
      if (recovering) fail("stream_lost", true, "Connection recovery cancelled.");
    },
    hasMicSlot: () => micSlot && !failed && !destroyed,
    async attachMicTrack(track) {
      // Nothing may start after stop(): the transport is closed and nobody would tear it down.
      if (failed || destroyed) return;
      mic ??= new MicOut({ send: (d) => transport?.sendMic(d) ?? false, ...deps.mic });
      mic.start(track);
    },
    async detachMicTrack() {
      mic?.stop();
    },
    registerTelemetry(fn) {
      telemetrySubs.add(fn);
      return () => telemetrySubs.delete(fn);
    },
    summary: (): SessionSummaryAccumulator => ({
      elapsedMs: performance.now() - startedAt,
      fps: fpsSeries,
      latency: latencySeries,
    }),
    releaseAudio() {
      audioHeld = false;
      audioOut?.release();
    },
    holdAudio() {
      audioHeld = true;
      audioOut?.hold();
    },
  };
}
