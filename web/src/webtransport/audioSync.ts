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
/** Used until a video frame has been presented. */
const DEFAULT_TARGET_MS = 50;

const median = (xs: number[]): number => {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.floor(s.length / 2)];
};

export class AudioSync {
  private readonly floors: { bucket: number; min: number }[] = [];
  private readonly video: number[] = [];

  /** An audio packet arrived at `arrivalMs`: updates the delay floor (arrival − capture). */
  noteAudio(captureUs: number, arrivalMs: number): void {
    const owd = arrivalMs - captureUs / 1000;
    const bucket = Math.floor(arrivalMs / BUCKET_MS);
    const last = this.floors[this.floors.length - 1];
    if (last?.bucket === bucket) last.min = Math.min(last.min, owd);
    else this.floors.push({ bucket, min: owd });
    while (this.floors.length > 0 && this.floors[0].bucket <= bucket - BUCKETS) this.floors.shift();
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

  /** D: how long after capture audio plays. `minMs`: the output device's own latency plus
   *  what it needs to be posted to the audio thread in time. */
  targetDelayMs(minMs = 0): number {
    const wanted = this.videoLatencyMs() ?? DEFAULT_TARGET_MS;
    return Math.min(MAX_TARGET_MS, Math.max(wanted, MIN_TARGET_MS, minMs));
  }

  /** When (on the `now` clock) the packet captured at `captureUs` should reach the speaker;
   *  null before the first audio packet has fixed the floor. */
  playAtMs(captureUs: number, minMs = 0): number | null {
    const floor = this.floor();
    return floor === null ? null : captureUs / 1000 + floor + this.targetDelayMs(minMs);
  }

  /** How much later than its video the audio is heard (positive: audio lags). */
  avOffsetMs(minMs = 0): number | null {
    const v = this.videoLatencyMs();
    return v === null ? null : this.targetDelayMs(minMs) - v;
  }
}
