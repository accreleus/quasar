// PyroWave bitstream v1, 4:2:0 (protocol/codec-pyrowave.md, amendment 55), the CPU side of the
// decoder: no WebGPU here. Ported from the B00 spike (#1049), itself from upstream
// Themaister/pyrowave 7fe0e266 (MIT).

export const LEVELS = 5;

/** offsets[] marker: a received datagram covers the index but carried no block (all zero). */
export const BLOCK_ZERO = 0xffff_ffff;
/** offsets[] marker: no received datagram covers the index; the decoder conceals it. */
export const BLOCK_LOST = 0xffff_fffe;

export type PyroWaveErrorReason =
  | "bad_frame" // malformed input: drop the frame
  | "unsupported" // a configuration this decoder does not carry: use another codec
  | "too_large" // the configuration does not fit this device or the negotiated resolution
  | "no_subgroups" // a forced decoder path needs a feature the device lacks
  | "gpu_error" // WebGPU reported a validation or out-of-memory error
  | "device_lost" // rebuild the device and the decoder
  | "destroyed";

export class PyroWaveError extends Error {
  constructor(
    readonly reason: PyroWaveErrorReason,
    message: string,
  ) {
    super(`PyroWave: ${message}`);
    this.name = "PyroWaveError";
  }
}

export interface PyroWaveHeader {
  width: number;
  height: number;
  /** ycbcr_range: FULL (0) or LIMITED (1). */
  fullRange: boolean;
}

/**
 * The picture fields of a BitstreamSequenceHeader (the PWV1Header's, or a SOF's). Supported: 4:2:0,
 * sRGB primaries and transfer, the BT.709 matrix (amendment 55), even width and height, and centre
 * chroma siting, which amendment 55 does not name yet (erratum item 4, quasar-protocol#29).
 */
function pictureFields(w0: number, w1: number): PyroWaveHeader {
  const bad = (why: string) => new PyroWaveError("unsupported", why);
  const width = (w0 & 0x3fff) + 1;
  const height = ((w0 >>> 14) & 0x3fff) + 1;
  if ((w1 >>> 26) & 1) throw bad("only 4:2:0 chroma is supported");
  if ((w1 >>> 27) & 1) throw bad("only sRGB/BT.709 primaries are supported, not BT.2020");
  if ((w1 >>> 28) & 1) throw bad("only the sRGB transfer is supported, not PQ");
  if ((w1 >>> 29) & 1) throw bad("only the BT.709 matrix is supported");
  if ((w1 >>> 31) & 1) throw bad("only centre chroma siting is supported");
  if ((width | height) & 1) throw bad(`${width}x${height}: 4:2:0 needs an even width and height`);
  return { width, height, fullRange: ((w1 >>> 30) & 1) === 0 };
}

/** Reads a 24-byte PWV1Header: `hello_ack.video.description_b64`, or the start of a .pwv1 file. */
export function parsePwv1Header(bytes: Uint8Array): PyroWaveHeader {
  if (bytes.byteLength < 24) throw new PyroWaveError("bad_frame", "not a PWV1 header: too short");
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  if (dv.getUint32(0, true) !== 0x3156_5750 || bytes[21] !== 1) throw new PyroWaveError("bad_frame", "not a PWV1 header (magic or header_version)");
  return pictureFields(dv.getUint32(4, true), dv.getUint32(8, true));
}

/** Splits upstream's PWV1 disk format (the golden fixtures) into its header and frames. */
export function parsePwv1(file: Uint8Array): { header: PyroWaveHeader; frames: Uint8Array[] } {
  const header = parsePwv1Header(file);
  const dv = new DataView(file.buffer, file.byteOffset, file.byteLength);
  const frames: Uint8Array[] = [];
  for (let o = 24; o + 4 <= file.byteLength; ) {
    const n = dv.getUint32(o, true);
    if (o + 4 + n > file.byteLength) throw new PyroWaveError("bad_frame", "PWV1: truncated frame");
    frames.push(file.subarray(o + 4, o + 4 + n));
    o += 4 + n;
  }
  return { header, frames };
}

