// PyroWave datagrams back into decodable frames (protocol/codec-pyrowave.md §Video datagrams,
// §Receiving): per frame by frame_seq, each datagram validated, its coverage range marked, an
// oversized block rejoined from its parts. A frame is handed on when every datagram arrived or
// at its deadline with what did; a block nothing received covers is concealed by the decoder.

import { serialAfter, type VideoFragment } from "../webtransport/framing";

/** The PyroWave header in front of every PyroWave datagram payload. */
export const PYROWAVE_HEADER = 12;
/** Incomplete frames held at once; a newer one past this drops the oldest. */
export const MAX_PENDING_FRAMES = 4;
/** About 4K at 1 Gbit/s at 60 fps fits well inside; a larger frag_count is refused. */
export const MAX_FRAG_COUNT = 8192;

const KIND_PACKETS = 0;
const KIND_PART = 1;

export interface PyroWaveFrame {
  configId: number;
  frameSeq: number;
  ts90k: number;
  /** The agent's wall clock, Unix µs. */
  captureUs: number;
  /** Buffers of whole packets, as the decoder's `decode` takes them. */
  packets: Uint8Array[];
  /** Per block index, nonzero when a received datagram covers it (§Coverage). */
  covered: Uint8Array;
  /** Every one of the frame's datagrams arrived. */
  complete: boolean;
}

interface Oversized {
  count: number;
  first: number;
  end: number;
  parts: (Uint8Array | undefined)[];
  have: number;
  bad: boolean;
}

interface Pending {
  configId: number;
  fragCount: number;
  ts90k: number;
  captureUs: number;
  deadline: number;
  seen: Uint8Array;
  have: number;
  covered: Uint8Array;
  packets: Uint8Array[];
  /** Keyed by the frag_index of each block's part 0. */
  oversized: Map<number, Oversized>;
}

/**
 * One PyroWave configuration's receiver: `n` block indices, a frame deadline `frameMs` after its
 * first datagram (§Receiving's SHOULD: no later than one frame interval).
 */
export class PyroWaveReceiver {
  private pending = new Map<number, Pending>();
  private lastDone: number | null = null;
  /** Datagrams missing from every frame closed or dropped incomplete, cumulative. A frame none
   *  of whose datagrams arrived is not seen, so not counted. */
  lost = 0;

  constructor(
    readonly configId: number,
    private readonly n: number,
    private readonly frameMs: number,
  ) {}

  /** Takes one video fragment of this configuration; returns the frame it completed, if any. */
  push(f: VideoFragment, now: number): PyroWaveFrame | null {
    if (f.configId !== this.configId) return null;
    // Late or duplicate: a frame not after the last decoded is dropped (§Receiving).
    if (this.lastDone !== null && !serialAfter(f.frameSeq, this.lastDone, 16)) return null;
    let p = this.pending.get(f.frameSeq);
    if (!p) {
      if (f.fragCount > MAX_FRAG_COUNT) return null;
      if (this.pending.size >= MAX_PENDING_FRAMES) this.dropOldest();
      p = {
        configId: f.configId,
        fragCount: f.fragCount,
        ts90k: f.ts90k,
        captureUs: f.captureUs,
        deadline: now + this.frameMs,
        seen: new Uint8Array(f.fragCount),
        have: 0,
        covered: new Uint8Array(this.n),
        packets: [],
        oversized: new Map(),
      };
      this.pending.set(f.frameSeq, p);
    }
    // A datagram that disagrees with its frame's first is dropped, so it counts as lost.
    if (f.fragCount !== p.fragCount || f.ts90k !== p.ts90k || p.seen[f.fragIndex]) return null;
    p.seen[f.fragIndex] = 1;
    p.have += 1;
    this.accept(p, f);
    if (p.have < p.fragCount) return null;
    return this.finish(f.frameSeq, p, true);
  }

  /** The frame whose deadline passed, the newest if several did; older pending ones are dropped. */
  due(now: number): PyroWaveFrame | null {
    let best: number | null = null;
    for (const [seq, p] of this.pending) {
      if (p.deadline <= now && (best === null || serialAfter(seq, best, 16))) best = seq;
    }
    return best === null ? null : this.finish(best, this.pending.get(best) as Pending, false);
  }

