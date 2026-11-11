// The audio worklet's queue: decoded chunks, each with the render frame it should start at
// (audioSync decides it from capture time), played out into the audio thread's 128-frame
// quanta. Lives apart from the processor so it can be tested. The processor file imports it.

interface Chunk {
  /** Render frame (the AudioWorkletGlobalScope's `currentFrame` timeline) of its first sample. */
  start: number;
  planes: Float32Array[];
  frames: number;
  /** Frames already played or skipped from its head. */
  off: number;
  /** A fade standing in for a lost packet. */
  concealed: boolean;
}

export interface RingStats {
  /** Frames of silence played while the queue was empty after audio had started. */
  underrunFrames: number;
  /** Frames played from a concealment chunk (a fade standing in for a lost packet). */
  concealedFrames: number;
  /** Dropouts: a queue that ran dry, and the concealment fade, count once per gap however long it lasts. */
  underrunEvents: number;
  /** Chunks dropped because they were due before they arrived, or the queue was full. */
  dropped: number;
}

/** Chunks within this many frames of the last one's end are played back to back, so a
 *  millisecond of timestamp jitter makes no click (5 ms at 48 kHz). */
export const SLACK_FRAMES = 240;
/** The most audio queued (one second at 48 kHz). */
export const MAX_QUEUED_FRAMES = 48_000;

export class AudioRing {
  private queue: Chunk[] = [];
  private queued = 0;
  /** Frame after the last sample played; null before the first chunk. */
  private lastEnd: number | null = null;
  readonly stats: RingStats = { underrunFrames: 0, concealedFrames: 0, underrunEvents: 0, dropped: 0 };
  /** The last frame rendered was silence for want of audio. */
  private starving = false;

  constructor(
    private readonly slack = SLACK_FRAMES,
    private readonly maxQueued = MAX_QUEUED_FRAMES,
  ) {}

  /** Queue a chunk to start at render frame `start`; `now` is the render frame at this moment. */
  push(start: number, planes: Float32Array[], now: number, concealed = false): void {
    const frames = planes[0]?.length ?? 0;
    if (frames === 0) return;
    if (start + frames <= now || this.queued + frames > this.maxQueued) {
      this.stats.dropped += 1;
      return;
    }
    let at = this.queue.length;
    while (at > 0 && this.queue[at - 1].start > start) at--;
    this.queue.splice(at, 0, { start, planes, frames, off: 0, concealed });
    this.queued += frames;
  }

  /** Fill `out` (one array per channel, all the same length) for render frames from `frame`. */
  render(out: Float32Array[], frame: number): void {
    const n = out[0].length;
    let pos = 0;
    while (pos < n) {
      const chunk = this.queue[0];
      const f = frame + pos;
      if (!chunk) {
        this.silence(out, pos, n - pos);
        this.underrun(n - pos);
        return;
      }
      // Back to back with the last chunk when its timestamp is within the slack; an overlap
      // (a duplicate, or a late reorder) loses its head instead of playing twice.
      if (this.lastEnd !== null && chunk.start + chunk.off < this.lastEnd + this.slack) {
        if (chunk.start + chunk.off >= this.lastEnd) chunk.start = this.lastEnd - chunk.off;
        else chunk.off = this.lastEnd - chunk.start;
      }
      const at = chunk.start + chunk.off;
      if (at > f) {
        const gap = Math.min(at - f, n - pos);
        this.silence(out, pos, gap);
        this.underrun(gap);
        pos += gap;
        continue;
      }
      // Due before it could be played: its head is skipped, all of it if it is wholly past.
      if (at < f) chunk.off += f - at;
      if (chunk.off >= chunk.frames) {
        this.queue.shift();
        this.queued -= chunk.frames;
        this.stats.dropped += 1;
        continue;
      }
      const take = Math.min(chunk.frames - chunk.off, n - pos);
      for (let c = 0; c < out.length; c++) {
        out[c].set(chunk.planes[Math.min(c, chunk.planes.length - 1)].subarray(chunk.off, chunk.off + take), pos);
      }
      chunk.off += take;
      pos += take;
      if (chunk.concealed) {
        // The fade opens the gap's one event unless silence already did; the silence after it joins it.
        this.stats.concealedFrames += take;
        if (!this.starving) this.stats.underrunEvents += 1;
        this.starving = true;
      } else {
        this.starving = false;
      }
      this.lastEnd = chunk.start + chunk.off;
      if (chunk.off >= chunk.frames) {
        this.queue.shift();
        this.queued -= chunk.frames;
      }
    }
  }

  private underrun(frames: number): void {
    if (this.lastEnd === null) return;
    this.stats.underrunFrames += frames;
    if (!this.starving) this.stats.underrunEvents += 1;
    this.starving = true;
  }

  private silence(out: Float32Array[], from: number, count: number): void {
    for (const channel of out) channel.fill(0, from, from + count);
  }
}
