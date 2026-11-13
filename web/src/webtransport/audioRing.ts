// The audio worklet's queue: decoded chunks, each with the render frame it should start at
// (audioSync decides it from capture time), played out into the audio thread's 128-frame
// quanta. Lives apart from the processor so it can be tested. The processor file imports it.

interface Chunk {
  /** Render frame (the AudioWorkletGlobalScope's `currentFrame` timeline) it plays at; moved to
   *  the last chunk's end while a change in the playout delay is being closed. */
  start: number;
  /** The frame the stream scheduled it at: what its own timeline's steps are measured against. */
  nominal: number;
  /** The scheduler's target, so a delay step can be separated from missing packets. */
  targetDelayFrames?: number;
  planes: Float32Array[];
  frames: number;
  /** Source frames already played or skipped from its head; fractional while the rate is off. */
  off: number;
  /** Output frames already played from it: what places it on the render timeline, where `off`
   *  does not while its samples are played at an off-real-time rate. */
  out: number;
  /** A fade standing in for a lost packet. */
  concealed: boolean;
  /** Its start and rate are decided; a chunk spans quanta and must be aligned only once. */
  settled: boolean;
}

export interface RingStats {
  /** Frames of silence played while the queue was empty after audio had started. */
  underrunFrames: number;
  /** Frames played from a concealment chunk (a fade standing in for a lost packet). */
  concealedFrames: number;
  /** Dropouts: a queue that ran dry, and the concealment fade, count once per gap however long it lasts. */
  underrunEvents: number;
  /** The part of `underrunEvents` that was the queue running dry and silence playing: the rest
   *  opened with a fade, so `underrunEvents - silenceRuns` is the concealed packets (#1346). */
  silenceRuns: number;
  /** Chunks dropped because they were due before they arrived, or the queue was full. */
  dropped: number;
  /** The least audio queued at a quantum since the last {@link AudioRing.takeStats}, frames;
   *  `Infinity` before any audio has played. 0 exactly when silence was played in the window. */
  minQueuedFrames: number;
}

/** Chunks within this many frames of the last one's end are played back to back, so a
 *  millisecond of timestamp jitter makes no click (5 ms at 48 kHz). */
export const SLACK_FRAMES = 240;
/** The most audio queued (one second at 48 kHz). */
export const MAX_QUEUED_FRAMES = 48_000;
/** A step in the playout delay up to this size is closed by playing slightly off real time
 *  (60 ms at 48 kHz). A larger one is a stall or a reset, not a delay to follow: it keeps the
 *  old behaviour, the whole gap as silence or the whole overlap cut. */
export const MAX_STRETCH_FRAMES = 2_880;
/** How far off real time the queue plays while it closes such a step: 2 %. Over the second or
 *  two it takes, that shifts pitch by a third of a semitone, not a click — the accepted trade
 *  here, where a resampler is all this needs and a phase vocoder or WSOLA is not. */
export const STRETCH_RATE = 0.02;
/** A correction under way runs until the step is this small (1 ms at 48 kHz), not until it is
 *  inside the slack: stopping there would leave the queue 5 ms ahead of the delay it was given,
 *  which is 5 ms of the jitter cover gone. */
export const STRETCH_DONE_FRAMES = 48;

export class AudioRing {
  private queue: Chunk[] = [];
  private queued = 0;
  /** Frame after the last sample played; null before the first chunk. */
  private lastEnd: number | null = null;
  readonly stats: RingStats = {
    underrunFrames: 0,
    concealedFrames: 0,
    underrunEvents: 0,
    silenceRuns: 0,
    dropped: 0,
    minQueuedFrames: Infinity,
  };
  /** The last frame rendered was silence for want of audio. */
  private starving = false;
  /** Source frames played per output frame: 1, or 2 % off while a delay change is closed. */
  private rate = 1;
  /** The stream's own start of the last chunk finished, and its length: what a step in the
   *  stream's timeline, rather than in ours, is measured against. */
  private prevStart: number | null = null;
  private prevFrames = 0;
  private prevTargetDelayFrames: number | undefined;
  /** The last chunk finished was a concealment fade: audio is missing, so the next gap is not
   *  a delay change. */
  private prevConcealed = false;

  constructor(
    private readonly slack = SLACK_FRAMES,
    private readonly maxQueued = MAX_QUEUED_FRAMES,
  ) {}

