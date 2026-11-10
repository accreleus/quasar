// Telemetry for a WebTransport session (#1035): the same stats series, clock row and
// TelemetrySnapshot a WebRTC session produces (webrtc/telemetry.ts), measured where this
// transport has the facts: the presenter's own present times, the bytes and frames the
// runtime handles, and the control stream's ping/pong. There is no RTCPeerConnection to ask.

import { median, percentile } from "../lib/stats";
import { getOrCreateDeviceKey } from "../webrtc/capability";
import { classifyClientHealth } from "../webrtc/clientHealth";
import { estimateClockOffset, shouldPostClock, type ClockSample, type PostedClock } from "../webrtc/clockOffset";
import { summarizePresentWindow } from "../webrtc/presentCadence";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../webrtc/telemetry";
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
  deviceKey?: string;
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
  private readonly deviceKey: string;

  private lastPresentMs: number | null = null;
  private intervals: number[] = [];
  private framesInWindow = 0;
  private framesTotal = 0;
  private bytesInWindow = 0;
  private bytesTotal = 0;
  private lastTickMs: number;
  private keyframeRequests = 0;
  private lostTotal = 0;
  private lostAtTick = 0;
  private lostSeen = 0;
  private lostBase = 0;
  /** The last window's median interval: what a thin window's freezes are judged against. */
  private typicalMs: number | null = null;
  private decodeMsInWindow: number[] = [];
  private readonly inDecode = new Map<number, number>();
  private readonly g2g: number[] = [];
  private readonly clock: ClockSample[] = [];
  private rttMs: number | null = null;
  private codec: string | null = null;
  private decodeFailed = false;

  private pending: Record<string, unknown>[] = [];
  private posted: PostedClock = { offsetMs: null, atMs: 0 };
  private clockPostInFlight = false;

  constructor(private readonly opts: WtTelemetryOptions) {
    this.fetch = opts.fetch ?? ((...a) => fetch(...a));
    this.nowEpochMs = opts.nowEpochMs ?? (() => Date.now());
    this.nowMs = opts.nowMs ?? (() => performance.now());
    this.isHidden = opts.isHidden ?? (() => typeof document !== "undefined" && document.visibilityState === "hidden");
    this.deviceKey = opts.deviceKey ?? getOrCreateDeviceKey();
    this.lastTickMs = this.nowMs();
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

  /** Media bytes off the wire (access units, after reassembly). */
  bytes(n: number): void {
    this.bytesInWindow += n;
    this.bytesTotal += n;
  }

  /** A `keyframe_request` went on the wire, whoever asked: the decoder, a late frame, a tab shown again. */
  keyframeRequested(): void {
    this.keyframeRequests += 1;
  }

  /** Video fragments found missing since connect (cumulative, as `packets_lost` is on WebRTC). */
  lost(total: number): void {
    // A reconnect starts a new count at 0: the session's total carries on.
    if (total < this.lostSeen) this.lostBase += this.lostSeen;
    this.lostSeen = total;
    this.lostTotal = this.lostBase + total;
  }

  /** A unit handed to the decoder, keyed by its capture time (the frame's timestamp). */
  decodeIn(captureUs: number): void {
    if (this.inDecode.size >= MAX_IN_DECODE) this.inDecode.delete(this.inDecode.keys().next().value as number);
    this.inDecode.set(captureUs, this.nowMs());
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
   * A frame reached the presenter. `captureUs` is the agent's wall clock at encoder output; it is compared on
   * the agent's clock, so a sample exists only once the offset is known.
   */
  presented(captureUs: number): void {
    const now = this.nowMs();
    if (this.lastPresentMs !== null) this.intervals.push(now - this.lastPresentMs);
    this.lastPresentMs = now;
    this.framesInWindow += 1;
    this.framesTotal += 1;
    const offset = this.clockOffsetMs();
    if (offset === null) return;
    const sample = this.nowEpochMs() + offset - captureUs / 1000;
    if (sample >= 0 && sample < 5000) {
      this.g2g.push(sample);
      if (this.g2g.length > G2G_WINDOW) this.g2g.shift();
    }
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
      packetsLost: this.lostTotal - this.lostAtTick,
      freezeCount,
      presentFps: cadence.fpsFromMean,
      presentSdMs: cadence.sdMs,
      presentP95Ms: cadence.p95Ms,
      presentCadence: cadence,
      // Frames are presented as they decode: there is no playout buffer on this path.
      playoutTargetMs: 0,
      g2gMs,
      g2g95Ms: percentile(this.g2g, 95),
      clientHealth: health.health,
      clientHealthReason: health.reason,
      negotiatedCodec: this.codec,
      framesDecodedTotal: this.framesTotal,
      bytesReceivedTotal: this.bytesTotal,
    };

    // The keys are the metric manifest's (docs/session-trace/metrics.json): the shared
    // browser keys where this transport measures the same thing, `wt_*` where it does not.
    const m: Record<string, number> = {
      fps: snap.fps,
      bitrate_kbps: Math.round(snap.bitrateKbps ?? 0),
      freeze_count: freezeCount,
      // Cumulative, as on WebRTC: the classifier takes its delta across a window.
      packets_lost: this.lostTotal,
      playout_target_ms: 0,
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
    this.lostAtTick = this.lostTotal;
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
