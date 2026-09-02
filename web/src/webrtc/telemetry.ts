// Browser-side latency telemetry. Three loops: getStats() at 1Hz (fps, rtt,
// jitter-buffer, decode time, drops, capture-to-display; posted to
// POST /v1/sessions/{id}/stats every 5s), clock-sync ping/pong at 500ms,
// and requestVideoFrameCallback (always-on presentation-pacing σ, #108) — never
// setInterval, RVFC fires per displayed frame.
import { DEFAULT_PLAYOUT_MS, resolvePlayoutMs } from "./playout";
import type { CaptureMetrics } from "../input/capture";
import { measureDisplayRefreshHz, type MeasurementHandle } from "./displayRefreshEstimator";
import { classifyClientHealth, type ClientHealth } from "./clientHealth";
import { getOrCreateDeviceKey } from "./capability";
import type { ClockEstimate, PostedClock } from "./clockOffset";
import { estimateClockOffset, shouldPostClock } from "./clockOffset";
import { CLOCK_REPOST_DELTA_MS, CLOCK_REPOST_INTERVAL_S } from "./thresholds";
import { summarizePresentWindow, type PresentCadence } from "./presentCadence";
import {
  feedPresentedFrame,
  INITIAL_FRAME_DEDUP_STATE,
  type FrameDedupState,
} from "./presentFrameDedup";
import { median, percentile } from "../lib/stats";

const SAMPLE_MAX = 600;

/** Whether RVFC has yielded a validated captureTime sample this session. */
export type RvfcCaptureTimeCapability = "pending" | "available" | "unavailable";
export type AbsCaptureTimeNegotiation = "pending" | "negotiated" | "unavailable";

/** The URI Chrome exposes through RTCRtpReceiver.getParameters(). */
export const ABS_CAPTURE_TIME_URI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-capture-time";

/**
 * Distinct from RVFC's optional captureTime field: negotiated wire timing does
 * not guarantee a browser build surfaces frame-correlated capture timestamps.
 */
export function hasAbsCaptureTimeExtension(
  headerExtensions: ReadonlyArray<{ uri?: string }> | undefined,
): boolean {
  return headerExtensions?.some((extension) => extension.uri === ABS_CAPTURE_TIME_URI) ?? false;
}

export interface RvfcCaptureTimeSampleResult {
  sampleMs: number | null;
}

/** Capture-to-display telemetry, not proof abs-capture-time was negotiated on wire. */
export function rvfcCaptureTimeSample(metadata?: VideoFrameCallbackMetadata): RvfcCaptureTimeSampleResult {
  const captureTime = metadata?.captureTime;
  const expectedDisplayTime = metadata?.expectedDisplayTime;
  if (
    typeof captureTime !== "number" || !Number.isFinite(captureTime) ||
    typeof expectedDisplayTime !== "number" || !Number.isFinite(expectedDisplayTime)
  ) {
    return { sampleMs: null };
  }

  const sampleMs = expectedDisplayTime - captureTime;
  // Reject implausible values rather than mix a fallback timestamp domain in.
  return {
    sampleMs: sampleMs >= 0 && sampleMs < 5000 ? sampleMs : null,
  };
}

/** Stats polls with live inbound RTP but no abs-capture-time on the receiver
 *  before the negotiation is called unavailable. Polls are ~1 Hz, so this is a
 *  few seconds of grace for receiver parameters to populate after SDP. */
const ABS_CAPTURE_TIME_GRACE_POLLS = 3;

const RVFC_CAPTURE_TIME_GRACE_FRAMES = 3;
// RTP can keep flowing while captureTime goes absent (hidden tab, renderer
// stall, detachment). Do not persist old timing.
const RVFC_CAPTURE_TIME_STALE_MS = 5_000;

/** Settle time before a resize/fullscreen change triggers a refresh
 *  re-measurement. Long enough that dragging a window across a monitor edge
 *  measures once, at the destination, rather than continuously en route. */
const DISPLAY_REMEASURE_DEBOUNCE_MS = 400;

export interface TelemetrySnapshot {
  fps: number;
  /** Delta of inbound-rtp bytesReceived over the poll window; null until 2nd poll. */
  bitrateKbps: number | null;
  rttMs: number | null;
  jbMs: number | null;
  decodeMs: number | null;
  packetsLost: number;
  framesDropped: number;
  freezeCount: number;
  /**
   * fps from the MEAN interval, kept for continuity with the stored `present_fps`
   * series — not the number to read: at source fps == display Hz a missed vsync
   * doubles an interval and drags the mean. Read `presentCadence.fpsFromMedian`.
   */
  presentFps: number | null;
  presentSdMs: number | null;
  presentP95Ms: number | null;
  /** The whole distribution this window's scalars were reduced from. */
  presentCadence: PresentCadence;
  playoutTargetMs: number;
  g2gMs: number | null;
  g2g95Ms: number | null;
  rvfcCaptureTimeCapability: RvfcCaptureTimeCapability;
  /** Strict wire-level negotiation, independently receiver-verified. */
  absCaptureTimeNegotiation: AbsCaptureTimeNegotiation;
  encodeMs: number | null;
  networkMs: number | null;
  decodeDisplayMs: number | null;
  inputMetrics: CaptureMetrics | null;
  /** rAF/vsync-measured, independent of the video stream's achieved fps. */
  displayRefreshHz: number | null;
  clientHealth: ClientHealth;
  clientHealthReason: string;
  /** Multi-codec spec §6.1; null until getStats exposes a codec stat (pre-negotiation). */
  negotiatedCodec: string | null;
  /** Cumulative, not per-window — lets callers distinguish "never decoded" from
   * "none this window" without re-deriving getStats parsing. */
  framesDecodedTotal: number;
  bytesReceivedTotal: number;
}

