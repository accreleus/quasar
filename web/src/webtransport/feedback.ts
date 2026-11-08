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
/** Channels whose datagrams carry `pkt_seq` (§Channels): video, audio, FEC. */
const COUNTED = new Set([0x01, 0x02, 0x04]);

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

  constructor(private lastTake: number) {}

  /** One datagram as it arrived, at `at` ms on the monotonic clock. */
  record(datagram: Uint8Array, at: number): void {
    if (datagram.length < 5 || !COUNTED.has(datagram[0])) return;
    const pktSeq = new DataView(datagram.buffer, datagram.byteOffset, datagram.byteLength).getUint32(1, true);
    if (this.next === null) this.next = pktSeq;
    else if (after(this.next, pktSeq)) return; // already reported as lost
    // A bound on what a stalled reader holds: past it, the oldest go first.
    if (this.arrivals.size >= MAX_HELD) this.arrivals.delete(this.arrivals.keys().next().value!);
    this.arrivals.set(pktSeq, at);
    this.bytes += datagram.length;
  }

  /** The message for everything up to the newest `pkt_seq` that arrived at least
   *  {@link HOLD_BACK_MS} ago; null when there is none. */
  take(now: number): FeedbackMessage | null {
    const first = this.next;
    if (first === null) return null;
    let last: number | null = null;
    for (const [s, at] of this.arrivals) {
      if (at <= now - HOLD_BACK_MS && (last === null || after(s, last))) last = s;
    }
    if (last === null) return null;
    const count = Math.min(((last - first) >>> 0) + 1, MAX_COUNT);
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