  /** When `due` next has a frame to give, or null with nothing pending. */
  nextDeadline(): number | null {
    let at: number | null = null;
    for (const p of this.pending.values()) if (at === null || p.deadline < at) at = p.deadline;
    return at;
  }

  private finish(seq: number, p: Pending, complete: boolean): PyroWaveFrame {
    for (const [k, q] of [...this.pending]) {
      if (k !== seq && !serialAfter(seq, k, 16)) continue;
      this.lost += q.fragCount - q.have;
      this.pending.delete(k);
    }
    this.lastDone = seq;
    const { configId, ts90k, captureUs, packets, covered } = p;
    return { configId, frameSeq: seq, ts90k, captureUs, packets, covered, complete };
  }

  private dropOldest(): void {
    let oldest: number | null = null;
    for (const k of this.pending.keys()) if (oldest === null || serialAfter(oldest, k, 16)) oldest = k;
    if (oldest === null) return;
    const p = this.pending.get(oldest) as Pending;
    this.lost += p.fragCount - p.have;
    this.pending.delete(oldest);
    // Closed: a late datagram must not reopen it and have its gaps counted again.
    this.lastDone = oldest;
  }

  /** Validates one datagram and records what it carries; an invalid one adds nothing. */
  private accept(p: Pending, f: VideoFragment): void {
    const d = f.payload;
    if (d.byteLength < PYROWAVE_HEADER) return;
    const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
    const kind = d[0];
    const partIndex = d[1];
    const partCount = d[2];
    const first = v.getUint32(4, true);
    const end = v.getUint32(8, true);
    const body = d.subarray(PYROWAVE_HEADER);
    if (end > this.n || first > end) return;
    if (kind === KIND_PACKETS) {
      const blocks = packetIndices(body);
      if (!blocks) return;
      // The one empty range: datagram 0 holding only the SOF (block 0 is oversized).
      const sofOnly = blocks.length === 0 && body.byteLength === 8;
      if (first === end && !(f.fragIndex === 0 && first === 0 && sofOnly)) return;
      if (blocks.some((i) => i < first || i >= end)) return;
      p.covered.fill(1, first, end);
      if (body.byteLength) p.packets.push(body);
      return;
    }
    if (kind !== KIND_PART || partCount < 2 || partIndex >= partCount || first === end) return;
    const base = f.fragIndex - partIndex;
    let o = p.oversized.get(base);
    if (!o) {
      o = { count: partCount, first, end, parts: new Array(partCount), have: 0, bad: false };
      p.oversized.set(base, o);
    }
    if (o.count !== partCount || o.first !== first || o.end !== end) {
      o.bad = true;
      return;
    }
    if (partIndex === 0 && (body.byteLength < 8 || new DataView(body.buffer, body.byteOffset, 8).getUint32(4, true) >>> 8 !== first)) {
      o.bad = true;
      return;
    }
    // The block's own index stays lost until every part arrived; any valid part covers the rest.
    p.covered.fill(1, first + 1, end);
    if (o.parts[partIndex]) return;
    o.parts[partIndex] = body;
    o.have += 1;
    if (o.have < o.count || o.bad) return;
    const parts = o.parts as Uint8Array[];
    const joined = new Uint8Array(parts.reduce((a, b) => a + b.byteLength, 0));
    let at = 0;
    for (const part of parts) {
      joined.set(part, at);
      at += part.byteLength;
    }
    const words = (new DataView(joined.buffer).getUint32(0, true) >>> 16) & 0xfff;
    if (words < 2 || words * 4 !== joined.byteLength) return;
    p.covered[first] = 1;
    p.packets.push(joined);
  }
}

/** The block indices of a buffer of whole packets; null when a packet overruns it or is malformed. */
function packetIndices(b: Uint8Array): number[] | null {
  if (b.byteLength & 3) return null;
  const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const out: number[] = [];
  for (let o = 0; o < b.byteLength; ) {
    if (o + 8 > b.byteLength) return null;
    const w0 = v.getUint32(o, true);
    if (w0 >>> 31) {
      o += 8;
      continue;
    }
    const words = (w0 >>> 16) & 0xfff;
    if (words < 2 || o + words * 4 > b.byteLength) return null;
    out.push(v.getUint32(o + 4, true) >>> 8);
    o += words * 4;
  }
  return out;
}
