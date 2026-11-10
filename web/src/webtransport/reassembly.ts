import { serialAfter, type FecRepair, type VideoFragment } from "./framing";
import type { AccessUnit } from "./transport";

/** Frames held at once, incomplete or waiting for an earlier one; past it the oldest counts as lost. */
export const MAX_PENDING = 64;
/** The largest access unit accepted; a `frag_count` that implies more is refused. */
export const MAX_UNIT_BYTES = 4 * 1024 * 1024;
/** Bytes held across every held frame. */
export const MAX_PENDING_BYTES = 8 * 1024 * 1024;
/** An incomplete frame with no fragment for this long is late, whatever its NACKs. Counted from
 *  its latest fragment, so a large keyframe still arriving is kept. */
export const MAX_PENDING_AGE_MS = 500;
/** The round trip assumed until a retransmission measures one. */
export const INITIAL_RTT_MS = 100;
/** Added to the round trip before a NACK counts as unanswered: jitter and the agent's turnaround. */
export const RTT_SLACK_MS = 10;
/** Smallest payload a non-final fragment can carry at the 1200-byte budget (§Channels, about 1123). */
const MIN_FRAGMENT_PAYLOAD = 1_000;
/** A fragment further behind the last completed frame than this is not late: the stream jumped. */
const MAX_LATE_FRAMES = 256;
/** Late fragments in a row after which a keyframe restarts the stream instead of being dropped. */
const LATE_RUN_RESYNC = 32;
/** While waiting for a keyframe, one request per wait, again after this long without it. */
export const KEY_REASK_MS = 1_000;
/** How long a NACK is remembered for its round-trip sample. */
const ASK_MEMORY_MS = 2_000;
const MAX_ASKS_REMEMBERED = 4_096;
/** §NACK: at most 32 ranges per message. */
const MAX_NACK_RANGES = 32;

interface Pending {
  /** Null for a frame known only from the gap it left: nothing of it has arrived. */
  first: VideoFragment | null;
  parts: (Uint8Array | undefined)[];
  have: number;
  bytes: number;
  /** Leading fragments all present, and the highest index received (-1: none). */
  contiguous: number;
  top: number;
  /** NACKs sent per fragment (0, 1 or 2) and when the last went. Index 0 only, until the count is known. */
  asks: number[];
  askedAt: number[];
  at: number;
  late: boolean;
  /** Repair datagrams held for this frame (§FEC), until each has rebuilt its fragment or cannot. */
  repairs?: FecRepair[];
}

/** Repair datagrams held per frame: more than a frame of the largest size could use. */
const MAX_REPAIRS_PER_FRAME = 64;

/** A `nack` to send (§NACK): inclusive `frag_index` ranges of one frame. */
export interface Nack {
  frameSeq: number;
  frags: [number, number][];
}

export interface Reassembled {
  /** Whole units, in `frame_seq` order. */
  units: AccessUnit[];
  nacks: Nack[];
  /** A frame later ones reference is late: ask for a keyframe. */
  needKeyframe: boolean;
}

const gap = (at: number): Pending => ({
  first: null,
  parts: [],
  have: 0,
  bytes: 0,
  contiguous: 0,
  top: -1,
  asks: [0],
  askedAt: [0],
  at,
  late: false,
});

/**
 * Video fragments back into access units, released in `frame_seq` order, with the client
 * half of §Loss recovery. A missing fragment is NACKed as soon as a later fragment shows it
 * missing, and once more after one round trip. A frame is late when a `nack_miss` names one
 * of its missing fragments, its repeat NACK goes unanswered for another round trip, or no
 * fragment of it arrives for {@link MAX_PENDING_AGE_MS}. A late frame is dropped; if it was
 * not `discardable`, every frame is dropped until the next keyframe, and one is asked for.
 * The round trip is measured from a fragment's latest NACK to its retransmission, even one
 * that arrives after its frame was given up. A longer sample at most doubles the estimate
 * and a shorter one is averaged in at 1/8, so one outlier, or a resend answering a repeat
 * whose first resend was lost, cannot run it away. Deadlines are checked on every datagram, every
 * `nack_miss` and every {@link FrameAssembler.tick}.
 */
