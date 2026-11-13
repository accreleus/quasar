// A/V sync for the WebTransport path (protocol/transport-webtransport.md §Audio: `capture_us` on
// both channels is what keeps audio and video in sync). Video is presented as it decodes, so the
// capture→present latency of video is what audio is delayed by: a packet captured at c is played
// at c + D, with D following the median video latency. Neither side needs the other's clock:
// both are mapped through the same one-way-delay floor read off the audio packets.

/** One bucket of the delay floor; the floor is the minimum over the last {@link BUCKETS}. */
const BUCKET_MS = 1_000;
const BUCKETS = 10;
/** Video latency samples behind the median: about two seconds at 60 fps. */
const VIDEO_SAMPLES = 120;
/** Audio is never played sooner than this after capture: it must cover arrival jitter. */
export const MIN_TARGET_MS = 40;
/** A video latency this long is a stall, not a delay to match. */
export const MAX_TARGET_MS = 150;
/** Audio packets behind the jitter estimate: ten seconds at the contract's one 10 ms packet per datagram. */
const JITTER_SAMPLES = 1_000;
/** One packet later than this past the floor is a stall, not jitter to cover: it is lost to
 *  playback either way, and covering it would hold audio that far behind its video for the
 *  whole window. Lateness that keeps recurring is still covered in full. */
export const MAX_JITTER_MS = 40;
/** Packets behind the recurring-lateness percentile: two seconds at 10 ms. */
const RECURRING_SAMPLES = 200;
/** The buffer is deepened by this after the ring ran dry (#1346): the jitter estimate covers
 *  lateness it has seen, and an underrun is lateness it did not cover. */
export const UNDERRUN_STEP_MS = 10;
/** The most underruns can add: past this the link is stalling, not jittering. */
export const MAX_UNDERRUN_MARGIN_MS = 60;
/** Margin given back per second without an underrun: one step lasts 100 s, so a clean link pays nothing. */
const UNDERRUN_DECAY_MS_PER_S = 0.1;
/** Used until a video frame has been presented. */
const DEFAULT_TARGET_MS = 50;
/** Keep the margin's continuous decay from continuously changing the playback rate. */
const TARGET_DEADBAND_MS = 5;
const TARGET_FALL_HOLD_MS = 5_000;

const median = (xs: number[]): number => {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.floor(s.length / 2)];
};

export class AudioSync {
  private readonly floors: { bucket: number; min: number }[] = [];
  private readonly video: number[] = [];
  /** How far past the floor recent packets arrived. */
  private readonly excess: number[] = [];
  /** The underrun margin as it stood at `marginAt`; it decays from there. */
  private margin = 0;
  private marginAt = 0;
  private audioAt = 0;
  private scheduledTarget: number | null = null;
  private lowerSince: number | null = null;

  /** The ring ran dry and played silence: deepen the buffer by one step. */
  noteUnderrun(nowMs: number): void {
    this.margin = Math.min(MAX_UNDERRUN_MARGIN_MS, this.underrunMarginMs(nowMs) + UNDERRUN_STEP_MS);
    this.marginAt = nowMs;
  }

  /** What underruns add to the playout delay now: a step each, given back slowly. Kept across
   *  {@link reset}: a new delay floor does not make the link any steadier. */
  underrunMarginMs(nowMs: number): number {
    return Math.max(0, this.margin - (Math.max(0, nowMs - this.marginAt) / 1_000) * UNDERRUN_DECAY_MS_PER_S);
  }

  /** An audio packet arrived at `arrivalMs`: updates the delay floor (arrival − capture). */
  noteAudio(captureUs: number, arrivalMs: number): void {
    this.audioAt = arrivalMs;
    const owd = arrivalMs - captureUs / 1000;
    const bucket = Math.floor(arrivalMs / BUCKET_MS);
    // Before the lateness is read: a floor older than the window (a frozen tab, a long silence)
    // says nothing about this packet.
    while (this.floors.length > 0 && this.floors[0].bucket <= bucket - BUCKETS) this.floors.shift();
    const before = this.floor();
    const last = this.floors[this.floors.length - 1];
    if (last?.bucket === bucket) last.min = Math.min(last.min, owd);
    else this.floors.push({ bucket, min: owd });
    this.excess.push(Math.max(0, owd - (before ?? owd)));
    if (this.excess.length > JITTER_SAMPLES) this.excess.shift();
  }