/**
 * One sample posted to POST /v1/sessions/{id}/stats. `metrics` stays a numeric
 * Record (schema.md browser dictionary is numeric, `is_hidden` rides as 0/1);
 * client-health CLASS is a string, so it's a sibling field instead. Server
 * ignores unknown fields (additive only).
 */
interface StatSample {
  ts_unix_ms: number;
  metrics: Record<string, number>;
  client_health?: ClientHealth;
  client_health_reason?: string;
  /** Stable per-device id (localStorage) for profile-certification history. */
  device_key?: string;
  /** Multi-codec spec §6.1. Absent when not yet resolvable (older browsers omit
   * the "codec" stat entirely). */
  codec_mime_type?: string;
}

/** A window with no frames at all — every summary null, no beat claimed. */
const EMPTY_CADENCE: PresentCadence = summarizePresentWindow([], null, null);

const EMPTY_SNAPSHOT: TelemetrySnapshot = {
  fps: 0, bitrateKbps: null, rttMs: null, jbMs: null, decodeMs: null,
  packetsLost: 0, framesDropped: 0, freezeCount: 0,
  presentFps: null, presentSdMs: null, presentP95Ms: null, presentCadence: EMPTY_CADENCE,
  playoutTargetMs: DEFAULT_PLAYOUT_MS,
  g2gMs: null, g2g95Ms: null,
  rvfcCaptureTimeCapability: "pending",
  absCaptureTimeNegotiation: "pending",
  encodeMs: null, networkMs: null, decodeDisplayMs: null,
  inputMetrics: null,
  displayRefreshHz: null,
  clientHealth: "smooth",
  clientHealthReason: "",
  negotiatedCodec: null,
  framesDecodedTotal: 0,
  bytesReceivedTotal: 0,
};

/**
 * Wires up the three measurement loops for a live session. Create, call start()
 * when the video track arrives and the channel is open, register `onUpdate` for
 * the diagnostic panel, call stop() on cleanup.
 */
export class SessionTelemetry {
  private readonly video: HTMLVideoElement;
  private readonly getStatsFn: () => Promise<RTCStatsReport>;
  private readonly channel: RTCDataChannel;
  private readonly sessionId: string;
  private readonly token: string;
  /** With the AS-05 controller, returns its live value so `playout_target_ms`
   * describes the buffer each σ was actually measured under; else the static
   * resolved value (`?playout=` override). See playout.ts. */
  private readonly playoutMsProvider: () => number;

  private fps = 0;
  private bitrateKbps: number | null = null;
  private rttMs: number | null = null;
  private jbMs: number | null = null;
  private decodeMs: number | null = null;
  private packetsLost = 0;
  private framesDropped = 0;
  private freezeCount = 0;

  /** Frame-to-frame *display* intervals from RVFC — #108's headline smoothness
   * metric; getStats reports 0 drops while the display still judders because
   * the renderer doubles/drops frames at vsync. */
  private readonly presentIntervals: number[] = []; // ms, drained each stats poll
  /** #263: tracks the last DISTINCT presented frame (mediaTime), so a vsync
   * tick re-firing for an already-presented frame doesn't pollute the samples. */
  private frameDedupState: FrameDedupState = INITIAL_FRAME_DEDUP_STATE;
  private presentFps: number | null = null;
  private presentSdMs: number | null = null;
  private presentP95Ms: number | null = null;
  private presentCadence: PresentCadence = EMPTY_CADENCE;
  /** rAF/vsync-measured. Re-measured on every event that can change which
   * display (or which compositing rate) the tab is presenting at: tab
   * visibility, fullscreen entry/exit, and window resize — the last two stand
   * in for "moved to another monitor", which the platform does not report.
   * The first measurement is taken during start(), the busiest moment on the
   * main thread, so treating it as permanent is exactly the #85 defect. */
  private displayRefreshHz: number | null = null;
  /** In-flight rAF measurement handle: lets stop() cancel it and the re-measure
   * triggers coalesce concurrent re-measurements to one at a time. */
  private refreshMeasurement: MeasurementHandle | null = null;
  private stopped = false; // guards against late .result resolves writing back
  private readonly onVisibilityChange: () => void;
  private readonly onDisplayChange: () => void;
  /** Debounces the resize trigger: a drag across a monitor edge fires resize
   * continuously, and each measurement runs an rAF loop for up to 2 s. */
  private resizeDebounce: ReturnType<typeof setTimeout> | null = null;

