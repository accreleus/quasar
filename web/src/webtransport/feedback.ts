// Receiver feedback for congestion control: protocol/transport-webtransport.md §Feedback.
// The agent turns it into the rate estimate that drives the session's ABR.

/** §Feedback: every 50 ms while datagrams arrive. */
export const FEEDBACK_MS = 50;
const MAX_COUNT = 8192;
const MAX_HELD = 4 * MAX_COUNT;
/** Arrival offsets are in 50 µs units; this one means not received (yet). */
const NOT_RECEIVED = 0xffff;
/** A datagram is reported only once it is this old: one reordered by less is not a loss. */
export const HOLD_BACK_MS = 5;
/** The reorder window never grows past this, so a real loss is still reported within it (#1330).
 *  The media path also caps it by the agent's retransmit window ({@link FeedbackRecorder.capMs}). */
export const MAX_REORDER_MS = 150;
/** The window is a high percentile of the lateness measured over this much recent time, and none
 *  until there are enough measurements: one late datagram is a spike, not a path that reorders. */
export const REORDER_HISTORY_MS = 1_000;
export const REORDER_MIN_SAMPLES = 8;
/** Below about 100 measurements in the history this is the largest one or two. */
const REORDER_PERCENTILE = 0.99;
const BUCKET_MS = 5;
/** Channels whose datagrams carry `pkt_seq` (§Channels): video, audio, audio redundancy, FEC. */
const COUNTED = new Set([0x01, 0x02, 0x04, 0x06]);

/** Is `a` after `b`, in u32 serial-number arithmetic (§Channels, "Wrap-around")? */
const after = (a: number, b: number) => {
  const d = (a - b) >>> 0;
  return d !== 0 && d < 0x8000_0000;
};

export interface FeedbackMessage {
  type: "feedback";
  seq: number;
  first_pkt_seq: number;
  count: number;
  ref_us: number;
  arrivals_b64: string;
  recv_kbps: number;
  lost: number;
  interval_ms: number;
}

/** Records when each agent → client datagram arrived and reports them in `pkt_seq` order. */
export class FeedbackRecorder {
  /** The first `pkt_seq` not reported yet. */
  private next: number | null = null;
  private readonly arrivals = new Map<number, number>();
  private bytes = 0;
  private seq = 0;
  /** The highest `pkt_seq` arrived, and when each missing one below it was first passed by a
   *  later arrival, in time order; dropped once older than {@link MAX_REORDER_MS}. */
  private highest: number | null = null;
  private passedAt = new Map<number, number>();
  /** Lateness measured in the last {@link REORDER_HISTORY_MS} (from `head` on), and how many fall
   *  in each {@link BUCKET_MS} bucket. */
  private lateness: { at: number; bucket: number }[] = [];
  private head = 0;
  private readonly buckets = new Array<number>(Math.ceil(MAX_REORDER_MS / BUCKET_MS)).fill(0);
  /** Datagrams that arrived since the last message, and the rate of the last message's interval. */
  private received = 0;
  private perMs = 0;
  /** Lateness measured since the last {@link takeLate}. */
  private late = 0;

  constructor(private lastTake: number) {}

  /** The most the window may be, in ms: the media path sets it so a held NACK still finds its
   *  fragment in the agent's retransmit window (§Hello ack `retransmit_window_ms`). */
  capMs: () => number = () => MAX_REORDER_MS;

  /** How long a gap may stay open before it is a loss, in ms beyond {@link HOLD_BACK_MS}: how
   *  late reordered datagrams have recently come, 0 on a path that does not reorder. The frame
   *  assembler holds its NACKs by it too, so a late fragment is neither lost nor resent. */
  reorderMs(now: number): number {
    for (; this.head < this.lateness.length && now - this.lateness[this.head].at > REORDER_HISTORY_MS; this.head++) {
      this.buckets[this.lateness[this.head].bucket]--;
    }
    if (this.head > 1024) {
      this.lateness = this.lateness.slice(this.head);
      this.head = 0;
    }
    const n = this.lateness.length - this.head;
    if (n < REORDER_MIN_SAMPLES) return 0;
    let b = 0;
    for (let k = Math.ceil(REORDER_PERCENTILE * n); (k -= this.buckets[b]) > 0; b++);
    return Math.max(0, Math.min((b + 1) * BUCKET_MS, this.capMs()));
  }

