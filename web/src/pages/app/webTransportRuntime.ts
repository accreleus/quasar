// The session runtime for a WebTransport session (#1031, video only):
// transport -> DecodeStage -> presenter, behind the same SessionRuntime surface
// the page drives for WebRTC. Input, audio and the mic are not carried yet
// (#1032, #1033): those methods are inert.

import { mintTransportToken } from "../../api/library";
import type { WebTransportCoords } from "../../api/types";
import { median, percentile } from "../../lib/stats";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../webrtc/telemetry";
import { DecodeStage, type DecodeStageOptions } from "../../webtransport/decodeStage";
import { createWebGpuPresenter, type Presenter } from "../../webtransport/presenter";
import { TransportError, type MediaTransport } from "../../webtransport/transport";
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

export interface WebTransportRuntimeDeps {
  createTransport(opts: { sessionId: string; coords: WebTransportCoords; mintToken(): Promise<WebTransportCoords> }): MediaTransport;
  createPresenter(canvas: HTMLCanvasElement): Promise<Presenter | null>;
  decoder?: Pick<DecodeStageOptions, "createDecoder" | "isConfigSupported">;
}

export interface WebTransportRuntimeConfig {
  sessionId: string;
  authToken: string;
  coords: WebTransportCoords;
  canvas: HTMLCanvasElement;
  callbacks: Pick<SessionRuntimeCallbacks, "onDisconnectSuspected" | "onSessionTakenOver">;
  deps?: Partial<WebTransportRuntimeDeps>;
}

const productionDeps: WebTransportRuntimeDeps = {
  createTransport: (o) => new WebTransportMedia(o),
  createPresenter: createWebGpuPresenter,
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
  let transport: MediaTransport | null = null;
  let presenter: Presenter | null = null;
  let stage: DecodeStage | null = null;
  let timer: ReturnType<typeof setInterval> | null = null;
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

  const tick = () => {
    const g2gMs = median(g2g);
    const snap: TelemetrySnapshot = {
      ...EMPTY_SNAPSHOT,
      fps: framesInWindow * (1000 / TELEMETRY_MS),
      g2gMs,
      g2g95Ms: percentile(g2g, 95),
      negotiatedCodec: "video/H264",
      framesDecodedTotal: framesTotal,
      bytesReceivedTotal: bytesTotal,
    };
    fpsSeries.push(snap.fps);
    if (g2gMs != null) latencySeries.push(g2gMs);
    framesInWindow = 0;
    for (const fn of telemetrySubs) fn(snap);
  };

  const fail = (status: string, patch: Partial<SessionRuntimeSnapshot> = {}) => set({ status, ...patch });

  async function run(): Promise<void> {
    const p = await deps.createPresenter(cfg.canvas);
    if (destroyed) {
      p?.destroy();
      return;
    }
    if (!p) {
      fail("WebTransport needs WebGPU, which this browser does not offer", { clientUnsupported: true });
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
    stage = new DecodeStage({
      ...deps.decoder,
      onFrame: (frame) => {
        if (destroyed || !presenter) {
          frame.close();
          return;
        }
        // shortcut: assumes the agent's and this machine's clocks agree (true with the
        // browser on the host); the ping/pong offset arrives with #1032 and #1035.
        const sample = Date.now() - frame.timestamp / 1000;
        presenter.present(frame);
        framesTotal += 1;
        framesInWindow += 1;
        if (sample >= 0 && sample < 5000) {
          g2g.push(sample);
          if (g2g.length > G2G_WINDOW) g2g.shift();
        }
        if (!snapshot.firstFrame) set({ firstFrame: true, status: "streaming over WebTransport" });
      },
      requestKeyframe: (reason) => t.requestKeyframe(reason),
      onUnsupported: (c) => fail(`this browser cannot decode ${c.codec} here`, { clientUnsupported: true }),
    });
    const s = stage;
    try {
      await t.connect({
        config: (c) => s.config(c),
        unit: (u) => {
          bytesTotal += u.data.length;
          s.unit(u);
        },
        end: (e) => {
          if (destroyed) return;
          if (e.code === CLOSE_TAKEN_OVER) cfg.callbacks.onSessionTakenOver();
          else cfg.callbacks.onDisconnectSuspected();
          fail(e.code === CLOSE_ENDED ? "session ended" : "WebTransport connection lost", { channelOpen: false });
        },
      });
    } catch (err) {
      if (destroyed) return;
      fail(`WebTransport failed: ${err instanceof TransportError ? err.code : "internal"}`);
      return;
    }
    if (destroyed) return;
    // hello_ack: the control stream is up, and it is what carries input on this transport.
    set({ wsOpen: true, pcConnected: true, channelOpen: true, iceState: "connected" });
    timer = setInterval(tick, TELEMETRY_MS);
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
      if (timer !== null) clearInterval(timer);
      transport?.close();
      stage?.close();
      presenter?.destroy();
      listeners.clear();
      telemetrySubs.clear();
    },
    engage: () => null,
    release() {},
    syncKeyboardLock() {},
    setMouse() {},
    isCaptured: () => false,
    cancelRecovery() {},
    hasMicSlot: () => false,
    attachMicTrack: () => Promise.resolve(),
    detachMicTrack: () => Promise.resolve(),
    registerTelemetry(fn) {
      telemetrySubs.add(fn);
      return () => telemetrySubs.delete(fn);
    },
    summary: (): SessionSummaryAccumulator => ({
      elapsedMs: performance.now() - startedAt,
      fps: fpsSeries,
      latency: latencySeries,
    }),
    releaseAudio() {},
    holdAudio() {},
  };
}