  // getStats() delta tracking (cumulative counters -> per-interval rates)
  private prevJbDelay = 0;
  private prevJbEmitted = 0;
  private prevDecodeTime = 0;
  private prevFramesDecoded = 0;
  private prevBytesReceived = 0;
  private prevBytesTsMs = 0;
  private prevPacketsLost = 0;
  private prevFramesDropped = 0;
  private prevFreezeCount = 0;

  // Clock sync (ping/pong over the input DataChannel)
  private pingSeq = 0;
  private readonly pingSends = new Map<number, number>(); // seq -> Date.now() (epoch ms) at send
  private readonly offsetWindow: Array<{ rtt: number; offset: number }> = [];
  private clockTimer: ReturnType<typeof setInterval> | null = null;
  /**
   * ST-05: posted to POST /v1/sessions/{id}/trace/clock once stable, re-posted
   * while it drifts (clockOffset.ts shouldPostClock). The control plane APPLIES
   * this to put browser points on the host clock, so a stale value silently skews
   * every aligned series for the rest of the session — not cosmetic. Same
   * estimator, same window as `clockOffsetMs()`; no live-vs-latched split. A
   * channel that never carries pongs never posts, leaving the row "unmeasured",
   * never offset 0.
   */
  private postedClock: PostedClock = { offsetMs: null, atMs: 0 };
  private clockPostInFlight = false;

  /** Never mixed with RTP timestamps. */
  private readonly g2gSamples: number[] = [];
  private rvfcCaptureTimeCapability: RvfcCaptureTimeCapability = "pending";
  private rvfcCaptureTimeMisses = 0;
  private lastValidRvfcCaptureTimeAtMs: number | null = null;

  private overlayRunning = false;
  private statsTimer: ReturnType<typeof setInterval> | null = null;
  private postTimer: ReturnType<typeof setInterval> | null = null;

  private readonly pendingSamples: StatSample[] = [];

  private updateListener: ((snap: TelemetrySnapshot) => void) | null = null;

  /** Cached so buildMetrics() can reuse it without a 2nd getter call (which
   * would reset the rate window mid-poll). */
  private lastInputMetrics: CaptureMetrics | null = null;

  private readonly getCaptureMetrics: (() => CaptureMetrics) | null;
  /** Browser-side receiver-parameter proof for the negotiated video RTP stream. */
  private readonly getAbsCaptureTimeNegotiated: (() => boolean) | null;
  private absCaptureTimeNegotiation: AbsCaptureTimeNegotiation = "pending";
  /** Consecutive polls with live inbound RTP but no abs-capture-time extension. */
  private absCaptureTimeMisses = 0;

  /** Launched profile's fps -> per-frame decode/present budget. 0 (unknown)
   * makes the classifier use its absolute fallback ceiling. Stable per session. */
  private readonly profileFps: number;
  /** Stable per-device id (localStorage), attached to each posted sample for
   * profile-certification history keyed by (user, device). */
  private readonly deviceKey: string;
  private clientHealth: ClientHealth = "smooth";
  private clientHealthReason = "";

  /** Sticky: one codec per session per the multi-codec spec's design. */
  private negotiatedCodec: string | null = null;
  /** Raw getStats totals, not deltas — read by getSnapshot() for SessionPage's
   * decode-failure detector. */
  private framesDecodedTotal = 0;
  private bytesReceivedTotal = 0;
  /** Sticky; set externally via setDecodeFailed() once SessionPage sees
   * framesDecoded stuck at 0 while bytesReceived grows for >2s post-arrival. */
  private decodeFailed = false;

  constructor(
    video: HTMLVideoElement,
    getStatsFn: () => Promise<RTCStatsReport>,
    channel: RTCDataChannel,
    sessionId: string,
    token: string,
    playoutMsProvider?: () => number,
    getCaptureMetrics?: () => CaptureMetrics,
    profileFps?: number,
    getAbsCaptureTimeNegotiated?: () => boolean,
  ) {
    this.video = video;
    this.getStatsFn = getStatsFn;
    this.channel = channel;
    this.sessionId = sessionId;
    this.token = token;
    this.playoutMsProvider = playoutMsProvider ?? (() => resolvePlayoutMs());
    this.getCaptureMetrics = getCaptureMetrics ?? null;
    this.profileFps = profileFps ?? 0;
    this.getAbsCaptureTimeNegotiated = getAbsCaptureTimeNegotiated ?? null;
    this.deviceKey = getOrCreateDeviceKey();

    // capture.ts only sends on this channel, so we chain-receive here without conflict.
    const prev = channel.onmessage;
    channel.onmessage = (e) => {
      if (prev) (prev as EventListener)(e);
      this.handleChannelMessage(e.data as string);
    };

    this.onVisibilityChange = () => {
      if (document.visibilityState === "visible") this.remeasureDisplayRefresh();
    };
    // Fullscreen and resize share one debounced handler: entering fullscreen
    // fires BOTH events, and a window dragged across a monitor edge fires
    // resize continuously. Debouncing means one measurement, taken once the
    // new geometry has settled — which is the only geometry worth measuring.
    this.onDisplayChange = () => {
      if (this.resizeDebounce !== null) clearTimeout(this.resizeDebounce);
      this.resizeDebounce = setTimeout(() => {
        this.resizeDebounce = null;
        this.remeasureDisplayRefresh();
      }, DISPLAY_REMEASURE_DEBOUNCE_MS);
    };
  }