  /** One datagram as it arrived, at `at` ms on the monotonic clock. */
  record(datagram: Uint8Array, at: number): void {
    if (datagram.length < 5 || !COUNTED.has(datagram[0])) return;
    const pktSeq = new DataView(datagram.buffer, datagram.byteOffset, datagram.byteLength).getUint32(1, true);
    this.timeReordering(pktSeq, at);
    this.received++;
    if (this.next === null) this.next = pktSeq;
    else if (after(this.next, pktSeq)) return; // already reported as lost
    // A bound on what a stalled reader holds: past it, the oldest go first.
    if (this.arrivals.size >= MAX_HELD) this.arrivals.delete(this.arrivals.keys().next().value!);
    this.arrivals.set(pktSeq, at);
    this.bytes += datagram.length;
  }

  private timeReordering(pktSeq: number, at: number): void {
    for (const [s, t] of this.passedAt) {
      if (at - t <= MAX_REORDER_MS) break;
      this.passedAt.delete(s);
    }
    if (this.highest === null || after(pktSeq, this.highest)) {
      // Reordering within the window can only open a gap of what arrives within it at the current
      // rate; a wider one is an outage, reported at once.
      const missing = (pktSeq - (this.highest ?? pktSeq) - 1) >>> 0;
      if (this.highest !== null && missing <= this.perMs * Math.min(MAX_REORDER_MS, this.capMs())) {
        for (let s = (this.highest + 1) >>> 0; s !== pktSeq; s = (s + 1) >>> 0) this.passedAt.set(s, at);
      }
      this.highest = pktSeq;
      return;
    }
    const passed = this.passedAt.get(pktSeq);
    if (passed === undefined) return;
    this.passedAt.delete(pktSeq);
    const bucket = Math.min(Math.floor((at - passed) / BUCKET_MS), this.buckets.length - 1);
    this.lateness.push({ at, bucket });
    this.buckets[bucket]++;
    this.late++;
  }

  /** How many late datagrams were measured since the last call. Only a datagram that fills a gap
   *  a later one opened counts: a path that loses without reordering (a resend has its own
   *  `pkt_seq`) or an outage (a gap too wide to time) measures none, and nothing is measured at all
   *  while {@link capMs} is 0 or less (before the first pong, or a round trip near the agent's
   *  retransmit window), so a rule reading this is off then (#1330). */
  takeLate(): number {
    const n = this.late;
    this.late = 0;
    return n;
  }

  /** The message for everything up to the newest `pkt_seq` that arrived at least
   *  {@link HOLD_BACK_MS} ago, ending before any gap a later datagram passed less than the
   *  reorder window ago; null when there is none. */
  take(now: number): FeedbackMessage | null {
    const first = this.next;
    if (first === null) return null;
    let last: number | null = null;
    for (const [s, at] of this.arrivals) {
      if (at <= now - HOLD_BACK_MS && (last === null || after(s, last))) last = s;
    }
    if (last === null) return null;
    let count = Math.min(((last - first) >>> 0) + 1, MAX_COUNT);
    // A gap still inside the window may be a datagram on its way (#1330): it waits, with all after it.
    const open = now - HOLD_BACK_MS - this.reorderMs(now);
    for (let i = 0; i < count; i++) {
      const s = (first + i) >>> 0;
      if (!this.arrivals.has(s) && (this.passedAt.get(s) ?? -Infinity) > open) count = i;
    }
    if (count === 0) return null;
    let ref = Infinity;
    for (let i = 0; i < count; i++) ref = Math.min(ref, this.arrivals.get((first + i) >>> 0) ?? Infinity);
    if (ref === Infinity) ref = now; // nothing in range arrived: every entry is 0xFFFF
    const out = new Uint8Array(count * 2);
    const view = new DataView(out.buffer);
    let lost = 0;
    for (let i = 0; i < count; i++) {
      const s = (first + i) >>> 0;
      const at = this.arrivals.get(s);
      this.arrivals.delete(s);
      if (at === undefined) lost++;
      const unit = at === undefined ? NOT_RECEIVED : Math.min(Math.round(((at - ref) * 1000) / 50), NOT_RECEIVED - 1);
      view.setUint16(i * 2, unit, true);
    }
    this.next = (first + count) >>> 0;
    const interval = Math.max(now - this.lastTake, 1);
    this.lastTake = now;
    this.perMs = this.received / interval;
    this.received = 0;
    const recvKbps = Math.round((this.bytes * 8) / interval);
    this.bytes = 0;
    let bin = "";
    for (const b of out) bin += String.fromCharCode(b);
    return {
      type: "feedback",
      seq: ++this.seq,
      first_pkt_seq: first,
      count,
      ref_us: Math.round(ref * 1000),
      arrivals_b64: btoa(bin),
      recv_kbps: recvKbps,
      lost,
      interval_ms: Math.round(interval),
    };
  }
}
