// Telemetry for a WebTransport session (#1035): the same stats series, clock row and
// TelemetrySnapshot a WebRTC session produces (webrtc/telemetry.ts), measured where this
// transport has the facts: the display frames the presenter's pictures land in, the bytes and
// frames the runtime handles, and the control stream's ping/pong. There is no RTCPeerConnection to ask.

import { median, percentile } from "../lib/stats";
import { getOrCreateDeviceKey } from "../webrtc/capability";
import { classifyClientHealth } from "../webrtc/clientHealth";
import { estimateClockOffset, shouldPostClock, type ClockSample, type PostedClock } from "../webrtc/clockOffset";
import { summarizePresentWindow } from "../webrtc/presentCadence";
import { addAudioDropouts, EMPTY_SNAPSHOT, newPostedCounters, type PostedCounters, type TelemetrySnapshot } from "../webrtc/telemetry";
import { CLOCK_REPOST_DELTA_MS, CLOCK_REPOST_INTERVAL_S, PRESENT_MIN_SAMPLES } from "../webrtc/thresholds";

/** capture→present samples kept for the median and p95 (webrtc/telemetry.ts keeps 600 too). */
const G2G_WINDOW = 600;
/** Pongs kept for the min-RTT clock estimate, as on WebRTC. */
const CLOCK_WINDOW = 40;
/** Frames whose decode has not come back yet; a decoder that drops some must not grow this. */
const MAX_IN_DECODE = 64;
/** Samples queued between posts, and the most one post carries (control-api stats). A batch
 *  whose post fails is dropped, as on WebRTC: telemetry never retries at the session's cost. */
const MAX_PENDING = 60;
const MAX_BATCH = 64;

export interface WtTelemetryOptions {
  sessionId: string;
  authToken: string;
  /** The launched profile's fps: the per-frame budget client health is judged against. 0: unknown. */
  profileFps?: number;
  fetch?: typeof fetch;
  nowEpochMs?: () => number;
  nowMs?: () => number;
  isHidden?: () => boolean;
  /** Calls back once with the start time (ms, `performance.now()` timeline) of the next display
   *  frame: `requestAnimationFrame`. A hidden tab never calls back. */
  requestFrame?: (cb: (displayMs: number) => void) => void;
  deviceKey?: string;
  /** The page view's posted totals, carried into a WebRTC fallback. */
  counters?: PostedCounters;
}

/**
 * A frame shown later than this after the one before it is a freeze: the WebRTC stats
 * definition (`freezeCount`), so the two transports' `freeze_count` mean the same thing.
 */
export function isFreeze(intervalMs: number, typicalMs: number): boolean {
  return intervalMs > Math.max(3 * typicalMs, typicalMs + 150);
}

export class WtTelemetry {
  private readonly fetch: typeof fetch;
  private readonly nowEpochMs: () => number;
  private readonly nowMs: () => number;
  private readonly isHidden: () => boolean;
  private readonly requestFrame: (cb: (displayMs: number) => void) => void;
  private readonly deviceKey: string;

  /** A picture was submitted and the display frame it lands in has not started: one flag, so a
   *  hidden tab (no frames) holds nothing and several submits in a frame are one displayed frame. */
  private awaitingDisplay = false;
  /** The cadence run: a display callback asked for before a break must not start the next run. */
  private run = 0;
  /** Start of the display frame the last picture landed in. */
  private lastPresentMs: number | null = null;
  private intervals: number[] = [];
  private framesInWindow = 0;
  private framesTotal = 0;
  private bytesInWindow = 0;
  private bytesTotal = 0;
  private lastTickMs: number;
  private keyframeRequests = 0;
  private readonly counters: PostedCounters;
  private lostAtTick: number;
  /** The last window's median interval: what a thin window's freezes are judged against. */
  private typicalMs: number | null = null;
  private decodeMsInWindow: number[] = [];
  private readonly inDecode = new Map<number, number>();
  private readonly g2g: number[] = [];
  private readonly clock: ClockSample[] = [];
  private rttMs: number | null = null;
  private codec: string | null = null;
  private decodeFailed = false;
  private cushionMs = 0;
  private arrivalJitterMs: number | null = null;