  /** Start an rAF refresh measurement unless one is already running. Coalescing
   *  on `refreshMeasurement` is what makes it safe to call from three listeners
   *  that can all fire for a single user action (fullscreen fires resize too).
   *  A null result (throttled tab, cancelled) leaves the previous estimate in
   *  place rather than blanking a number the UI is reading. */
  private remeasureDisplayRefresh(): void {
    if (this.stopped || this.refreshMeasurement !== null) return;
    const handle = measureDisplayRefreshHz();
    this.refreshMeasurement = handle;
    handle.result.then((hz) => {
      this.refreshMeasurement = null;
      if (!this.stopped && hz !== null) this.displayRefreshHz = hz;
    });
  }

  /** Register a callback invoked after each getStats() poll (approximately 1 Hz). */
  onUpdate(listener: (snap: TelemetrySnapshot) => void): void {
    this.updateListener = listener;
  }

  /** Sticky — never cleared here; a hard decode failure doesn't self-heal mid-session. */
  setDecodeFailed(failed: boolean): void {
    this.decodeFailed = failed;
  }

  /** Start all measurement loops. Call after the video track is attached. */
  start(): void {
    this.startStats();
    this.startClockSync();
    this.startOverlayLoop();
    this.startPosting();
    this.remeasureDisplayRefresh(); // true monitor refresh from rAF vsync, not RVFC
    document.addEventListener("visibilitychange", this.onVisibilityChange);
    document.addEventListener("fullscreenchange", this.onDisplayChange);
    window.addEventListener("resize", this.onDisplayChange);
  }

  /** Stop all loops and disconnect. Safe to call multiple times. */
  stop(): void {
    this.stopped = true;
    this.overlayRunning = false;
    if (this.statsTimer !== null) clearInterval(this.statsTimer);
    if (this.clockTimer !== null) clearInterval(this.clockTimer);
    if (this.postTimer !== null) clearInterval(this.postTimer);
    this.statsTimer = null;
    this.clockTimer = null;
    this.postTimer = null;
    // So the .result promise cannot write displayRefreshHz after teardown.
    if (this.refreshMeasurement !== null) {
      this.refreshMeasurement.cancel();
      this.refreshMeasurement = null;
    }
    if (this.resizeDebounce !== null) {
      clearTimeout(this.resizeDebounce);
      this.resizeDebounce = null;
    }
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    document.removeEventListener("fullscreenchange", this.onDisplayChange);
    window.removeEventListener("resize", this.onDisplayChange);
  }

  /**
   * ADD to a browser epoch stamp to express it on the host clock (clockOffset.ts).
   * Same estimator/window as `maybePostClock()`, so bench mode and the control
   * plane never see two different numbers. Null (unmeasured) must never become
   * 0 — bench mode reports `offset_unknown`.
   */
  clockOffsetMs(): number | null {
    return estimateClockOffset(this.offsetWindow)?.clientOffsetMs ?? null;
  }

  /** Current snapshot (latest values; safe to call at any time). */
  getSnapshot(): TelemetrySnapshot {
    const g2g = median(this.g2gSamples);
    const g2g95 = percentile(this.g2gSamples, 95);
    const networkMs = this.rttMs != null ? this.rttMs / 2 : null;

    let decodeDisplayMs: number | null = null;
    if (g2g != null && networkMs != null && this.jbMs != null) {
      // NOT clamped at 0: a negative residual means the estimates disagree
      // (g2g is a rolling median over up to 600 RVFC samples, rtt/jitter-buffer
      // are this 1s window) — clamping would print a confident 0.0ms instead.
      decodeDisplayMs = g2g - networkMs - this.jbMs;
    }

    return {
      fps: this.fps,
      bitrateKbps: this.bitrateKbps,
      rttMs: this.rttMs,
      jbMs: this.jbMs,
      decodeMs: this.decodeMs,
      packetsLost: this.packetsLost,
      framesDropped: this.framesDropped,
      freezeCount: this.freezeCount,
      presentFps: this.presentFps,
      presentSdMs: this.presentSdMs,
      presentP95Ms: this.presentP95Ms,
      presentCadence: this.presentCadence,
      playoutTargetMs: this.playoutMsProvider(),
      g2gMs: g2g,
      g2g95Ms: g2g95,
      rvfcCaptureTimeCapability: this.rvfcCaptureTimeCapability,
      absCaptureTimeNegotiation: this.absCaptureTimeNegotiation,
      encodeMs: null, // host encode ms comes from agent telemetry (P4-05)
      networkMs,
      decodeDisplayMs,
      // Call the getter here so the rate window advances once per poll; cache
      // the result so buildMetrics() can reuse it without a 2nd call.
      inputMetrics: (() => {
        if (!this.getCaptureMetrics) return null;
        this.lastInputMetrics = this.getCaptureMetrics();
        return this.lastInputMetrics;
      })(),
      displayRefreshHz: this.displayRefreshHz,
      clientHealth: this.clientHealth,
      clientHealthReason: this.clientHealthReason,
      negotiatedCodec: this.negotiatedCodec,
      framesDecodedTotal: this.framesDecodedTotal,
      bytesReceivedTotal: this.bytesReceivedTotal,
    };
  }

