import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { BLOCK_LOST, BLOCK_ZERO, PyroWaveError, blockLayout, buildUpload, parsePwv1, parsePwv1Header } from "./bitstream";

const fixture = (name: string) => new Uint8Array(readFileSync(resolve(__dirname, "../../../testdata/pyrowave", name)));

// A block packet: header words (payload_words, ballot, block_index) then filler words. Ballot 0
// (no 8x8 block set) needs nothing past the header, so any payload_words >= 2 is consistent.
function block(index: number, payloadWords: number, ballot = 0, fill = 0xa0): Uint8Array {
  const b = new Uint8Array(Math.max(payloadWords, 2) * 4).fill(fill);
  const dv = new DataView(b.buffer);
  dv.setUint32(0, (payloadWords << 16) | ballot, true);
  dv.setUint32(4, (index << 8) | 0x11, true);
  return b;
}
const sof = () => {
  const b = new Uint8Array(8);
  new DataView(b.buffer).setUint32(0, 0x8000_0000 | (113 << 14) | 199, true);
  return b;
};
const concat = (...parts: Uint8Array[]) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) (out.set(p, o), (o += p.length));
  return out;
};

describe("parsePwv1Header", () => {
  it("reads the picture and colour fields of a fixture's PWV1 header", () => {
    expect(parsePwv1Header(fixture("detail_200x114_hi.pwv1"))).toEqual({
      width: 200,
      height: 114,
      fullRange: false,
    });
  });

  const reasonOf = (f: () => unknown) => {
    try {
      f();
    } catch (e) {
      expect(e).toBeInstanceOf(PyroWaveError);
      return (e as PyroWaveError).reason;
    }
    throw new Error("did not throw");
  };

  it("refuses a buffer that is not a v1 PWV1 header as a bad frame", () => {
    expect(() => parsePwv1Header(new Uint8Array(24))).toThrow(/PWV1/);
    expect(reasonOf(() => parsePwv1Header(new Uint8Array(24)))).toBe("bad_frame");
    expect(reasonOf(() => parsePwv1Header(new Uint8Array(10)))).toBe("bad_frame");
  });

  // Bits of the header's second word: 26 chroma_resolution, 27 color_primaries, 28 transfer_function,
  // 29 ycbcr_transform, 31 chroma_siting.
  it.each([
    [26, /4:2:0/],
    [27, /BT.2020/],
    [28, /PQ/],
    [29, /BT.709 matrix/],
    [31, /centre chroma siting/],
  ])("refuses bit %i, which amendment 55 does not carry", (bit, message) => {
    const h = fixture("flat_200x114_lo.pwv1").slice(0, 24);
    const dv = new DataView(h.buffer);
    dv.setUint32(8, dv.getUint32(8, true) | (1 << bit), true);
    expect(() => parsePwv1Header(h)).toThrow(message);
    expect(reasonOf(() => parsePwv1Header(h))).toBe("unsupported");
  });

  it.each([
    [201, 114],
    [200, 115],
  ])("refuses an odd size, %ix%i", (w, hh) => {
    const h = fixture("flat_200x114_lo.pwv1").slice(0, 24);
    const dv = new DataView(h.buffer);
    dv.setUint32(4, (dv.getUint32(4, true) & ~0x0fff_ffff) | ((hh - 1) << 14) | (w - 1), true);
    expect(() => parsePwv1Header(h)).toThrow(/even/);
    expect(reasonOf(() => parsePwv1Header(h))).toBe("unsupported");
  });
});

describe("parsePwv1", () => {
  it("splits a fixture file into its header and its one frame", () => {
    const { header, frames } = parsePwv1(fixture("flat_320x192_lo.pwv1"));
    expect(header.width).toBe(320);
    expect(frames).toHaveLength(1);
    expect(frames[0].byteLength).toBe(6128); // manifest frame_bytes
  });
});

describe("blockLayout", () => {
  // N from bitstream.md "Block index ordering", worked by hand for 4:2:0.
  it.each([
    [200, 114, 72],
    [320, 192, 138],
    [1920, 1080, 3261],
  ])("%ix%i has %i block indices", (w, h, n) => {
    expect(blockLayout(w, h).totalBlocks).toBe(n);
  });
});