export class FrameAssembler {
  private pending = new Map<number, Pending>();
  /** Complete frames waiting for an earlier one. */
  private ready = new Map<number, AccessUnit>();
  private bytes = 0;
  private lastDone: number | null = null;
  private highest: number | null = null;
  /** Starts true: a client starts at a keyframe, and a delta that completes first is useless. */
  private waitingForKey = true;
  private lateRun = 0;
  private needKey = false;
  /** When this keyframe wait last asked for one; null when not waiting or not asked. */
  private keyAskedAt: number | null = null;
  private srtt: number | null = null;
  /** Latest NACK time per `frameSeq * 65536 + fragIndex`, in time order. */
  private asked = new Map<number, number>();

  constructor(private readonly now: () => number = () => performance.now()) {}

  /** Fragments found missing so far, each counted once at its first NACK: the loss telemetry reads. */
  lost = 0;

  /** The round trip loss recovery waits on, in ms. */
  get rttMs(): number {
    return this.srtt ?? INITIAL_RTT_MS;
  }

  push(f: VideoFragment): Reassembled {
    const at = this.now();
    this.expire(at);
    if (f.retransmit) this.sample(f.frameSeq * 65_536 + f.fragIndex, at);

    if (this.lastDone !== null && !serialAfter(f.frameSeq, this.lastDone, 16)) {
      const behind = (this.lastDone - f.frameSeq + 65_536) % 65_536;
      const jumped = behind > MAX_LATE_FRAMES;
      const resync = jumped || (f.keyframe && this.lateRun >= LATE_RUN_RESYNC);
      if (!resync) {
        if (behind > 0) this.lateRun += 1;
        return this.step(at); // late, or a duplicate of a released frame
      }
      this.restart(f);
    }
    this.lateRun = 0;

    if (this.highest !== null && serialAfter(f.frameSeq, this.highest, 16)) {
      if ((f.frameSeq - this.highest + 65_536) % 65_536 > MAX_PENDING) this.restart(f);
      else {
        // Every frame between is missing whole, and known only by its number.
        for (let s = (this.highest + 1) & 0xffff; s !== f.frameSeq; s = (s + 1) & 0xffff) {
          if (!this.pending.has(s) && !this.ready.has(s)) this.pending.set(s, gap(at));
        }
      }
    }
    if (this.highest === null || serialAfter(f.frameSeq, this.highest, 16)) this.highest = f.frameSeq;
    if (this.ready.has(f.frameSeq)) return this.step(at);

    // A count no unit within the byte budget needs: refused before anything is held.
    const payload = f.fragIndex < f.fragCount - 1 ? f.payload.length : MIN_FRAGMENT_PAYLOAD;
    if (f.fragCount > Math.ceil(MAX_UNIT_BYTES / Math.max(payload, MIN_FRAGMENT_PAYLOAD))) {
      this.lose(f.frameSeq, f.discardable);
      return this.step(at);
    }

    let p = this.pending.get(f.frameSeq);
    if (!p) {
      p = gap(at);
      this.pending.set(f.frameSeq, p);
    }
    if (!p.first) {
      p.first = f;
      p.parts = new Array(f.fragCount);
      p.asks = Array.from({ length: f.fragCount }, (_, i) => (i === 0 ? p!.asks[0] : 0));
      p.askedAt = Array.from({ length: f.fragCount }, (_, i) => (i === 0 ? p!.askedAt[0] : 0));
    } else if (!sameFrame(p.first, f)) {
      // Fragments of one frame disagree: none of them can be trusted.
      this.lose(f.frameSeq, p.first.discardable);
      return this.step(at);
    }
    if (p.parts[f.fragIndex]) return this.step(at);
    // FEC first (§Loss recovery 4): a fragment the frame's repairs can now rebuild is rebuilt
    // here, before recover() would NACK it.
    if (this.store(f.frameSeq, p, f.fragIndex, f.payload, at)) this.rebuild(f.frameSeq, p, at);
    while (this.pending.size + this.ready.size > MAX_PENDING || this.bytes > MAX_PENDING_BYTES) {
      if (!this.dropOldest()) break;
    }
    return this.step(at);
  }