  // ── getStats() polling (always-on, 1 Hz) ──────────────────────────────────

  private startStats(): void {
    this.statsTimer = setInterval(() => void this.pollStats(), 1000);
  }

  private async pollStats(): Promise<void> {
    let report: RTCStatsReport;
    try {
      report = await this.getStatsFn();
    } catch {
      return;
    }

    // Find video inbound-rtp, the nominated candidate pair, and every "codec"
    // stat keyed by id (multi-codec spec §6.1: inbound-rtp.codecId resolves here).
    // RTCStatsReport values are typed RTCStats but carry optional fields; cast
    // through unknown rather than assume an unsafe index signature.
    type StatMap = Record<string, unknown>;
    let rtp: StatMap | null = null;
    let cp: StatMap | null = null;
    const codecStats = new Map<string, StatMap>();
    for (const s of report.values()) {
      const stat = s as unknown as StatMap;
      if (stat["type"] === "inbound-rtp" && stat["kind"] === "video") rtp = stat;
      else if (stat["type"] === "candidate-pair") {
        if ((stat["nominated"] as boolean | undefined) || (stat["state"] as string | undefined) === "succeeded") {
          if (!cp || (stat["nominated"] as boolean | undefined)) cp = stat;
        }
      } else if (stat["type"] === "codec") {
        const id = stat["id"] as string | undefined;
        if (id) codecStats.set(id, stat);
      }
    }
    if (!rtp) return;

    // Sticky once resolved: one codec per session, a later miss must not blank a prior hit.
    const codecId = rtp["codecId"] as string | undefined;
    if (codecId) {
      const mimeType = codecStats.get(codecId)?.["mimeType"] as string | undefined;
      if (mimeType) this.negotiatedCodec = mimeType;
    }

    this.resolveAbsCaptureTime();

    const rttRaw = cp?.["currentRoundTripTime"] as number | undefined;
    if (rttRaw != null) this.rttMs = rttRaw * 1000;

    // Delta-compute jitter-buffer delay and decode time from cumulative counters.
    const jbDelay = (rtp["jitterBufferDelay"] as number | undefined) ?? 0;
    const jbEmitted = (rtp["jitterBufferEmittedCount"] as number | undefined) ?? 0;
    const dEmit = jbEmitted - this.prevJbEmitted;
    if (dEmit > 0) this.jbMs = ((jbDelay - this.prevJbDelay) / dEmit) * 1000;

    const decodeTime = (rtp["totalDecodeTime"] as number | undefined) ?? 0;
    const framesDecoded = (rtp["framesDecoded"] as number | undefined) ?? 0;
    const dDec = framesDecoded - this.prevFramesDecoded;
    if (dDec > 0) this.decodeMs = ((decodeTime - this.prevDecodeTime) / dDec) * 1000;

    this.prevJbDelay = jbDelay;
    this.prevJbEmitted = jbEmitted;
    this.prevDecodeTime = decodeTime;
    this.prevFramesDecoded = framesDecoded;
    // Cumulative total, not the delta above — decode-failure detection needs
    // "has this receiver EVER decoded a frame", not this window's rate.
    this.framesDecodedTotal = framesDecoded;

    this.fps = (rtp["framesPerSecond"] as number | undefined) ?? 0;

    // Computed here in the 1Hz loop, never per displayed frame, to stay off the
    // RVFC pacing path. First poll seeds the baseline and leaves bitrate null.
    const bytesReceived = (rtp["bytesReceived"] as number | undefined) ?? 0;
    const nowMs = performance.now();
    if (this.prevBytesTsMs > 0) {
      const dtSec = (nowMs - this.prevBytesTsMs) / 1000;
      const dBytes = bytesReceived - this.prevBytesReceived;
      // Guard a counter reset (ICE restart) yielding a negative delta.
      if (dtSec > 0 && dBytes >= 0) {
        this.bitrateKbps = (dBytes * 8) / 1000 / dtSec;
      }
    }
    this.prevBytesReceived = bytesReceived;
    this.prevBytesTsMs = nowMs;
    this.bytesReceivedTotal = bytesReceived;

    this.drainPresentWindow();

    // Cumulative counters -> per-window deltas. packetsLost is a signed int32
    // and can go negative with duplication; clamp at 0.
    const totalLost = (rtp["packetsLost"] as number | undefined) ?? 0;
    const totalDropped = (rtp["framesDropped"] as number | undefined) ?? 0;
    const totalFreezes = (rtp["freezeCount"] as number | undefined) ?? 0;
    this.packetsLost = Math.max(0, totalLost - this.prevPacketsLost);
    this.framesDropped = Math.max(0, totalDropped - this.prevFramesDropped);
    this.freezeCount = Math.max(0, totalFreezes - this.prevFreezeCount);
    this.prevPacketsLost = totalLost;
    this.prevFramesDropped = totalDropped;
    this.prevFreezeCount = totalFreezes;

    // A current RTP stream is not proof RVFC continues to present frames.
    this.expireStaleRvfcCaptureTime();

    // Classify before getSnapshot() so the snapshot carries the current class.
    const isHidden =
      typeof document !== "undefined" && document.visibilityState === "hidden";
    const ch = classifyClientHealth({
      decodeMs: this.decodeMs,
      presentSdMs: this.presentSdMs,
      presentP95Ms: this.presentP95Ms,
      freezeCount: this.freezeCount,
      isHidden,
      profileFrameBudgetMs: this.profileFps > 0 ? 1000 / this.profileFps : 0,
      decodeFailed: this.decodeFailed,
    });
    this.clientHealth = ch.health;
    this.clientHealthReason = ch.reason;

    // Calls the capture getter exactly once, updating lastInputMetrics for buildMetrics().
    const snap = this.getSnapshot();

    const sample: StatSample = {
      ts_unix_ms: Date.now(),
      metrics: { ...this.buildMetrics(), is_hidden: isHidden ? 1 : 0 },
      client_health: ch.health,
      client_health_reason: ch.reason,
      device_key: this.deviceKey,
      ...(this.negotiatedCodec ? { codec_mime_type: this.negotiatedCodec } : {}),
    };
    this.pendingSamples.push(sample);
    if (this.pendingSamples.length > 60) this.pendingSamples.splice(0, this.pendingSamples.length - 60);

    if (this.updateListener) this.updateListener(snap);
  }

