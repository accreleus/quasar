// The display cushion for WebTransport video: decoded frames are shown at their capture time plus
// the fastest path delay seen plus a cushion that follows measured arrival jitter, instead of the
// moment the decoder returns them. On a clean link the cushion is 0 and a frame is shown on
// arrival, as before. Not used by PyroWave, whose stage presents its own textures.

/** The cushion is this many frame intervals at most: 3 absorbs a late frame or two without
 *  adding the latency a viewer would notice before the stream stutters. */
const MAX_CUSHION_FRAMES = 3;
/** ... and never more than this, whatever the frame rate: 3 intervals at 30 fps is 100 ms. */
const MAX_CUSHION_MS = 50;
/** Cushion = GAIN × jitter − DEADBAND: the smoothed jitter is a mean, a late frame is a few times it. */
const JITTER_GAIN = 3;
/** The first 2 ms of that are ignored, so a clean link stays at 0 (capture-to-present unchanged). */
const DEADBAND_MS = 2;
/** The cushion grows at once and shrinks this much per frame (about 6 ms/s at 120 fps). */
const SHRINK_MS_PER_FRAME = 0.05;
/** Frames waiting at once; a decoder holds few output frames, so more are dropped oldest first. */
const MAX_QUEUED = 6;
/** The fastest-path delay creeps up this much per frame, so clock drift cannot pin it low
 *  (shortcut: a permanent step up in path delay is followed only at this rate). */
const BASE_CREEP_MS = 0.002;
/** A frame due within this is shown now: a timer cannot land closer, and would only add its own jitter. */
const TIMER_SLACK_MS = 1;
/** RFC 3550 jitter smoothing. */
const JITTER_SMOOTHING = 16;

export interface PlayoutOptions {
  /** Shows the frame (and closes it). */
  show(frame: VideoFrame, captureUs: number): void;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (id: unknown) => void;
}

interface Queued {
  frame: VideoFrame;
  captureUs: number;
  dueMs: number;
}

export class Playout {
  private queue: Queued[] = [];
  private jitterMs = 0;
  private cushionMs = 0;
  private baseMs: number | null = null;
  private last: { arrivalMs: number; captureMs: number } | null = null;
  private timer: unknown = null;
  private maxCushionMs = MAX_CUSHION_MS;
  private readonly now: () => number;

  constructor(private readonly opts: PlayoutOptions) {
    this.now = opts.now ?? (() => performance.now());
  }

  /** The stream's frame rate, for the cushion's bound; unknown (0) keeps the absolute bound. */
  setFps(fps: number): void {
    this.maxCushionMs = fps > 0 ? Math.min(MAX_CUSHION_MS, (MAX_CUSHION_FRAMES * 1000) / fps) : MAX_CUSHION_MS;
    this.cushionMs = Math.min(this.cushionMs, this.maxCushionMs);
  }

  /** Smoothed frame arrival jitter (ms): the transit time's change between consecutive frames. */
  arrivalJitterMs(): number {
    return this.jitterMs;
  }

  /** The cushion frames are held for beyond the path's fastest delay (ms). */
  cushion(): number {
    return this.cushionMs;
  }

  /** A decoded frame, captured at `captureUs` on the agent's clock; the playout owns and closes it. */
  push(frame: VideoFrame, captureUs: number): void {
    const arrival = this.now();
    const captureMs = captureUs / 1000;
    this.update(arrival, captureMs);
    // No cushion, nothing waiting: shown on arrival, with no timer in the path (a clean link).
    if (this.cushionMs === 0 && this.queue.length === 0) return this.opts.show(frame, captureUs);
    this.queue.push({ frame, captureUs, dueMs: captureMs + (this.baseMs ?? 0) + this.cushionMs });
    while (this.queue.length > MAX_QUEUED) this.queue.shift()?.frame.close();
    this.pump();
  }

  /** Drops what is waiting and forgets the path: after a reconnect, a hidden tab or a new configuration. */
  reset(): void {
    this.clear();
    this.baseMs = null;
    this.last = null;
    this.jitterMs = 0;
    this.cushionMs = 0;
  }

  close(): void {
    this.clear();
  }

  private clear(): void {
    if (this.timer !== null) (this.opts.clearTimer ?? clearTimeout)(this.timer as never);
    this.timer = null;
    for (const q of this.queue) q.frame.close();
    this.queue = [];
  }

  private update(arrival: number, captureMs: number): void {
    const offset = arrival - captureMs;
    this.baseMs = this.baseMs === null ? offset : Math.min(this.baseMs + BASE_CREEP_MS, offset);
    if (this.last) {
      const d = arrival - this.last.arrivalMs - (captureMs - this.last.captureMs);
      this.jitterMs += (Math.abs(d) - this.jitterMs) / JITTER_SMOOTHING;
    }
    this.last = { arrivalMs: arrival, captureMs };
    const target = Math.min(this.maxCushionMs, Math.max(0, JITTER_GAIN * this.jitterMs - DEADBAND_MS));
    this.cushionMs = target > this.cushionMs ? target : Math.max(target, this.cushionMs - SHRINK_MS_PER_FRAME);
  }

  /** Shows the newest frame that is due (older due ones were superseded), then waits for the next. */
  private pump(): void {
    if (this.timer !== null) (this.opts.clearTimer ?? clearTimeout)(this.timer as never);
    this.timer = null;
    const now = this.now() + TIMER_SLACK_MS;
    let newest = -1;
    for (let i = 0; i < this.queue.length; i++) if (this.queue[i].dueMs <= now) newest = i;
    if (newest >= 0) {
      const due = this.queue.splice(0, newest + 1);
      for (const q of due.slice(0, -1)) q.frame.close();
      const shown = due[due.length - 1];
      this.opts.show(shown.frame, shown.captureUs);
    }
    if (this.queue.length > 0) {
      const wait = Math.max(0, this.queue[0].dueMs - now);
      this.timer = (this.opts.setTimer ?? setTimeout)(() => this.pump(), wait);
    }
  }
}
