// The session runtime for a WebTransport session (#1031, #1032, #1033): transport -> DecodeStage
// (or PyroWaveStage) -> presenter, with input capture on the transport's input paths, game audio
// through AudioOut and the microphone through MicOut, behind the same SessionRuntime surface the
// page drives for WebRTC.

import { mintTransportToken } from "../../api/library";
import type { WebTransportCoords } from "../../api/types";
import { setupCapture } from "../../input/capture";
import { NEUTRAL_MOUSE, type MouseSettings } from "../../input/mouseScale";
import type { BenchController } from "../../bench/benchController";
import { isBenchModeEnabled } from "../../bench/benchFlag";
import type { TelemetrySnapshot } from "../../webrtc/telemetry";
import { TraceEventEmitter } from "../../webrtc/traceEvents";
import { AudioOut, type AudioOutDeps } from "../../webtransport/audioOut";
import { MicOut, type MicOutDeps } from "../../webtransport/micOut";
import { DecodeStage, type DecodeFailure, type DecodeStageOptions } from "../../webtransport/decodeStage";
import { fallbackReason } from "../../webtransport/fallback";
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
  /** The session wants PyroWave (`session.pyrowave.wanted`): offer it in `hello` if this browser can decode it. */
  pyrowave?: boolean;
  callbacks: Pick<SessionRuntimeCallbacks, "onDisconnectSuspected" | "onSessionTakenOver"> &
    Partial<Pick<SessionRuntimeCallbacks, "onSummonOverlay" | "onKeyboardLockRefused" | "onNonStandardGamepad">> & {
    /** Terminal and not a fallback: the session ended or WebTransport cannot be used. `detail` is
     *  human copy for the failure card. */
    onTransportFailed(detail: string): void;
    /** WebTransport has been closed and the page should carry on over WebRTC.
     *  `reason` is the `transport.fallback` reason (control-api.md §Transport fallback). */
    onFallback(reason: string): void;
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
  /** The one reconnect a lost stream gets before the view falls back. */
  let reconnected = false;
  /** Set by the first terminal failure; nothing runs after it. */
  let failed = false;
  let transport: MediaTransport | null = null;
  let presenter: Presenter | null = null;
  let stage: DecodeStage | null = null;
  let pyroStage: PyroWaveStage | null = null;
  /** Whether `hello` offers PyroWave; decided once, so a reconnect offers the same. */
  let pyroOffer = false;
  let timer: ReturnType<typeof setInterval> | null = null;
  let capture: ReturnType<typeof setupCapture> | null = null;
  let inputSender: ReturnType<typeof webTransportInputSender> | null = null;
  let captured = false;
  let mouse: MouseSettings = NEUTRAL_MOUSE;
  const telemetry = new WtTelemetry({ sessionId: cfg.sessionId, authToken: cfg.authToken, profileFps: cfg.tierFps, ...deps.telemetry });
  let postTimer: ReturnType<typeof setInterval> | null = null;
  // The trace is HTTP, not the transport's: bench windows are posted through it as on WebRTC.
  const tracer = deps.createTracer(cfg.sessionId, cfg.authToken);
  let bench: BenchController | null = null;
  /** The decoded frame the bench marker reader reads, for the length of one `onFrame`. */
  let benchFrame: VideoFrame | null = null;
  let benchFaulted = false;
  let audioOut: AudioOut | null = null;
  /** The sound starts muted (webrtc/sessionRuntime holdAudio): nothing the app plays before the reveal is heard. */
  let audioHeld = true;
  let micSlot = false;
  let mic: MicOut | null = null;
  const startedAt = performance.now();

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
    audioOut?.close();
    closing = transport?.close() ?? closing;
    stage?.close();
    pyroStage?.close();
    presenter?.destroy();
    return closing;
  };

  // A failure the contract falls back on hands the session to WebRTC; the rest end this view.
  const fail = (code: string) => {
    if (failed || destroyed) return;
    failed = true;
    const closed = stop();
    const reason = fallbackReason(code);
    if (reason) {
      // WebRTC's own input path takes over; nothing is left capturing for this connection.
      set({ status: "falling back to WebRTC…", channelOpen: false, inputCaptured: false, pointerLocked: false });
      // Close first (control-api.md §Transport fallback): the page opens WebRTC only once the
      // WebTransport session has been told to close, which close() bounds to a short wait.
      void Promise.resolve(closed).then(() => {
        if (!destroyed) cfg.callbacks.onFallback(reason);
      });
      return;
    }
    const detail = transportFailureDetail(code);
    // Before the reveal the loader shows the page's failure card; after it, `recovery`
    // `failed` puts the reason on the session's existing recovery banner.
    set({ status: detail, channelOpen: false, inputCaptured: false, pointerLocked: false, recovery: { phase: "failed", attempt: 0, maxAttempts: 0, message: detail } });
    cfg.callbacks.onTransportFailed(detail);
  };

  // A hidden tab's decoder falls behind; one keyframe when it shows again resyncs it.
  function onVisible(): void {
    if (document.visibilityState !== "visible") return;
    telemetry.breakCadence();
    transport?.requestKeyframe("loss");
  }

  const tick = () => {
    const snap = telemetry.tick();
    const audio = audioOut?.stats();
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
          fail("gpu_failed");
        },
        { pyrowave: cfg.pyrowave === true },
      );
    } catch (err) {
      // requestAdapter, requestDevice or configure refused.
      console.warn("presenter:", err);
      fail("gpu_failed");
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
    const s = new DecodeStage({
      ...deps.decoder,
      onFrame: (frame) => {
        const captureUs = frame.timestamp;
        telemetry.decodeOut(captureUs);
        if (bench) {
          // ?bench=1 only: the marker is read off the decoded frame, at its own size, before
          // the presenter takes (and closes) it. A fault in the instrument must not cost the frame.
          // The window's present_ms is therefore decoder output, where a WebRTC session's is
          // RVFC's expectedDisplayTime: an A/B between the two must say so (#1035).
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
        if (p.present(frame)) presented(captureUs);
      },
      requestKeyframe: (reason) => transport?.requestKeyframe(reason),
      onFatal: (reason: DecodeFailure) => fail(reason),
    });
    stage = s;
    await connectTransport(cfg.coords, false);
  }

  /** Opens a transport with `coords` and returns once it is up, or the view has failed.
   *  A `reconnecting` one that fails reports `stream_lost`: the stream was up before. */
  async function connectTransport(coords: WebTransportCoords, reconnecting: boolean): Promise<void> {
    // A mint that was in flight when the view ended must not open a connection.
    if (destroyed || failed) return;
    const t = deps.createTransport({ sessionId: cfg.sessionId, coords, mintToken: mintCoords, pyrowave: pyroOffer });
    transport = t;
    if (reconnecting) telemetry.breakCadence();
    let lostOnThisConnection = 0;
    try {
      await t.connect({
        streams: (streams) => {
          micSlot = streams.mic !== null;
          // A reconnect's streams replace the previous connection's player.
          audioOut?.close();
          audioOut = null;
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
          telemetry.bytes(u.data.length);
          telemetry.decodeIn(u.captureUs);
          stage?.unit(u);
        },
        pyrowave: (f) => {
          if (destroyed || failed || t !== transport) return;
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
      const code = err instanceof TransportError ? err.code : "internal";
      if (code === "taken_over") {
        takenOver();
      } else if (code !== "closed") {
        if (code === "session_ended") cfg.callbacks.onDisconnectSuspected();
        fail(reconnecting && code !== "session_ended" ? "stream_lost" : code);
      }
      return;
    }
    if (destroyed || failed) return;
    // hello_ack: the control stream is up, and it is what carries input on this transport.
    set({ wsOpen: true, pcConnected: true, channelOpen: true, iceState: "connected" });
    startInput(t);
    if (timer === null) {
      telemetry.startWindow();
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
    if (reconnected) {
      fail("stream_lost");
      return;
    }
    reconnected = true;
    transport = null; // the closed one's own `end` is then not this view's
    teardownInput(); // the new connection builds its own
    closing = t.close();
    set({ status: "reconnecting over WebTransport…", channelOpen: false, inputCaptured: false, pointerLocked: false });
    mintCoords().then((c) => connectTransport(c, true), () => fail("stream_lost"));
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
    isCaptured: () => captured,
    cancelRecovery() {},
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