  private pending: Record<string, unknown>[] = [];
  private posted: PostedClock = { offsetMs: null, atMs: 0 };
  private clockPostInFlight = false;

  constructor(private readonly opts: WtTelemetryOptions) {
    this.fetch = opts.fetch ?? ((...a) => fetch(...a));
    this.nowEpochMs = opts.nowEpochMs ?? (() => Date.now());
    this.nowMs = opts.nowMs ?? (() => performance.now());
    this.isHidden = opts.isHidden ?? (() => typeof document !== "undefined" && document.visibilityState === "hidden");
    this.requestFrame = opts.requestFrame ?? ((cb) => (typeof requestAnimationFrame === "function" ? void requestAnimationFrame(cb) : cb(this.nowMs())));
    this.deviceKey = opts.deviceKey ?? getOrCreateDeviceKey();
    this.lastTickMs = this.nowMs();
    this.counters = opts.counters ?? newPostedCounters();
    this.lostAtTick = this.counters.packetsLost;
  }

  /** The first window opens here, when the ticks start: connecting is not part of it. */
  startWindow(): void {
    this.lastTickMs = this.nowMs();
  }

  setCodec(mime: string | null): void {
    this.codec = mime;
  }

  setDecodeFailed(failed: boolean): void {
    this.decodeFailed = failed;
  }

  /** The display cushion in force and the arrival jitter behind it (null: none measured, as on PyroWave). */
  setPlayout(cushionMs: number, arrivalJitterMs: number | null): void {
    this.cushionMs = cushionMs;
    this.arrivalJitterMs = arrivalJitterMs;
  }

  /** Audio the stream did not supply since the last call (concealed or silent frames, and runs of it). */
  addAudioDropouts(samples: number, events: number): void {
    addAudioDropouts(this.counters, samples, events);
  }

  /** Media bytes off the wire (access units, after reassembly). */
  bytes(n: number): void {
    this.bytesInWindow += n;
    this.bytesTotal += n;
  }

  /** A `keyframe_request` went on the wire, whoever asked: the decoder, a late frame, a tab shown again. */
  keyframeRequested(): void {
    this.keyframeRequests += 1;
  }

  /** More video fragments found missing. The session's total is posted cumulative, as the
   *  contract and the classifier read `packets_lost`. The caller turns each connection's own
   *  count into additions. */
  addLost(fragments: number): void {
    if (fragments > 0) this.counters.packetsLost += fragments;
  }

  /** A unit handed to the decoder, keyed by its capture time (the frame's timestamp). */
  decodeIn(captureUs: number): void {
    if (this.inDecode.size >= MAX_IN_DECODE) this.inDecode.delete(this.inDecode.keys().next().value as number);
    this.inDecode.set(captureUs, this.nowMs());
  }

  /** Presenting stopped for a reason that is not a freeze (the tab was hidden, the transport
   *  was replaced): the next displayed frame starts a new run of intervals. */
  breakCadence(): void {
    this.lastPresentMs = null;
    this.run += 1;
  }

  /** That unit's frame came out of the decoder. */
  decodeOut(captureUs: number): void {
    const at = this.inDecode.get(captureUs);
    if (at === undefined) return;
    this.inDecode.delete(captureUs);
    this.decodeMsInWindow.push(this.nowMs() - at);
  }

  /**
   * One control-stream pong: its round trip, and the agent's wall clock `tw` (Unix ms) at
   * the instant it answered (transport-webtransport.md §Ping and pong). The offset is
   * host − client, the direction every other consumer uses (clockOffset.ts).
   */
  pong(rttMs: number, agentWallMs?: number): void {
    this.rttMs = rttMs;
    if (typeof agentWallMs !== "number" || !Number.isFinite(agentWallMs)) return;
    this.clock.push({ rtt: rttMs, offset: agentWallMs - (this.nowEpochMs() - rttMs / 2) });
    if (this.clock.length > CLOCK_WINDOW) this.clock.shift();
  }

