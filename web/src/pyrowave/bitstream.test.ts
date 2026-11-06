import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { BLOCK_LOST, BLOCK_ZERO, blockLayout, buildUpload, parsePwv1, parsePwv1Header } from "./bitstream";

const fixture = (name: string) => new Uint8Array(readFileSync(resolve(__dirname, "../../../testdata/pyrowave", name)));

// A block packet: header words (payload_words, ballot, block_index) then filler words.
function block(index: number, payloadWords: number, ballot = 1, fill = 0xa0): Uint8Array {
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

  it("refuses a buffer that is not a v1 PWV1 header", () => {
    expect(() => parsePwv1Header(new Uint8Array(24))).toThrow(/PWV1/);
    expect(() => parsePwv1Header(new Uint8Array(10))).toThrow(/PWV1/);
  });

  it("refuses 4:4:4 and BT.2020, which amendment 55 does not carry", () => {
    const h = fixture("flat_200x114_lo.pwv1").slice(0, 24);
    const dv = new DataView(h.buffer);
    const w1 = dv.getUint32(8, true);
    dv.setUint32(8, w1 | (1 << 26), true);
    expect(() => parsePwv1Header(h)).toThrow(/4:2:0/);
    dv.setUint32(8, w1 | (1 << 29), true);
    expect(() => parsePwv1Header(h)).toThrow(/BT.709/);
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
    const b3 = block(3, 4, 1, 0x33);
    const b7 = block(7, 2, 1, 0x77);
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
    const up = buildUpload(layout, [block(4, 3, 1, 0x01), block(4, 3, 1, 0x02)], null);
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

  it("refuses a coverage map of the wrong size", () => {
    expect(() => buildUpload(layout, [], new Uint8Array(71))).toThrow(/coverage/);
  });
});