  /** Requires >=5 samples to avoid noise from a near-empty window (e.g. a hidden
   * tab, where RVFC stops firing, leaves all three null). */
  private drainPresentWindow(): void {
    const iv = this.presentIntervals;
    // presentFps stays fps-from-the-MEAN so the stored `present_fps` series
    // stays comparable; the distribution itself is presentCadence.ts.
    const cadence = summarizePresentWindow(
      iv,
      this.displayRefreshHz,
      this.profileFps > 0 ? this.profileFps : null,
    );
    this.presentCadence = cadence;
    this.presentFps = cadence.fpsFromMean;
    this.presentSdMs = cadence.sdMs;
    this.presentP95Ms = cadence.p95Ms;
    // displayRefreshHz keeps its last known estimate — blanking it on a brief
    // RVFC under-fill would make the warning flicker.
    iv.length = 0;
    this.frameDedupState = INITIAL_FRAME_DEDUP_STATE;
  }

  private buildMetrics(): Record<string, number> {
    const m: Record<string, number> = { fps: this.fps };
    if (this.bitrateKbps != null) m["bitrate_kbps"] = Math.round(this.bitrateKbps);
    if (this.rttMs != null) m["rtt_ms"] = this.rttMs;
    if (this.jbMs != null) m["jitter_buffer_ms"] = this.jbMs;
    if (this.decodeMs != null) m["decode_ms"] = this.decodeMs;
    m["packets_lost"] = this.packetsLost;
    m["frames_dropped"] = this.framesDropped;
    m["freeze_count"] = this.freezeCount;
    if (this.displayRefreshHz != null) m["display_refresh_hz"] = this.displayRefreshHz;

    // playout_target_ms records the buffer this window was measured under so a
    // stored σ is self-describing; present_* omitted when too few frames (e.g. hidden tab).
    m["playout_target_ms"] = this.playoutMsProvider();
    if (this.presentFps != null) m["present_fps"] = this.presentFps;
    if (this.presentSdMs != null) m["present_interval_sd_ms"] = this.presentSdMs;
    if (this.presentP95Ms != null) m["present_interval_p95_ms"] = this.presentP95Ms;
    // present_fps above stays a MEAN; present_fps_median is the one to read.
    // Additive keys — older control planes drop unknown keys, safe to send anywhere.
    const pc = this.presentCadence;
    m["present_n"] = pc.n;
    if (pc.fpsFromMedian != null) m["present_fps_median"] = pc.fpsFromMedian;
    if (pc.medianMs != null) m["present_interval_median_ms"] = pc.medianMs;
    if (pc.maxMs != null) m["present_interval_max_ms"] = pc.maxMs;
    if (pc.doubledFraction != null) m["present_beat_fraction"] = pc.doubledFraction;
    if (pc.longFrames != null) m["present_long_frames"] = pc.longFrames;

    // Keep both explicit: a negotiated extension cannot make a missing captureTime appear.
    m["rvfc_capture_time_available"] = this.rvfcCaptureTimeCapability === "available" ? 1 : 0;
    m["abs_capture_time_negotiated"] = this.absCaptureTimeNegotiation === "negotiated" ? 1 : 0;
    if (this.g2gSamples.length > 0) {
      const g2g = median(this.g2gSamples);
      if (g2g != null) {
        // `glass_to_glass_ms` overclaims (excludes app render/scan-out, is a
        // median over a never-drained 600-sample ring); `rvfc_capture_to_display_ms`
        // says that. Old key kept so no stored series breaks
        // (docs/session-trace/metrics.json marks it deprecated_for the new one).
        m["glass_to_glass_ms"] = g2g;
        m["rvfc_capture_to_display_ms"] = g2g;
      }
      if (this.rttMs != null) m["network_pacing_ms"] = this.rttMs / 2;
      if (g2g != null && this.rttMs != null && this.jbMs != null) {
        // Unclamped, see getSnapshot(): a negative residual is information, not an error to hide.
        m["decode_display_ms"] = g2g - this.rttMs / 2 - this.jbMs;
      }
    }

    if (this.getCaptureMetrics) {
      const im = this.lastInputMetrics;
      if (im) {
        m["input_msg_per_sec"] = im.inputMsgPerSec;
        m["input_coalesced_per_sec"] = im.coalescedSamplesPerSec;
        m["input_channel_buffered_bytes"] = im.channelBufferedAmount;
        m["input_gamepad_count"] = im.gamepadCount;
        m["input_gamepad_send_per_sec"] = im.gamepadSendPerSec;
        m["input_mm_per_sec"] = im.mmSentPerSec;
        if (im.inputTrace) m["input_trace"] = 1;
        if (im.backpressureDetected) m["input_backpressure"] = 1;
      }
    }

    return m;
  }