  /**
   * One fragment's payload into its frame. False when the frame is gone afterwards: whole and
   * moved to `ready`, or lost for being over the byte budget.
   */
  private store(seq: number, p: Pending, index: number, payload: Uint8Array, at: number): boolean {
    const first = p.first as VideoFragment;
    p.parts[index] = payload;
    p.at = at;
    p.have += 1;
    p.bytes += payload.length;
    p.top = Math.max(p.top, index);
    while (p.parts[p.contiguous]) p.contiguous += 1;
    this.bytes += payload.length;
    if (p.bytes > MAX_UNIT_BYTES) {
      this.lose(seq, first.discardable);
      return false;
    }
    if (p.have === first.fragCount) {
      this.complete(seq, p);
      return false;
    }
    return true;
  }

  /**
   * A repair datagram (§FEC). It is used at once when its group lacks exactly one fragment,
   * and held otherwise: a later fragment may leave one missing. One for a frame that is not
   * pending (whole, lost, or not seen yet) is dropped; the agent sends repairs after the
   * fragments they protect.
   */
  repair(r: FecRepair): Reassembled {
    const at = this.now();
    this.expire(at);
    const p = this.pending.get(r.frameSeq);
    if (p && !p.late && (p.repairs?.length ?? 0) < MAX_REPAIRS_PER_FRAME) {
      (p.repairs ??= []).push(r);
      this.rebuild(r.frameSeq, p, at);
    }
    return this.step(at);
  }

  /** Fragments rebuilt from repair datagrams, for telemetry. */
  repaired = 0;

  /** Rebuilds every fragment the frame's repairs can, until none makes progress. */
  private rebuild(seq: number, p: Pending, at: number): void {
    const first = p.first;
    // A rebuilt fragment takes a sibling's header (§FEC): nothing to do before one has arrived.
    if (!first || !p.repairs) return;
    for (let progress = true; progress; ) {
      progress = false;
      for (const r of [...p.repairs]) {
        const missing: number[] = [];
        let lengthXor = r.lengthXor;
        const last = r.firstFrag + (r.count - 1) * r.stride;
        for (let i = r.firstFrag; i <= last && last < first.fragCount; i += r.stride) {
          const part = p.parts[i];
          if (part) lengthXor ^= part.length;
          else missing.push(i);
        }
        // Past the frame, nothing left to rebuild, or a length its own payload cannot hold: no use.
        if (last >= first.fragCount || missing.length === 0 || (missing.length === 1 && lengthXor > r.payload.length)) {
          p.repairs.splice(p.repairs.indexOf(r), 1);
          continue;
        }
        if (missing.length > 1) continue; // two or more missing is NACK's (§FEC)
        const out = r.payload.slice(0, lengthXor);
        for (let i = r.firstFrag; i <= last; i += r.stride) {
          const part = p.parts[i];
          if (!part) continue;
          const n = Math.min(part.length, out.length);
          for (let b = 0; b < n; b++) out[b] ^= part[b];
        }
        p.repairs.splice(p.repairs.indexOf(r), 1);
        this.repaired += 1;
        if (!this.store(seq, p, missing[0], out, at)) return;
        progress = true;
      }
    }
  }