  /** Queue a chunk to start at render frame `start`; `now` is the render frame at this moment. */
  push(start: number, planes: Float32Array[], now: number, concealed = false, targetDelayFrames?: number): void {
    const frames = planes[0]?.length ?? 0;
    if (frames === 0) return;
    if (start + frames <= now || this.queued + frames > this.maxQueued) {
      this.stats.dropped += 1;
      return;
    }
    // By start time only around a concealment fade, which is posted ahead of real chunks still
    // in the decoder. Real chunks arrive in stream order and keep it: a fall in the playout
    // delay of a chunk or more makes a later chunk's start the earlier one, and sorting on it
    // would play them out of order.
    let at = this.queue.length;
    while (at > 0 && this.queue[at - 1].start > start && (concealed || this.queue[at - 1].concealed)) at--;
    this.queue.splice(at, 0, { start, nominal: start, targetDelayFrames, planes, frames, off: 0, out: 0, concealed, settled: false });
    this.queued += frames;
  }

  /** The counts for the window just closed. The buffer minimum is per window, so it starts
   *  again: the caller reports it, then this ring measures the next one. */
  takeStats(): RingStats {
    const stats = { ...this.stats };
    this.stats.minQueuedFrames = Infinity;
    return stats;
  }

  /** Fill `out` (one array per channel, all the same length) for render frames from `frame`. */
  render(out: Float32Array[], frame: number): void {
    const n = out[0].length;
    this.noteQueued();
    let pos = 0;
    while (pos < n) {
      const chunk = this.queue[0];
      const f = frame + pos;
      if (!chunk) {
        this.silence(out, pos, n - pos);
        this.underrun(n - pos);
        return;
      }
      if (!chunk.settled) {
        chunk.settled = true;
        this.align(chunk, f);
      }
      const at = chunk.start + chunk.out;
      if (at > f) {
        const gap = Math.min(at - f, n - pos);
        this.silence(out, pos, gap);
        this.underrun(gap);
        pos += gap;
        continue;
      }
      // Due before it could be played: its head is skipped, all of it if it is wholly past.
      if (at < f) {
        chunk.out += f - at;
        chunk.off += (f - at) * this.rate;
      }
      if (chunk.off >= chunk.frames) {
        this.finish(chunk, true);
        continue;
      }
      const written =
        this.rate === 1 ? this.copy(chunk, out, pos, n - pos) : this.resample(chunk, out, pos, n - pos);
      pos += written;
      chunk.out += written;
      if (chunk.concealed) {
        // The fade opens the gap's one event unless silence already did; the silence after it joins it.
        this.stats.concealedFrames += written;
        if (!this.starving) this.stats.underrunEvents += 1;
        this.starving = true;
      } else {
        this.starving = false;
      }
      // Where the next chunk must start to be heard back to back.
      this.lastEnd = frame + pos;
      if (chunk.off >= chunk.frames) this.finish(chunk, false);
    }
  }

  /** Decide where the incoming chunk plays and at what rate. A change in the playout delay
   *  shows as a step in the stream's own start times between two chunks that are otherwise
   *  back to back; it is closed by playing slightly slower (a rise: the audio we have is now
   *  early) or faster (a fall) from where the last chunk ended, instead of by a gap of silence
   *  or a cut head. Everything else keeps the old behaviour: the queue ran dry, audio is lost
   *  behind a concealment fade, the stream skipped a whole number of chunks (packets that
   *  never arrived), or the step is over the bound (a stall or a reset). */
  private align(chunk: Chunk, f: number): void {
    const end = this.lastEnd;
    const correcting = this.rate !== 1;
    if (end === null) {
      this.rate = 1;
      return;
    }
    const obs = chunk.start - end;
    // The stream's own step: where it says this chunk starts against where the one before ended.
    // A step that is not also there (audio dropped behind the playhead) is lost audio, not a
    // delay change, and stays counted.
    const jump = this.prevStart === null ? obs : chunk.start - (this.prevStart + this.prevFrames);
    const delayChange =
      chunk.targetDelayFrames !== undefined && this.prevTargetDelayFrames !== undefined
        ? chunk.targetDelayFrames - this.prevTargetDelayFrames
        : 0;
    const sourceJump = jump - delayChange;
    const intentional = delayChange !== 0 && Math.abs(sourceJump) <= this.slack;
    const step =
      (end === f || intentional) &&
      !this.lostChunks(sourceJump) &&
      !(this.prevConcealed && sourceJump > this.slack) &&
      Math.abs(obs) > (correcting ? STRETCH_DONE_FRAMES : this.slack) &&
      Math.abs(obs) <= MAX_STRETCH_FRAMES &&
      (correcting || Math.abs(obs - jump) <= this.slack);
    if (step) {
      // If the queue already ran dry, start now: waiting for the new target would count
      // the intentional step as a second underrun and deepen the buffer again.
      chunk.start = Math.max(end, f);
      this.rate = obs > 0 ? 1 - STRETCH_RATE : 1 + STRETCH_RATE;
      return;
    }
    this.rate = 1;
    // Back to back with the last chunk when its timestamp is within the slack; an overlap
    // (a duplicate, or a late reorder) loses its head instead of playing twice.
    if (chunk.start + chunk.out <= end + this.slack) {
      if (chunk.start + chunk.out >= end) chunk.start = end - chunk.out;
      else {
        // An overlap: its head is lost rather than played twice.
        const skip = end - chunk.start - chunk.out;
        chunk.out += skip;
        chunk.off += skip * this.rate;
      }
    }
  }