  // ── Stats posting (always-on, every 5 s) ──────────────────────────────────

  private startPosting(): void {
    this.postTimer = setInterval(() => {
      void this.flushSamples();
      void this.maybePostClock(); // piggy-backs on the same off-render-path cadence
    }, 5000);
  }

  /** Never posts until stable, so a session that never carries pongs leaves the
   * server clock row absent ("unmeasured" — clockOffset.ts). */
  private async maybePostClock(): Promise<void> {
    // In-flight flag, not a latch: a transient failure costs one cadence, not the session.
    if (this.clockPostInFlight) return;
    const est = estimateClockOffset(this.offsetWindow);
    if (
      !shouldPostClock(
        est,
        this.postedClock,
        Date.now(),
        CLOCK_REPOST_INTERVAL_S,
        CLOCK_REPOST_DELTA_MS,
      )
    ) {
      return;
    }
    const estimate = est as ClockEstimate;
    this.clockPostInFlight = true;
    try {
      await fetch(`/v1/sessions/${this.sessionId}/trace/clock`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.token}`,
        },
        body: JSON.stringify({
          client_offset_ms: estimate.clientOffsetMs,
          uncertainty_ms: estimate.uncertaintyMs,
        }),
      });
      // Only a completed post updates what the server is believed to hold.
      this.postedClock = { offsetMs: estimate.clientOffsetMs, atMs: Date.now() };
    } catch {
      // Fire-and-forget; postedClock untouched, so the next cadence retries.
    } finally {
      this.clockPostInFlight = false;
    }
  }

  private async flushSamples(): Promise<void> {
    if (this.pendingSamples.length === 0) return;
    // Take up to 64 samples, bounded per the P4-01 contract.
    const batch = this.pendingSamples.splice(0, Math.min(this.pendingSamples.length, 64));
    try {
      await fetch(`/v1/sessions/${this.sessionId}/stats`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${this.token}`,
        },
        body: JSON.stringify({ samples: batch }),
      });
    } catch {
      // fire-and-forget; telemetry never affects session state
    }
  }

  // ── Clock sync (ping/pong, 500 ms) ──────────────────────────────────────────

  private startClockSync(): void {
    const tick = () => {
      if (this.channel.readyState !== "open") return;
      this.pingSeq = (this.pingSeq + 1) & 0x7fffffff;
      const seq = this.pingSeq;
      // Epoch ms so the computed offset matches the browser's ts_unix_ms domain
      // (trace-format.md §4: add offset to a Date.now() stamp -> host epoch ms).
      const tc = Date.now();
      this.pingSends.set(seq, tc);
      if (this.pingSends.size > 256) {
        const oldest = this.pingSends.keys().next().value;
        if (oldest !== undefined) this.pingSends.delete(oldest);
      }
      try {
        this.channel.send(JSON.stringify({ t: "ping", id: seq, tc }));
      } catch {
        // channel may have closed
      }
    };
    tick();
    this.clockTimer = setInterval(tick, 500);
  }

  private handleChannelMessage(raw: string): void {
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(raw) as Record<string, unknown>;
    } catch {
      return;
    }
    if (msg["t"] === "pong") {
      const id = msg["id"] as number | undefined;
      const ts = msg["ts"] as number | undefined;
      if (id != null && ts != null) this.onPong(id, ts);
    }
  }

  private onPong(id: number, hostTs: number): void {
    const sendTc = this.pingSends.get(id);
    if (sendTc == null) return;
    // sendTc/now both epoch ms, matching host ts_unix_ms; satisfies the contract
    // offset = hostTs - epochMidpoint -> add to any Date.now() to get host clock
    // (trace-format.md §4). Date.now() resolution (~1ms) is fine at a 500ms
    // ping cadence; the min-RTT filter absorbs the noise.
    const now = Date.now();
    const rtt = now - sendTc;
    const offset = hostTs - (sendTc + now) / 2; // host_epoch ~= client_epoch + offset
    this.offsetWindow.push({ rtt, offset });
    if (this.offsetWindow.length > 40) this.offsetWindow.shift();
  }

  // offsetWindow feeds the ST-05 estimate (maybePostClock -> POST
  // /v1/sessions/{id}/trace/clock). G2G comes only from same-clock RVFC
  // captureTime/expectedDisplayTime; the offset aligns other event series.

  private startOverlayLoop(): void {
    if (this.overlayRunning) return;
    this.overlayRunning = true;
    this.scheduleFrame();
  }

  private scheduleFrame(): void {
    if ("requestVideoFrameCallback" in this.video) {
      this.video.requestVideoFrameCallback((_now, metadata) =>
        this.onDisplayedFrame(metadata),
      );
    } else {
      requestAnimationFrame(() => this.onDisplayedFrame());
    }
  }

  private onDisplayedFrame(metadata?: VideoFrameCallbackMetadata): void {
    if (!this.overlayRunning) return;
    const now = performance.now();

    // Never fall back to RTCRtpSynchronizationSource.timestamp: it's RTP-domain, not NTP.
    const sample = rvfcCaptureTimeSample(metadata);
    this.recordRvfcCaptureTimeSample(sample.sampleMs);

    // pollStats drains this array every second, so at any plausible refresh
    // rate it holds a couple hundred entries at most — no bound needed.
    //
    // #263: dedup by metadata.mediaTime (presentFrameDedup.ts) before
    // accumulating, so intervals reflect distinct presented frames regardless
    // of display Hz. metadata is undefined on the rAF fallback path (no RVFC),
    // where every callback is treated as distinct.
    const { intervalMs, nextState } = feedPresentedFrame(
      this.frameDedupState,
      metadata?.mediaTime,
      now,
    );
    this.frameDedupState = nextState;
    if (intervalMs != null) {
      this.presentIntervals.push(intervalMs);
    }

    this.scheduleFrame();
  }

  // ── Sample helpers ──────────────────────────────────────────────────────────

  private push(arr: number[], v: number): void {
    arr.push(v);
    if (arr.length > SAMPLE_MAX) arr.shift();
  }

  /**
   * Only called after an inbound-rtp report is found, so a negative answer is
   * trustworthy (video is genuinely arriving). Receiver params populate only
   * after SDP negotiation, so `ABS_CAPTURE_TIME_GRACE_POLLS` rides out an early
   * false negative rather than declaring a healthy stream unavailable at 1s.
   * "negotiated" is sticky: one codec/transport per session, a later miss is a
   * stats hiccup, not a renegotiation.
   */
  private resolveAbsCaptureTime(): void {
    if (this.absCaptureTimeNegotiation === "negotiated") return;
    // No probe wired: staying "pending" is honest; "unavailable" would assert a
    // result this instance has no evidence for.
    if (!this.getAbsCaptureTimeNegotiated) return;
    if (this.getAbsCaptureTimeNegotiated()) {
      this.absCaptureTimeNegotiation = "negotiated";
      return;
    }
    this.absCaptureTimeMisses += 1;
    if (this.absCaptureTimeMisses >= ABS_CAPTURE_TIME_GRACE_POLLS) {
      this.absCaptureTimeNegotiation = "unavailable";
    }
  }

  /**
   * Latch available on first valid sample. Before that, tolerate a few callbacks:
   * Chrome can issue early RVFC metadata without captureTime while decoder starts.
   * Only a valid sample refreshes freshness; null/invalid metadata must not preserve
   * old timing. Once unavailable, no series can remain; a valid sample recovers.
   */
  private recordRvfcCaptureTimeSample(sampleMs: number | null): void {
    if (sampleMs != null) {
      this.lastValidRvfcCaptureTimeAtMs = performance.now();
      this.rvfcCaptureTimeCapability = "available";
      this.rvfcCaptureTimeMisses = 0;
      this.push(this.g2gSamples, sampleMs);
      return;
    }
    if (this.rvfcCaptureTimeCapability === "available") return;

    this.rvfcCaptureTimeMisses += 1;
    if (this.rvfcCaptureTimeMisses >= RVFC_CAPTURE_TIME_GRACE_FRAMES) {
      this.rvfcCaptureTimeCapability = "unavailable";
      this.g2gSamples.length = 0;
    }
  }

  /** Clear after valid captureTime goes stale; later valid frames recover. */
  private expireStaleRvfcCaptureTime(nowMs = performance.now()): void {
    if (
      this.rvfcCaptureTimeCapability !== "available" ||
      this.lastValidRvfcCaptureTimeAtMs === null ||
      nowMs - this.lastValidRvfcCaptureTimeAtMs < RVFC_CAPTURE_TIME_STALE_MS
    ) return;

    this.rvfcCaptureTimeCapability = "unavailable";
    this.g2gSamples.length = 0;
  }
}

export { EMPTY_SNAPSHOT };