  /** Forget the delay floor and what was measured against it. After a step in the path's delay
   *  (a handover, a reroute) the old floor is wrong for up to its whole window; the next
   *  packets fix a new one. */
  reset(): void {
    this.floors.length = 0;
    this.video.length = 0;
    this.excess.length = 0;
    this.scheduledTarget = null;
    this.lowerSince = null;
  }

  /** What the playout delay must cover for recent packets to arrive in time: the furthest any
   *  ran past the floor in the window, up to {@link MAX_JITTER_MS}, and without that bound the
   *  95th percentile of the last two seconds (a link that bunches one packet in five). The
   *  percentile alone leaves five packets a second uncovered at 100 a second (#1346). */
  jitterMs(): number {
    if (this.excess.length < 20) return 0;
    const furthest = this.excess.reduce((a, b) => Math.max(a, b), 0);
    const s = this.excess.slice(-RECURRING_SAMPLES).sort((a, b) => a - b);
    return Math.max(Math.min(MAX_JITTER_MS, furthest), s[Math.min(s.length - 1, Math.floor(s.length * 0.95))]);
  }

  /** A video frame captured at `captureUs` was presented at `presentMs`. */
  noteVideo(captureUs: number, presentMs: number): void {
    const floor = this.floor();
    if (floor === null) return;
    const latency = presentMs - (captureUs / 1000 + floor);
    if (latency < 0 || latency > 5_000) return;
    this.video.push(latency);
    if (this.video.length > VIDEO_SAMPLES) this.video.shift();
  }

  private floor(): number | null {
    return this.floors.length === 0 ? null : Math.min(...this.floors.map((f) => f.min));
  }

  /** The median capture→present latency of video, null before any frame. */
  videoLatencyMs(): number | null {
    return this.video.length === 0 ? null : median(this.video);
  }

  /** The last scheduled delay, so telemetry cannot move a target no chunk has been given.
   *  Before the first chunk, returns the computed target. */
  targetDelayMs(minMs = 0): number {
    return this.scheduledTarget ?? this.computedTargetMs(minMs);
  }

  /** Device and posting floor plus jitter, bounded by the video match and the delay cap. */
  private computedTargetMs(minMs: number): number {
    const wanted = this.videoLatencyMs() ?? DEFAULT_TARGET_MS;
    return Math.min(MAX_TARGET_MS, Math.max(wanted, MIN_TARGET_MS, minMs + this.jitterMs()));
  }

  /** When (on the `now` clock) the packet captured at `captureUs` should reach the speaker;
   *  null before the first audio packet has fixed the floor. */
  playAtMs(captureUs: number, minMs = 0): number | null {
    const floor = this.floor();
    if (floor === null) return null;
    const computed = this.computedTargetMs(minMs);
    const scheduled = this.scheduledTarget;
    if (scheduled === null || computed > scheduled + TARGET_DEADBAND_MS) {
      this.scheduledTarget = computed;
      this.lowerSince = null;
    } else if (computed < scheduled - TARGET_DEADBAND_MS) {
      this.lowerSince ??= this.audioAt;
      if (this.audioAt - this.lowerSince >= TARGET_FALL_HOLD_MS) {
        this.scheduledTarget = computed;
        this.lowerSince = null;
      }
    } else {
      this.lowerSince = null;
    }
    return captureUs / 1000 + floor + this.targetDelayMs(minMs);
  }

  /** How much later than its video the audio is heard (positive: audio lags). */
  avOffsetMs(minMs = 0): number | null {
    const v = this.videoLatencyMs();
    return v === null ? null : this.targetDelayMs(minMs) - v;
  }
}