export interface Band {
  level: number;
  comp: number;
  band: number;
  /** First block index of this band. */
  blockOffset: number;
  blocksX: number;
  blockCount: number;
  width: number;
  height: number;
  /** Levels 2-4 live in the fp32 coefficient buffer, levels 0-1 in the fp16 one when available. */
  lo: boolean;
  /** Offset in elements into its coefficient buffer. */
  off: number;
}

export interface BlockLayout {
  width: number;
  height: number;
  /** Aligned picture size: a multiple of 32, at least 128. */
  aw: number;
  ah: number;
  /** Coded bands in block-index order. */
  bands: Band[];
  /** N: the number of block indices of a frame. */
  totalBlocks: number;
  /** base[comp][level][band]: storage of every band, including the LL bands the iDWT writes. */
  base: { lo: boolean; off: number }[][][];
  hiWords: number;
  loWords: number;
  /** Decoded Y, Cb, Cr planes, f32, in one buffer. */
  planeOff: [number, number, number];
  planeStride: [number, number, number];
  planeWords: number;
}

/** The band and block-index layout of a 4:2:0 frame: bitstream.md "Block index ordering". */
export function blockLayout(width: number, height: number): BlockLayout {
  const align = (v: number) => Math.max((v + 31) & ~31, 128);
  const aw = align(width);
  const ah = align(height);
  const base: BlockLayout["base"] = [0, 1, 2].map(() => []);
  let hiWords = 0;
  let loWords = 0;
  for (let c = 0; c < 3; c++)
    for (let l = 0; l < LEVELS; l++) {
      base[c][l] = [];
      if (l === 0 && c !== 0) continue;
      const n = (aw >> (l + 1)) * (ah >> (l + 1));
      for (let b = 0; b < 4; b++) {
        const lo = l >= 2;
        base[c][l][b] = { lo, off: lo ? loWords : hiWords };
        if (lo) loWords += n;
        else hiWords += n;
      }
    }
  const bands: Band[] = [];
  let blocks = 0;
  for (let l = LEVELS - 1; l >= 0; l--)
    for (let c = 0; c < 3; c++) {
      if (l === 0 && c !== 0) continue;
      for (let b = l === LEVELS - 1 ? 0 : 1; b < 4; b++) {
        const bw = aw >> (l + 1);
        const bh = ah >> (l + 1);
        const bx = Math.ceil(bw / 32);
        const count = bx * Math.ceil(bh / 32);
        bands.push({ level: l, comp: c, band: b, blockOffset: blocks, blocksX: bx, blockCount: count, width: bw, height: bh, ...base[c][l][b] });
        blocks += count;
      }
    }
  const planeY = aw * ah;
  const planeC = (aw >> 1) * (ah >> 1);
  return {
    width,
    height,
    aw,
    ah,
    bands,
    totalBlocks: blocks,
    base,
    hiWords,
    loWords,
    planeOff: [0, planeY, planeY + planeC],
    planeStride: [aw, aw >> 1, aw >> 1],
    planeWords: planeY + 2 * planeC,
  };
}

export interface Upload {
  /** Per block index: the block's first word in `payload`, BLOCK_ZERO or BLOCK_LOST. */
  offsets: Uint32Array<ArrayBuffer>;
  /** The accepted blocks only: at most one per index, so at most N * 4095 words plus padding. */
  payload: Uint32Array<ArrayBuffer>;
  received: number;
  zero: number;
  lost: number;
}

// A lane reads the word holding its first sign bit and the next. A block whose signs were cut off
// is refused (blockBytes), but should one get through, its reads reach at most 1024 sign bits (32
// words) plus one word past its planes: 34 zero words keep them out of whatever the GPU buffer held.
const PAYLOAD_PAD_WORDS = 34;

const popcount = (v: number) => {
  let n = 0;
  for (; v; v &= v - 1) n++;
  return n;
};

/**
 * The bytes a block needs, by its own header (bitstream.md "Decoding 8x8 blocks"): 8 bytes of
 * header; per set ballot bit a u16 code word and a byte of plane count and scale; per 4x2 subblock
 * its magnitude planes; then one sign bit per nonzero magnitude, as the dequant shader reads them
 * (an 8x8 block whose code word is 0 is all zero, so it has no signs). Infinity when the planes
 * themselves overrun the block.
 */