  /** host − client clock offset (ms); null until the estimate is stable. Never 0 for "unknown". */
  clockOffsetMs(): number | null {
    return estimateClockOffset(this.clock)?.clientOffsetMs ?? null;
  }

  /**
   * A frame was submitted to the presenter. `captureUs` is the agent's wall clock at encoder output; it is
   * compared on the agent's clock, so a sample exists only once the offset is known. The frame and
   * capture-to-present are counted at submission (the contract's meaning); the cadence is stamped at
   * the display frame the picture lands in (`displayed`), as WebRTC's RVFC intervals are.
   */
  presented(captureUs: number): void {
    this.framesInWindow += 1;
    this.framesTotal += 1;
    if (!this.awaitingDisplay) {
      this.awaitingDisplay = true;
      const run = this.run;
      this.requestFrame((displayMs) => this.displayed(displayMs, run));
    }
    const offset = this.clockOffsetMs();
    if (offset === null) return;
    const sample = this.nowEpochMs() + offset - captureUs / 1000;
    if (sample >= 0 && sample < 5000) {
      this.g2g.push(sample);
      if (this.g2g.length > G2G_WINDOW) this.g2g.shift();
    }
  }

  /** A display frame started with a picture submitted before it: the pictures submitted since the
   *  last display frame are one displayed frame (the later ones cover the earlier). */
  private displayed(displayMs: number, run: number): void {
    this.awaitingDisplay = false;
    // Asked for before a break (a hidden tab runs no callback until it is shown; a reconnect): the
    // wait for the first new picture is not an interval.
    if (run !== this.run) return;
    // A throttled background tab can still run a callback: its seconds-long gaps are not stalls.
    if (this.isHidden()) return this.breakCadence();
    if (this.lastPresentMs !== null) this.intervals.push(displayMs - this.lastPresentMs);
    this.lastPresentMs = displayMs;
  }