  /** `nack_miss`: the agent no longer holds these fragments, so a frame missing one is late. */
  miss(frameSeq: number, frags: [number, number][]): Reassembled {
    const p = this.pending.get(frameSeq);
    if (p && !p.late) {
      const count = p.first ? p.first.fragCount : 1;
      for (const [a, b] of frags.slice(0, MAX_NACK_RANGES)) {
        for (let i = Math.max(0, a); i <= Math.min(b, count - 1); i++) if (!p.parts[i]) p.late = true;
      }
    }
    return this.step(this.now());
  }

  /** The deadlines with no datagram: called on a timer, so release never waits for one. */
  tick(): Reassembled {
    const at = this.now();
    this.expire(at);
    return this.step(at);
  }

  private step(at: number): Reassembled {
    const nacks = this.recover(at);
    const units: AccessUnit[] = [];
    this.drain(units);
    let needKeyframe = false;
    if (!this.waitingForKey) this.keyAskedAt = null;
    else if (this.keyAskedAt === null ? this.needKey : at - this.keyAskedAt >= KEY_REASK_MS) {
      needKeyframe = true;
      this.keyAskedAt = at;
    }
    this.needKey = false;
    return { units, nacks, needKeyframe };
  }

  /** A retransmission's round trip, from the latest NACK for it. After a repeat, a resend
   *  answering the first NACK reads short, and is only averaged in. */
  private sample(key: number, at: number): void {
    const asked = this.asked.get(key);
    if (asked === undefined) return;
    this.asked.delete(key);
    const rtt = Math.max(1, at - asked);
    if (this.srtt === null) this.srtt = Math.min(rtt, MAX_PENDING_AGE_MS);
    else if (rtt > this.srtt) this.srtt = Math.min(rtt, 2 * this.srtt);
    else this.srtt = (7 * this.srtt + rtt) / 8;
  }

  /** The NACKs now due, and late marks for repeats that went unanswered. */
  private recover(at: number): Nack[] {
    const wait = this.rttMs + RTT_SLACK_MS;
    for (const [k, t] of this.asked) {
      if (at - t <= ASK_MEMORY_MS && this.asked.size <= MAX_ASKS_REMEMBERED) break;
      this.asked.delete(k);
    }
    const nacks: Nack[] = [];
    for (const [seq, p] of this.pending) {
      // Waiting for a keyframe, a delta is dropped whatever happens to it.
      if (p.late || (this.waitingForKey && p.first && !p.first.keyframe)) continue;
      const later = this.highest !== null && serialAfter(this.highest, seq, 16);
      const known = later ? (p.first ? p.first.fragCount : 1) : p.top;
      const frags: [number, number][] = [];
      for (let i = p.contiguous; i < known && !p.late; i++) {
        if (p.parts[i]) continue;
        const due = p.asks[i] === 0 || at - p.askedAt[i] >= wait;
        if (!due) continue;
        if (p.asks[i] >= 2) p.late = true;
        else if (addRange(frags, i)) {
          // Re-inserted, so the map stays in time order for the prune above.
          this.asked.delete(seq * 65_536 + i);
          this.asked.set(seq * 65_536 + i, at);
          if (p.asks[i] === 0) this.lost += 1;
          p.asks[i] += 1;
          p.askedAt[i] = at;
        }
      }
      if (!p.late && frags.length) nacks.push({ frameSeq: seq, frags });
    }
    return nacks;
  }

  /** Release whole frames in order, and settle late ones at the head. */
  private drain(out: AccessUnit[]): void {
    for (;;) {
      if (this.waitingForKey) {
        let key: number | null = null;
        for (const [s, u] of this.ready) if (u.keyframe && (key === null || serialAfter(key, s, 16))) key = s;
        if (key === null) return;
        this.dropBefore(key);
        this.waitingForKey = false;
        this.release(key, out);
        continue;
      }
      const next = ((this.lastDone as number) + 1) & 0xffff;
      if (this.ready.has(next)) {
        this.release(next, out);
        continue;
      }
      const p = this.pending.get(next);
      if (p) {
        if (!p.late) return;
        this.lose(next, p.first?.discardable ?? false);
      } else if (this.highest === null || !serialAfter(this.highest, next, 16)) {
        return; // nothing after it has arrived
      }
      this.lastDone = next; // lost and dropped: discardable, or the keyframe wait has begun
    }
  }

