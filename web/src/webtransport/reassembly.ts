import { serialAfter, type VideoFragment } from "./framing";
import type { AccessUnit } from "./transport";

/** Incomplete frames held at once; past it the oldest counts as lost. */
const MAX_PENDING = 64;

interface Pending {
  first: VideoFragment;
  parts: (Uint8Array | undefined)[];
  have: number;
  bytes: number;
}

export interface Reassembled {
  unit: AccessUnit | null;
  /** A frame later ones reference was lost: ask for a keyframe. */
  needKeyframe: boolean;
}

/**
 * Video fragments back into access units (§Video). With no NACK yet, a frame still
 * incomplete when a later one completes is lost; losing one that is not
 * `discardable` drops every unit until the next keyframe (§Loss recovery, the
 * `keyframe_request {reason: "loss"}` path for a client that does no NACK).
 */
export class FrameAssembler {
  private pending = new Map<number, Pending>();
  private lastDone: number | null = null;
  /** Starts true: a client starts at a keyframe, and a delta that completes first is useless. */
  private waitingForKey = true;

  push(f: VideoFragment): Reassembled {
    if (this.lastDone !== null && !serialAfter(f.frameSeq, this.lastDone, 16)) {
      return { unit: null, needKeyframe: false }; // late or a duplicate
    }
    let p = this.pending.get(f.frameSeq);
    if (!p) {
      p = { first: f, parts: new Array(f.fragCount), have: 0, bytes: 0 };
      this.pending.set(f.frameSeq, p);
    }
    if (f.fragCount !== p.first.fragCount || p.parts[f.fragIndex]) {
      return { unit: null, needKeyframe: false };
    }
    p.parts[f.fragIndex] = f.payload;
    p.have += 1;
    p.bytes += f.payload.length;

    let lostReferenced = false;
    if (this.pending.size > MAX_PENDING) lostReferenced = this.dropOlderThan(null);
    if (p.have < p.first.fragCount) return this.verdict(null, lostReferenced);

    this.pending.delete(f.frameSeq);
    lostReferenced = this.dropOlderThan(f.frameSeq) || lostReferenced;
    this.lastDone = f.frameSeq;
    if (p.first.keyframe) this.waitingForKey = false;
    if (this.waitingForKey) return this.verdict(null, lostReferenced);

    // The one copy on the path: fragments into a contiguous unit.
    const data = new Uint8Array(p.bytes);
    let at = 0;
    for (const part of p.parts) {
      data.set(part as Uint8Array, at);
      at += (part as Uint8Array).length;
    }
    const { keyframe, configId, frameSeq, ts90k, captureUs } = p.first;
    return this.verdict({ keyframe, configId, frameSeq, ts90k, captureUs, data }, lostReferenced);
  }

  private verdict(unit: AccessUnit | null, lostReferenced: boolean): Reassembled {
    if (lostReferenced && !(unit?.keyframe ?? false)) {
      this.waitingForKey = true;
      return { unit: null, needKeyframe: true };
    }
    return { unit, needKeyframe: false };
  }

  /** Drops pending frames before `seq` (every one but the newest when null);
   *  true when one of them was referenced by later frames. */
  private dropOlderThan(seq: number | null): boolean {
    let newest: number | null = seq;
    if (newest === null) {
      for (const k of this.pending.keys()) if (newest === null || serialAfter(k, newest, 16)) newest = k;
    }
    let referenced = false;
    for (const [k, p] of this.pending) {
      if (newest !== null && serialAfter(newest, k, 16)) {
        this.pending.delete(k);
        if (!p.first.discardable) referenced = true;
      }
    }
    return referenced;
  }
}