function blockBytes(dv: DataView, o: number, pw: number): number {
  const size = pw * 4;
  const nset = popcount(dv.getUint16(o, true));
  let at = 8 + 3 * nset;
  if (at > size) return Infinity;
  let signs = 0;
  for (let i = 0; i < nset; i++) {
    const cw = dv.getUint16(o + 8 + 2 * i, true);
    const qb = dv.getUint8(o + 8 + 2 * nset + i) & 0xf;
    for (let sub = 0; sub < 8; sub++) {
      const planes = qb + ((cw >>> (2 * sub)) & 3);
      if (at + planes > size) return Infinity;
      let any = 0; // bit b set: coefficient b of the subblock is nonzero
      for (let q = 0; q < planes; q++) any |= dv.getUint8(o + at + q);
      if (cw !== 0) signs += popcount(any);
      at += planes;
    }
  }
  return at + Math.ceil(signs / 8);
}

/**
 * One frame's GPU upload. `packets` are buffers of whole packets (a datagram's packets, a
 * reassembled oversized block, or a whole library frame); a SOF among them is checked against
 * the configuration and skipped. `covered[i]` is nonzero when a received datagram covers block
 * index i (amendment 55 §Coverage); null means every index is covered. An oversized block's own
 * index stays 0 until every part has arrived: a received part's range covers only the indices
 * after it. A block that arrived and holds what its own header claims is received; a covered
 * index without one is zero; anything else, a short block included, is lost.
 */
export function buildUpload(layout: BlockLayout, packets: readonly Uint8Array[], covered: Uint8Array | null): Upload {
  const n = layout.totalBlocks;
  const bad = (why: string) => new PyroWaveError("bad_frame", why);
  if (covered && covered.length !== n) throw bad(`coverage has ${covered.length} entries, the frame ${n}`);
  const offsets = new Uint32Array(n).fill(BLOCK_LOST);
  const short = new Uint8Array(n); // dropped, so lost even where covered
  const accepted: { dv: DataView; o: number; pw: number }[] = [];
  let words = PAYLOAD_PAD_WORDS;
  for (const p of packets) {
    if (p.byteLength & 3) throw bad("a packet buffer is not whole words");
    const dv = new DataView(p.buffer, p.byteOffset, p.byteLength);
    for (let o = 0; o < p.byteLength; ) {
      const w0 = dv.getUint32(o, true);
      if (w0 >>> 31) {
        if (o + 8 > p.byteLength) throw bad("a truncated start-of-frame header");
        const w1 = dv.getUint32(o + 4, true);
        if ((w1 >>> 24) & 3) throw bad("an unknown extended packet");
        let w: number, h: number;
        try {
          ({ width: w, height: h } = pictureFields(w0, w1));
        } catch (e) {
          throw bad(`a start-of-frame header the configuration does not match: ${(e as Error).message}`);
        }
        if (w !== layout.width || h !== layout.height) throw bad(`the frame is ${w}x${h}, the configuration ${layout.width}x${layout.height}`);
        o += 8;
        continue;
      }
      const pw = (w0 >>> 16) & 0xfff;
      if (pw < 2 || o + pw * 4 > p.byteLength) throw bad(`a bad block at byte ${o}`);
      const index = dv.getUint32(o + 4, true) >>> 8;
      if (index >= n) throw bad(`block index ${index} outside [0, ${n})`);
      if (offsets[index] === BLOCK_LOST) {
        if (blockBytes(dv, o, pw) > pw * 4) short[index] = 1;
        else {
          offsets[index] = words - PAYLOAD_PAD_WORDS;
          accepted.push({ dv, o, pw });
          words += pw;
        }
      }
      o += pw * 4;
    }
  }
  const payload = new Uint32Array(words);
  let w = 0;
  for (const { dv, o, pw } of accepted) for (let i = 0; i < pw; i++) payload[w++] = dv.getUint32(o + 4 * i, true);
  let zero = 0;
  for (let i = 0; i < n; i++)
    if (offsets[i] === BLOCK_LOST && !short[i] && (!covered || covered[i])) {
      offsets[i] = BLOCK_ZERO;
      zero++;
    }
  return { offsets, payload, received: accepted.length, zero, lost: n - accepted.length - zero };
}