  private release(seq: number, out: AccessUnit[]): void {
    const u = this.ready.get(seq) as AccessUnit;
    this.ready.delete(seq);
    this.bytes -= u.data.length;
    this.lastDone = seq;
    out.push(u);
  }

  private complete(seq: number, p: Pending): void {
    this.pending.delete(seq);
    // Held even while waiting for a keyframe: it may follow one still incomplete. The deltas
    // before the keyframe go when it is released.
    const first = p.first as VideoFragment;
    // The one copy on the path: fragments into a contiguous unit, which the decoder takes by transfer.
    const data = new Uint8Array(p.bytes);
    let offset = 0;
    for (const part of p.parts as Uint8Array[]) {
      data.set(part, offset);
      offset += part.length;
    }
    const { keyframe, configId, ts90k, captureUs } = first;
    this.ready.set(seq, { keyframe, configId, frameSeq: seq, ts90k, captureUs, data });
  }

  /** Forget a frame as lost; one later frames reference starts the keyframe wait. */
  private lose(seq: number, discardable: boolean): void {
    const p = this.pending.get(seq);
    if (p) {
      this.pending.delete(seq);
      this.bytes -= p.bytes;
    }
    if (!discardable) {
      this.waitingForKey = true;
      this.needKey = true;
    }
  }

  /** A frame from far outside the held window: start over from it. */
  private restart(f: VideoFragment): void {
    this.pending.clear();
    this.ready.clear();
    this.bytes = 0;
    this.lastDone = null;
    this.highest = null;
    this.waitingForKey = true;
    this.lateRun = 0;
    if (!f.keyframe) this.needKey = true; // a jump lands mid-GOP: ask for the keyframe it needs
  }

  private expire(at: number): void {
    for (const [k, p] of this.pending) {
      if (at - p.at > MAX_PENDING_AGE_MS) this.lose(k, p.first?.discardable ?? false);
    }
  }

  /** Drops the oldest held frame; false when nothing is held. */
  private dropOldest(): boolean {
    let oldest: number | null = null;
    for (const k of [...this.pending.keys(), ...this.ready.keys()]) {
      if (oldest === null || serialAfter(oldest, k, 16)) oldest = k;
    }
    if (oldest === null) return false;
    const u = this.ready.get(oldest);
    if (u) {
      this.ready.delete(oldest);
      this.bytes -= u.data.length;
      this.lose(oldest, false);
    } else this.lose(oldest, this.pending.get(oldest)?.first?.discardable ?? false);
    return true;
  }

  private dropBefore(seq: number): void {
    for (const k of this.pending.keys()) if (serialAfter(seq, k, 16)) this.lose(k, true);
    for (const [k, u] of this.ready) {
      if (serialAfter(seq, k, 16)) {
        this.ready.delete(k);
        this.bytes -= u.data.length;
      }
    }
  }
}

/** Add `i` to `ranges`, extending the last when `i` follows it; false when a 33rd would be needed. */
function addRange(ranges: [number, number][], i: number): boolean {
  const last = ranges[ranges.length - 1];
  if (last && last[1] === i - 1) last[1] = i;
  else if (ranges.length < MAX_NACK_RANGES) ranges.push([i, i]);
  else return false;
  return true;
}

/** §Video: every fragment of a frame carries the same count, config, flags and timestamps. */
function sameFrame(a: VideoFragment, b: VideoFragment): boolean {
  return (
    a.fragCount === b.fragCount &&
    a.configId === b.configId &&
    a.keyframe === b.keyframe &&
    a.ts90k === b.ts90k
  );
}
