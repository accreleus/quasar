import { serialAfter, type VideoFragment } from "./framing";
import type { AccessUnit } from "./transport";

/** Incomplete frames held at once; past it the oldest counts as lost. */
export const MAX_PENDING = 64;
/** The largest access unit accepted; a `frag_count` that implies more is refused. */
export const MAX_UNIT_BYTES = 4 * 1024 * 1024;
/** Bytes held across every incomplete frame. */
export const MAX_PENDING_BYTES = 8 * 1024 * 1024;
/** An incomplete frame older than this is lost: with no NACK yet nothing will complete it. */
export const MAX_PENDING_AGE_MS = 500;
/** Smallest payload a non-final fragment can carry at the 1200-byte budget (§Channels, about 1123). */
const MIN_FRAGMENT_PAYLOAD = 1_000;
/** A fragment further behind the last completed frame than this is not late: the stream jumped. */
const MAX_LATE_FRAMES = 256;
/** Late fragments in a row after which a keyframe restarts the stream instead of being dropped. */
const LATE_RUN_RESYNC = 32;

interface Pending {
  first: VideoFragment;
  parts: (Uint8Array | undefined)[];
  have: number;
  bytes: number;
  at: number;
}

export interface Reassembled {
  unit: AccessUnit | null;
  /** A frame later ones reference was lost: ask for a keyframe. */
  needKeyframe: boolean;
}

const NOTHING: Reassembled = { unit: null, needKeyframe: false };

/**
 * Video fragments back into access units (§Video). With no NACK yet, a frame still
 * incomplete when a later one completes, or older than {@link MAX_PENDING_AGE_MS}, is
 * lost; losing one that is not `discardable` drops every unit until the next keyframe
 * (§Loss recovery, the `keyframe_request {reason: "loss"}` path for a client that does no NACK).
 */
export class FrameAssembler {
  private pending = new Map<number, Pending>();
  private pendingBytes = 0;
  private lastDone: number | null = null;
  /** Starts true: a client starts at a keyframe, and a delta that completes first is useless. */
  private waitingForKey = true;
  private lateRun = 0;

  constructor(private readonly now: () => number = () => performance.now()) {}

  push(f: VideoFragment): Reassembled {
    const at = this.now();
    let lost = this.expire(at);

    if (this.lastDone !== null && !serialAfter(f.frameSeq, this.lastDone, 16)) {
      const behind = (this.lastDone - f.frameSeq + 65_536) % 65_536;
      const jumped = behind > MAX_LATE_FRAMES;
      const resync = jumped || (f.keyframe && this.lateRun >= LATE_RUN_RESYNC);
      if (!resync) {
        if (behind > 0) this.lateRun += 1;
        return this.verdict(null, lost); // late, or a duplicate of a completed frame
      }
      this.reset();
      lost = lost || !f.keyframe; // a jump lands mid-GOP: ask for the keyframe it needs
    }
    this.lateRun = 0;

    // A count no unit within the byte budget needs: refused before anything is held.
    const payload = f.fragIndex < f.fragCount - 1 ? f.payload.length : MIN_FRAGMENT_PAYLOAD;
    if (f.fragCount > Math.ceil(MAX_UNIT_BYTES / Math.max(payload, MIN_FRAGMENT_PAYLOAD))) {
      return this.verdict(null, lost);
    }

    let p = this.pending.get(f.frameSeq);
    if (!p) {
      p = { first: f, parts: new Array(f.fragCount), have: 0, bytes: 0, at };
      this.pending.set(f.frameSeq, p);
    } else if (!sameFrame(p.first, f)) {
      // Fragments of one frame disagree: none of them can be trusted.
      this.drop(f.frameSeq, p);
      return this.verdict(null, lost || !p.first.discardable);
    }
    if (p.parts[f.fragIndex]) return this.verdict(null, lost);
    p.parts[f.fragIndex] = f.payload;
    p.have += 1;
    p.bytes += f.payload.length;
    this.pendingBytes += f.payload.length;
    if (p.bytes > MAX_UNIT_BYTES) {
      this.drop(f.frameSeq, p);
      return this.verdict(null, lost || !p.first.discardable);
    }

    if (this.pending.size > MAX_PENDING || this.pendingBytes > MAX_PENDING_BYTES) {
      lost = this.dropOldest() || lost;
    }
    if (p.have < p.first.fragCount || !this.pending.has(f.frameSeq)) return this.verdict(null, lost);

    this.drop(f.frameSeq, p);
    lost = this.dropOlderThan(f.frameSeq) || lost;
    this.lastDone = f.frameSeq;
    if (p.first.keyframe) this.waitingForKey = false;
    if (this.waitingForKey) return this.verdict(null, lost);

    // The one copy on the path: fragments into a contiguous unit, which the decoder takes by transfer.
    const data = new Uint8Array(p.bytes);
    let offset = 0;
    for (const part of p.parts as Uint8Array[]) {
      data.set(part, offset);
      offset += part.length;
    }
    const { keyframe, configId, frameSeq, ts90k, captureUs } = p.first;
    return this.verdict({ keyframe, configId, frameSeq, ts90k, captureUs, data }, lost);
  }

  private verdict(unit: AccessUnit | null, lostReferenced: boolean): Reassembled {
    if (lostReferenced && !(unit?.keyframe ?? false)) {
      this.waitingForKey = true;
      return { unit: null, needKeyframe: true };
    }
    return unit ? { unit, needKeyframe: false } : NOTHING;
  }

  private drop(seq: number, p: Pending): void {
    this.pending.delete(seq);
    this.pendingBytes -= p.bytes;
  }

  private reset(): void {
    this.pending.clear();
    this.pendingBytes = 0;
    this.lastDone = null;
    this.waitingForKey = true;
    this.lateRun = 0;
  }

  /** Drops frames past their age; true when one of them was referenced. */
  private expire(at: number): boolean {
    let referenced = false;
    for (const [k, p] of this.pending) {
      if (at - p.at > MAX_PENDING_AGE_MS) {
        this.drop(k, p);
        if (!p.first.discardable) referenced = true;
      }
    }
    return referenced;
  }

  /** Drops the oldest pending frame. */
  private dropOldest(): boolean {
    let oldest: number | null = null;
    for (const k of this.pending.keys()) if (oldest === null || serialAfter(oldest, k, 16)) oldest = k;
    if (oldest === null) return false;
    const p = this.pending.get(oldest) as Pending;
    this.drop(oldest, p);
    return !p.first.discardable;
  }

  /** Drops pending frames before `seq`; true when one of them was referenced. */
  private dropOlderThan(seq: number): boolean {
    let referenced = false;
    for (const [k, p] of this.pending) {
      if (serialAfter(seq, k, 16)) {
        this.drop(k, p);
        if (!p.first.discardable) referenced = true;
      }
    }
    return referenced;
  }
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
