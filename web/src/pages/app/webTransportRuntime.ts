// The session runtime for a WebTransport session (#1031, #1032, #1033): transport -> DecodeStage ->
// presenter, with input capture on the transport's input paths, game audio through AudioOut and
// the microphone through MicOut, behind the same SessionRuntime surface the page drives for WebRTC.

import { mintTransportToken } from "../../api/library";
import type { WebTransportCoords } from "../../api/types";
import { setupCapture } from "../../input/capture";
import { NEUTRAL_MOUSE, type MouseSettings } from "../../input/mouseScale";
import { median, percentile } from "../../lib/stats";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../webrtc/telemetry";
import { AudioOut, type AudioOutDeps } from "../../webtransport/audioOut";
import { MicOut, type MicOutDeps } from "../../webtransport/micOut";
import { DecodeStage, type DecodeFailure, type DecodeStageOptions } from "../../webtransport/decodeStage";
import { createWebGpuPresenter, type Presenter } from "../../webtransport/presenter";
import { TransportError, type MediaTransport } from "../../webtransport/transport";
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
/** capture→present samples kept for the median and p95. */
const G2G_WINDOW = 600;
/** The mime form telemetry's negotiatedCodec uses, from the hello_ack's wire codec. */
const CODEC_MIME: Record<string, string> = { h264: "video/H264", h265: "video/H265", av1: "video/AV1" };
const RELAUNCH = "Launch again without ?transport=webtransport to stream over WebRTC.";

/** Why WebTransport could not go on, as one sentence for the page's failure card. */
export function transportFailureDetail(code: string): string {
  const why: Record<string, string> = {
    auth_failed: "The host refused this device's WebTransport token.",
    session_ended: "The session ended.",
    unsupported_version: "The host speaks a different WebTransport version.",
    takeover_rate_limited: "This session changed device too often; try again in a minute.",
    connect_failed: "The browser could not open a WebTransport connection to the host.",
    connect_timeout: "The host did not start streaming over WebTransport within a minute.",
    stream_lost: "The WebTransport connection to the host was lost.",
    decoder_unsupported: "This browser cannot decode the stream over WebTransport.",
    decode_error: "The stream kept failing to decode.",
    no_webgpu: "WebTransport needs WebGPU, which this browser does not offer.",
    gpu_failed: "The browser's GPU stopped drawing the stream.",
  };
  return `${why[code] ?? "The WebTransport connection failed."} ${RELAUNCH}`;
}

export interface WebTransportRuntimeDeps {
  createTransport(opts: { sessionId: string; coords: WebTransportCoords; mintToken(): Promise<WebTransportCoords> }): MediaTransport;
  createPresenter(canvas: HTMLCanvasElement, onFatal: (message: string) => void): Promise<Presenter | null>;
  decoder?: Pick<DecodeStageOptions, "createDecoder" | "isConfigSupported">;
  audio?: AudioOutDeps;
  mic?: Partial<Omit<MicOutDeps, "send">>;
  setupCapture: typeof setupCapture;
}

export interface WebTransportRuntimeConfig {
  sessionId: string;
  authToken: string;
  coords: WebTransportCoords;
  canvas: HTMLCanvasElement;
  callbacks: Pick<SessionRuntimeCallbacks, "onDisconnectSuspected" | "onSessionTakenOver"> &
    Partial<Pick<SessionRuntimeCallbacks, "onSummonOverlay" | "onKeyboardLockRefused" | "onNonStandardGamepad">> & {
    /** Terminal: WebTransport cannot go on. `detail` is human copy for the failure card. */
    onTransportFailed(detail: string): void;
  };
  deps?: Partial<WebTransportRuntimeDeps>;
}