  /** Closes one window (call once a second): the snapshot for the HUD, and one stats sample queued. */
  tick(): TelemetrySnapshot {
    const now = this.nowMs();
    const dtSec = Math.max(0.001, (now - this.lastTickMs) / 1000);
    this.lastTickMs = now;
    const cadence = summarizePresentWindow(this.intervals, null, this.opts.profileFps || null);
    // A window too thin for a median (the stream just came back from a stall) is judged against
    // the last one, or the profile's frame time: the stall itself must not go uncounted.
    this.typicalMs = cadence.medianMs ?? this.typicalMs;
    const typical = this.typicalMs ?? (this.opts.profileFps ? 1000 / this.opts.profileFps : null);
    const freezeCount = typical === null ? 0 : this.intervals.filter((iv) => isFreeze(iv, typical)).length;
    this.counters.freezeCount += freezeCount;
    const decodeMs = this.decodeMsInWindow.length ? this.decodeMsInWindow.reduce((a, b) => a + b, 0) / this.decodeMsInWindow.length : null;
    const hidden = this.isHidden();
    const health = classifyClientHealth({
      decodeMs,
      presentSdMs: cadence.sdMs,
      presentP95Ms: cadence.p95Ms,
      freezeCount,
      isHidden: hidden,
      profileFrameBudgetMs: this.opts.profileFps ? 1000 / this.opts.profileFps : 0,
      decodeFailed: this.decodeFailed,
    });
    const g2gMs = median(this.g2g);
    const snap: TelemetrySnapshot = {
      ...EMPTY_SNAPSHOT,
      fps: this.framesInWindow / dtSec,
      bitrateKbps: (this.bytesInWindow * 8) / 1000 / dtSec,
      rttMs: this.rttMs,
      decodeMs,
      packetsLost: this.counters.packetsLost - this.lostAtTick,
      freezeCount,
      presentFps: cadence.fpsFromMean,
      presentSdMs: cadence.sdMs,
      presentP95Ms: cadence.p95Ms,
      presentCadence: cadence,
      playoutTargetMs: this.cushionMs,
      g2gMs,
      g2g95Ms: percentile(this.g2g, 95),
      clientHealth: health.health,
      clientHealthReason: health.reason,
      negotiatedCodec: this.codec,
      framesDecodedTotal: this.framesTotal,
      bytesReceivedTotal: this.bytesTotal,
      audioConcealedSamples: this.counters.audioConcealedSamples,
      audioConcealmentEvents: this.counters.audioConcealmentEvents,
    };

    // The keys are the metric manifest's (docs/session-trace/metrics.json): the shared
    // browser keys where this transport measures the same thing, `wt_*` where it does not.
    const m: Record<string, number> = {
      fps: snap.fps,
      bitrate_kbps: Math.round(snap.bitrateKbps ?? 0),
      // Cumulative, as the contract says: the classifier takes its delta across a window.
      freeze_count: this.counters.freezeCount,
      packets_lost: this.counters.packetsLost,
      playout_target_ms: this.cushionMs,
      present_n: cadence.n,
      wt_keyframe_requests: this.keyframeRequests,
      is_hidden: hidden ? 1 : 0,
    };
    const put = (key: string, v: number | null) => {
      if (v !== null) m[key] = v;
    };
    put("rtt_ms", this.rttMs);
    put("decode_ms", decodeMs);
    put("present_fps", cadence.fpsFromMean);
    put("present_interval_sd_ms", cadence.sdMs);
    put("present_interval_p95_ms", cadence.p95Ms);
    put("present_fps_median", cadence.fpsFromMedian);
    put("present_interval_median_ms", cadence.medianMs);
    put("present_interval_max_ms", cadence.maxMs);
    put("present_beat_fraction", cadence.doubledFraction);
    put("present_long_frames", cadence.longFrames);
    put("wt_capture_to_present_ms", g2gMs);
    put("wt_arrival_jitter_ms", this.arrivalJitterMs);
    put("audio_concealed_samples", this.counters.audioConcealedSamples);
    put("audio_concealment_events", this.counters.audioConcealmentEvents);
    // A thin window has no σ: "smooth" there is unmeasured, and posting it would record a pass.
    const unmeasured = health.health === "smooth" && cadence.n < PRESENT_MIN_SAMPLES;
    this.pending.push({
      ts_unix_ms: this.nowEpochMs(),
      metrics: m,
      ...(unmeasured ? {} : { client_health: health.health, client_health_reason: health.reason }),
      device_key: this.deviceKey,
      ...(this.codec ? { codec_mime_type: this.codec } : {}),
    });
    if (this.pending.length > MAX_PENDING) this.pending.splice(0, this.pending.length - MAX_PENDING);

    this.intervals = [];
    this.decodeMsInWindow = [];
    this.framesInWindow = 0;
    this.bytesInWindow = 0;
    this.keyframeRequests = 0;
    this.lostAtTick = this.counters.packetsLost;
    return snap;
  }

  /** Posts the queued samples and, when it is due, the clock row (call every 5 s and at the end). */
  async flush(): Promise<void> {
    await Promise.all([this.postSamples(), this.postClock()]);
  }

  private post(path: string, body: unknown): Promise<Response> {
    return this.fetch(`/v1/sessions/${this.opts.sessionId}/${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${this.opts.authToken}` },
      body: JSON.stringify(body),
    });
  }

  private async postSamples(): Promise<void> {
    if (this.pending.length === 0) return;
    const batch = this.pending.splice(0, MAX_BATCH);
    // Fire and forget: telemetry never affects the session.
    await this.post("stats", { samples: batch }).catch(() => {});
  }

  private async postClock(): Promise<void> {
    if (this.clockPostInFlight) return;
    const est = estimateClockOffset(this.clock);
    if (!est || !shouldPostClock(est, this.posted, this.nowEpochMs(), CLOCK_REPOST_INTERVAL_S, CLOCK_REPOST_DELTA_MS)) return;
    this.clockPostInFlight = true;
    try {
      await this.post("trace/clock", { client_offset_ms: est.clientOffsetMs, uncertainty_ms: est.uncertaintyMs });
      // Only a completed post updates what the server is believed to hold.
      this.posted = { offsetMs: est.clientOffsetMs, atMs: this.nowEpochMs() };
    } catch {
      // The next cadence retries.
    } finally {
      this.clockPostInFlight = false;
    }
  }
}