  /** A whole number of chunks of the stream's starts skipped (within a twentieth of a chunk,
   *  0.5 ms): packets that never arrived or came too late to post, not a delay change, which
   *  lands on any offset. An unmarked rise in the delay that happens to be a whole number of chunks
   *  still plays as silence. The scheduler's known delay change is subtracted first. A fall is never lost audio. */
  private lostChunks(jump: number): boolean {
    const frames = this.prevFrames;
    if (frames <= 0 || jump <= 0) return false;
    const k = Math.round(jump / frames);
    return k >= 1 && Math.abs(jump - k * frames) <= frames / 20;
  }

  /** Play `chunk` on from `pos`, at real time. */
  private copy(chunk: Chunk, out: Float32Array[], pos: number, room: number): number {
    const take = Math.min(chunk.frames - chunk.off, room);
    for (let c = 0; c < out.length; c++) {
      out[c].set(chunk.planes[Math.min(c, chunk.planes.length - 1)].subarray(chunk.off, chunk.off + take), pos);
    }
    chunk.off += take;
    return take;
  }

  /** Play `chunk` on from `pos`, `rate` source frames per output frame: linear interpolation,
   *  which for the 2 % above is a pitch shift of a third of a semitone, not a click. */
  private resample(chunk: Chunk, out: Float32Array[], pos: number, room: number): number {
    let written = 0;
    while (written < room && chunk.off < chunk.frames) {
      const i = Math.floor(chunk.off);
      const t = chunk.off - i;
      const j = i + 1 < chunk.frames ? i + 1 : i;
      for (let c = 0; c < out.length; c++) {
        const src = chunk.planes[Math.min(c, chunk.planes.length - 1)];
        out[c][pos + written] = t === 0 ? src[i] : src[i] + (src[j] - src[i]) * t;
      }
      chunk.off += this.rate;
      written += 1;
    }
    return written;
  }

  private finish(chunk: Chunk, dropped: boolean): void {
    this.queue.shift();
    this.queued -= chunk.frames;
    if (dropped) this.stats.dropped += 1;
    this.prevStart = chunk.nominal;
    this.prevFrames = chunk.frames;
    this.prevTargetDelayFrames = chunk.targetDelayFrames;
    this.prevConcealed = chunk.concealed;
  }

  private underrun(frames: number): void {
    if (this.lastEnd === null) return;
    this.stats.underrunFrames += frames;
    // Silence played: the window reads 0 even when a later chunk is queued but not yet due.
    this.stats.minQueuedFrames = 0;
    if (!this.starving) {
      this.stats.underrunEvents += 1;
      this.stats.silenceRuns += 1;
    }
    this.starving = true;
  }

  /** The least audio waiting in the queue at this quantum: what a buffer reading is the minimum
   *  of, measured once a quantum rather than per sample and with nothing allocated. A
   *  concealment fade counts: it plays in place of the lost packet, so 0 is kept for silence. */
  private noteQueued(): void {
    if (this.lastEnd === null) return; // nothing has played yet: there is no buffer to measure
    let frames = 0;
    for (const chunk of this.queue) frames += Math.max(0, chunk.frames - chunk.off);
    this.stats.minQueuedFrames = Math.min(this.stats.minQueuedFrames, frames);
  }

  private silence(out: Float32Array[], from: number, count: number): void {
    for (const channel of out) channel.fill(0, from, from + count);
  }
}