describe("buildUpload", () => {
  const layout = blockLayout(200, 114); // N = 72

  it("a complete library frame: every sent block is received, every other index is zero", () => {
    const frame = parsePwv1(fixture("detail_200x114_hi.pwv1")).frames[0];
    const up = buildUpload(layout, [frame], null);
    expect(up.received).toBe(68); // manifest blocks
    expect(up.zero).toBe(4);
    expect(up.lost).toBe(0);
  });

  it("points each received block at its words, marks covered gaps zero and uncovered indices lost", () => {
    const covered = new Uint8Array(72);
    covered.fill(1, 0, 10); // a datagram covering [0, 10) arrived; nothing else did
    const b3 = block(3, 4, 0, 0x33);
    const b7 = block(7, 2, 0, 0x77);
    const up = buildUpload(layout, [concat(sof(), b3, b7)], covered);

    expect(up.offsets[3]).not.toBe(BLOCK_ZERO);
    expect(up.offsets[3]).not.toBe(BLOCK_LOST);
    expect(new Uint8Array(up.payload.buffer, up.offsets[3] * 4, 16)).toEqual(b3);
    expect(new Uint8Array(up.payload.buffer, up.offsets[7] * 4, 8)).toEqual(b7);
    expect(up.offsets[0]).toBe(BLOCK_ZERO);
    expect(up.offsets[9]).toBe(BLOCK_ZERO);
    expect(up.offsets[10]).toBe(BLOCK_LOST);
    expect(up.offsets[71]).toBe(BLOCK_LOST);
    expect([up.received, up.zero, up.lost]).toEqual([2, 8, 62]);
  });

  it("takes blocks from several datagrams, and a block that arrived wins over a missing range", () => {
    const covered = new Uint8Array(72); // the oversized block 5's other part was lost
    const up = buildUpload(layout, [block(5, 3), block(70, 2)], covered);
    expect(up.offsets[5]).not.toBe(BLOCK_LOST);
    expect(up.offsets[70]).not.toBe(BLOCK_LOST);
    expect(up.received).toBe(2);
  });

  it("keeps the first copy of a block sent twice", () => {
    const up = buildUpload(layout, [block(4, 3, 0, 0x01), block(4, 3, 0, 0x02)], null);
    expect(new Uint8Array(up.payload.buffer, up.offsets[4] * 4 + 8, 1)[0]).toBe(0x01);
    expect(up.received).toBe(1);
  });

  it("pads the payload so the sign-bit read past the last block stays in bounds", () => {
    const up = buildUpload(layout, [block(0, 2)], null);
    expect(up.payload.length).toBeGreaterThanOrEqual(2 + 2);
  });

  it.each([
    ["a block index past N", [block(72, 2)]],
    ["payload_words below 2", [block(1, 1)]],
    ["a packet that overruns its buffer", [block(1, 4).subarray(0, 12)]],
    ["a buffer that is not whole words", [new Uint8Array(6)]],
  ])("refuses %s", (_name, packets) => {
    expect(() => buildUpload(layout, packets, null)).toThrow();
  });

  it("a block shorter than its own header claims is lost, even where covered; the next block is received", () => {
    const short = block(2, 2, 0xffff); // 16 code words announced, none present
    const planes = block(3, 4, 1, 0xff); // one 8x8 block, cw 0xffff, 15 base planes: ~155 bytes, has 16
    const up = buildUpload(layout, [concat(short, planes, block(4, 2))], new Uint8Array(72).fill(1));
    expect(up.offsets[2]).toBe(BLOCK_LOST);
    expect(up.offsets[3]).toBe(BLOCK_LOST);
    expect(up.offsets[4]).not.toBe(BLOCK_LOST);
    expect(up.offsets[4]).not.toBe(BLOCK_ZERO);
    expect([up.received, up.zero, up.lost]).toEqual([1, 69, 2]);
  });

  it("accepts a block that holds exactly what its header claims", () => {
    // ballot 1 -> one cw + one qb byte; cw 0x0001 (sub 0 has 1 extra plane), qb 0: 8 + 3 + 1 bytes.
    const b = block(5, 3, 1, 0);
    new DataView(b.buffer).setUint16(8, 1, true);
    expect(buildUpload(layout, [b], null).offsets[5]).not.toBe(BLOCK_LOST);
    new DataView(b.buffer).setUint16(8, 0xffff, true); // now 8 + 3 + 24 bytes: too short
    expect(buildUpload(layout, [b], null).offsets[5]).toBe(BLOCK_LOST);
  });

  it("a block whose sign bits were cut off is lost; the same block with them is received", () => {
    // One 8x8 block, cw 0x5555 (one plane per 4x2 subblock), qb 0, every plane 0xff: 64 nonzero
    // magnitudes, so 8 + 3 + 8 bytes before the signs and 8 bytes of signs: 27 bytes, 7 words.
    const b = (pw: number) => {
      const x = block(6, pw, 1, 0);
      new DataView(x.buffer).setUint16(8, 0x5555, true);
      x.fill(0xff, 11, 19);
      return x;
    };
    expect(buildUpload(layout, [b(7)], null).offsets[6]).not.toBe(BLOCK_LOST);
    expect(buildUpload(layout, [b(6)], null).offsets[6]).toBe(BLOCK_LOST); // 24 bytes: 5 sign bytes
    expect(buildUpload(layout, [b(5)], null).offsets[6]).toBe(BLOCK_LOST); // planes fit, no signs
  });

  it("sizes the payload from the accepted blocks, not from what arrived", () => {
    const dup = Array.from({ length: 1000 }, () => block(3, 100));
    expect(buildUpload(layout, dup, null).payload.length).toBeLessThanOrEqual(100 + 34);
  });

  it("refuses a start-of-frame header whose size differs from the configuration", () => {
    const frame = parsePwv1(fixture("detail_200x114_hi.pwv1")).frames[0].slice();
    expect(() => buildUpload(blockLayout(320, 192), [frame], null)).toThrow(PyroWaveError);
    const dv = new DataView(frame.buffer);
    dv.setUint32(0, (dv.getUint32(0, true) & ~0x3fff) | 199, true); // width 200 -> 200 still
    expect(buildUpload(layout, [frame], null).received).toBe(68);
    dv.setUint32(0, (dv.getUint32(0, true) & ~0x3fff) | 255, true); // width 256
    expect(() => buildUpload(layout, [frame], null)).toThrow(/256x114/);
  });

  it.each([
    [26, /4:2:0/],
    [31, /centre chroma siting/],
  ])("refuses a start-of-frame header with bit %i set, whatever the configuration said", (bit, message) => {
    const frame = parsePwv1(fixture("detail_200x114_hi.pwv1")).frames[0].slice();
    const dv = new DataView(frame.buffer);
    dv.setUint32(4, dv.getUint32(4, true) | (1 << bit), true);
    expect(() => buildUpload(layout, [frame], null)).toThrow(message);
    expect(() => buildUpload(layout, [frame], null)).toThrow(expect.objectContaining({ reason: "bad_frame" }));
  });

  it("an oversized block with a part missing is lost, while the indices its received part covers are zero", () => {
    // Block 5 was sent in two parts carrying the range [5, 9); part 1 never arrived. The receiver
    // marks 6..8 covered (part 0 arrived) and leaves 5 uncovered (amendment 55 §Coverage).
    const covered = new Uint8Array(72).fill(1);
    covered[5] = 0;
    const up = buildUpload(layout, [], covered);
    expect(up.offsets[5]).toBe(BLOCK_LOST);
    expect([6, 7, 8].map((i) => up.offsets[i])).toEqual([BLOCK_ZERO, BLOCK_ZERO, BLOCK_ZERO]);
    expect([up.received, up.zero, up.lost]).toEqual([0, 71, 1]);
  });

  it("refuses a coverage map of the wrong size", () => {
    expect(() => buildUpload(layout, [], new Uint8Array(71))).toThrow(/coverage/);
  });
});