const productionDeps: WebTransportRuntimeDeps = {
  createTransport: (o) => new WebTransportMedia(o),
  createPresenter: createWebGpuPresenter,
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
  /** Set by the first terminal failure; nothing runs after it. */
  let failed = false;
  let transport: MediaTransport | null = null;
  let presenter: Presenter | null = null;
  let stage: DecodeStage | null = null;
  let timer: ReturnType<typeof setInterval> | null = null;
  let capture: ReturnType<typeof setupCapture> | null = null;
  let inputSender: ReturnType<typeof webTransportInputSender> | null = null;
  let captured = false;
  let mouse: MouseSettings = NEUTRAL_MOUSE;
  let rttMs: number | null = null;
  let negotiatedCodec: string | null = null;
  let audioOut: AudioOut | null = null;
  /** The sound starts muted (webrtc/sessionRuntime holdAudio): nothing the app plays before the reveal is heard. */
  let audioHeld = true;
  let micSlot = false;
  let mic: MicOut | null = null;
  const startedAt = performance.now();

  let framesTotal = 0;
  let bytesTotal = 0;
  let framesInWindow = 0;
  const g2g: number[] = [];
  const fpsSeries: number[] = [];
  const latencySeries: number[] = [];

  const set = (patch: Partial<SessionRuntimeSnapshot>) => {
    if (destroyed) return;
    snapshot = { ...snapshot, ...patch };
    for (const fn of listeners) fn();
  };

  const stop = () => {
    if (timer !== null) clearInterval(timer);
    timer = null;
    document.removeEventListener("visibilitychange", onVisible);
    // Releases held keys through the transport, so it goes first.
    capture?.cleanup();
    capture = null;
    inputSender?.dispose();
    inputSender = null;
    captured = false;
    mic?.stop();
    audioOut?.close();
    transport?.close();
    stage?.close();
    presenter?.destroy();
  };

  // shortcut: a terminal WebTransport failure ends this view with its reason; falling back
  // to WebRTC on its own is #1036, upgrade there.
  const fail = (code: string, patch: Partial<SessionRuntimeSnapshot> = {}) => {
    if (failed || destroyed) return;
    failed = true;
    stop();
    const detail = transportFailureDetail(code);
    // Before the reveal the loader shows the page's failure card; after it, `recovery`
    // `failed` puts the reason on the session's existing recovery banner.
    set({ status: detail, channelOpen: false, inputCaptured: false, pointerLocked: false, recovery: { phase: "failed", attempt: 0, maxAttempts: 0, message: detail }, ...patch });
    cfg.callbacks.onTransportFailed(detail);
  };

  // A hidden tab's decoder falls behind; one keyframe when it shows again resyncs it.
  function onVisible(): void {
    if (document.visibilityState === "visible") transport?.requestKeyframe("loss");
  }

  const tick = () => {
    const g2gMs = median(g2g);
    const snap: TelemetrySnapshot = {
      ...EMPTY_SNAPSHOT,
      fps: framesInWindow * (1000 / TELEMETRY_MS),
      rttMs,
      // Frames are presented as they decode: there is no playout buffer on this path yet.
      playoutTargetMs: 0,
      g2gMs,
      g2g95Ms: percentile(g2g, 95),
      negotiatedCodec,
      framesDecodedTotal: framesTotal,
      bytesReceivedTotal: bytesTotal,
    };
    const audio = audioOut?.stats();
    if (audio?.avOffsetMs != null) {
      // Read by the bench and the hardware pass: how much later than its video the sound is heard.
      cfg.canvas.dataset.avOffsetMs = audio.avOffsetMs.toFixed(1);
      cfg.canvas.dataset.audioUnderrunMs = audio.underrunMs.toFixed(0);
    }
    fpsSeries.push(snap.fps);
    if (g2gMs != null) latencySeries.push(g2gMs);
    framesInWindow = 0;
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
      p = await deps.createPresenter(cfg.canvas, (message) => {
        console.warn("presenter:", message);
        fail("gpu_failed");
      });
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
      fail("no_webgpu", { clientUnsupported: true });
      return;
    }
    presenter = p;
    const t = deps.createTransport({
      sessionId: cfg.sessionId,
      coords: cfg.coords,
      mintToken: () =>
        mintTransportToken(cfg.authToken, cfg.sessionId).then((r) => r.transport_options.webtransport),
    });
    transport = t;
    const s = new DecodeStage({
      ...deps.decoder,
      onFrame: (frame) => {
        // shortcut: assumes the agent's and this machine's clocks agree (true with the
        // browser on the host); the pong's clock offset is #1035.
        const captureUs = frame.timestamp;
        const sample = Date.now() - captureUs / 1000;
        if (!p.present(frame)) return;
        audioOut?.noteVideo(captureUs);
        framesTotal += 1;
        framesInWindow += 1;
        if (sample >= 0 && sample < 5000) {
          g2g.push(sample);
          if (g2g.length > G2G_WINDOW) g2g.shift();
        }
        if (!snapshot.firstFrame) set({ firstFrame: true, status: "streaming over WebTransport" });
      },
      requestKeyframe: (reason) => t.requestKeyframe(reason),
      // decoder_unsupported has its own banner (clientUnsupported); the recovery one would repeat it.
      onFatal: (reason: DecodeFailure) =>
        fail(reason, reason === "decoder_unsupported" ? { clientUnsupported: true, recovery: null } : {}),
    });
    stage = s;
    try {
      await t.connect({
        streams: (streams) => {
          micSlot = streams.mic !== null;
          if (!streams.audio || failed || destroyed) return;
          const out = (audioOut = new AudioOut(deps.audio));
          if (!audioHeld) out.release();
          // Audio must never end the session: a failure to start leaves the stream silent.
          void out.start(streams.audio).catch((err: unknown) => console.warn("audio:", err));
        },
        audio: (packet) => audioOut?.packet(packet),
        config: (c) => {
          negotiatedCodec = CODEC_MIME[c.codec] ?? c.codec;
          // capture.ts scales the mouse by the stream size, which a canvas does not know.
          cfg.canvas.dataset.streamW = String(c.width);
          cfg.canvas.dataset.streamH = String(c.height);
          s.config(c);
        },
        unit: (u) => {
          bytesTotal += u.data.length;
          s.unit(u);
        },
        rtt: (ms) => {
          rttMs = ms;
        },
        end: (e) => {
          if (destroyed || failed) return;
          if (e.code === CLOSE_TAKEN_OVER) {
            takenOver();
            return;
          }
          // The page asks the control plane why; its verdict outranks this one.
          cfg.callbacks.onDisconnectSuspected();
          fail(e.code === CLOSE_ENDED ? "session_ended" : "stream_lost");
        },
      });
    } catch (err) {
      if (destroyed || failed) return;
      const code = err instanceof TransportError ? err.code : "internal";
      if (code === "taken_over") {
        takenOver();
      } else if (code !== "closed") {
        if (code === "session_ended") cfg.callbacks.onDisconnectSuspected();
        fail(code);
      }
      return;
    }
    if (destroyed || failed) return;
    // hello_ack: the control stream is up, and it is what carries input on this transport.
    set({ wsOpen: true, pcConnected: true, channelOpen: true, iceState: "connected" });
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
    timer = setInterval(tick, TELEMETRY_MS);
    document.addEventListener("visibilitychange", onVisible);
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
