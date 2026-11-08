import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { BLOCK_LOST, BLOCK_ZERO, blockLayout, buildUpload, parsePwv1 } from "./bitstream";
import { PyroWaveReceiver, type PyroWaveFrame } from "./receiver";
import { parseVideoDatagram, type VideoFragment } from "../webtransport/framing";
import { packPyroWave, pyroWaveDatagrams } from "../webtransport/testSupport";

const fixture = (name: string) => new Uint8Array(readFileSync(resolve(__dirname, "../../../testdata/pyrowave", name)));
const { header, frames } = parsePwv1(fixture("detail_320x192_xhi.pwv1"));
const layout = blockLayout(header.width, header.height);
const N = layout.totalBlocks;
const FRAME_MS = 1000 / 60;

/** A ballot-0 block: consistent at any payload_words >= 2. */
function block(index: number, words: number): Uint8Array {
  const b = new Uint8Array(words * 4).fill(0xa0);
  const v = new DataView(b.buffer);
  v.setUint32(0, words << 16, true);
  v.setUint32(4, index << 8, true);
  return b;
}
function synthetic(blocks: [number, number][]): Uint8Array {
  const parts = [new Uint8Array(8), ...blocks.map(([i, w]) => block(i, w))];
  new DataView(parts[0].buffer).setUint32(0, 0x8000_0000 | ((header.height - 1) << 14) | (header.width - 1), true);
  const out = new Uint8Array(parts.reduce((a, p) => a + p.length, 0));
  let at = 0;
  for (const p of parts) (out.set(p, at), (at += p.length));
  return out;
}

/** The fixture frame (or `frame`) as parsed video fragments of `frameSeq`, packed into `room`. */
function fragments(frameSeq: number, frame = frames[0], room?: number): VideoFragment[] {
  return pyroWaveDatagrams({ configId: 3, frameSeq }, packPyroWave(frame, N, room)).map((d) => parseVideoDatagram(d) as VideoFragment);
}
const range = (f: VideoFragment) => {
  const v = new DataView(f.payload.buffer, f.payload.byteOffset);
  return [v.getUint32(4, true), v.getUint32(8, true)] as const;
};
function feed(r: PyroWaveReceiver, frags: VideoFragment[], now = 0): PyroWaveFrame[] {
  const out: PyroWaveFrame[] = [];
  for (const f of frags) {
    const done = r.push(f, now);
    if (done) out.push(done);
  }
  return out;
}
const upload = (f: PyroWaveFrame) => buildUpload(layout, f.packets, f.covered);
const whole = buildUpload(layout, [frames[0]], null);

describe("PyroWaveReceiver", () => {
  it("a whole frame decodes exactly as the library's frame", () => {
    const frags = fragments(1);
    expect(frags.length).toBeGreaterThan(3);
    const [frame] = feed(new PyroWaveReceiver(3, N, FRAME_MS), frags);
    expect(frame.complete).toBe(true);
    expect(frame.covered.every((c) => c === 1)).toBe(true);
    const up = upload(frame);
    expect([up.received, up.zero, up.lost]).toEqual([whole.received, whole.zero, 0]);
    expect(up.offsets.map((o) => (o === BLOCK_ZERO ? 0 : 1))).toEqual(whole.offsets.map((o) => (o === BLOCK_ZERO ? 0 : 1)));
  });

  it("datagrams in any order make the same frame", () => {
    const frags = fragments(1);
    const shuffled = frags.map((f, i) => [f, (i * 7919) % frags.length] as const).sort((a, b) => a[1] - b[1]).map(([f]) => f);
    const [frame] = feed(new PyroWaveReceiver(3, N, FRAME_MS), shuffled);
    expect(upload(frame).lost).toBe(0);
    expect(upload(frame).received).toBe(whole.received);
  });

  it("a lost datagram conceals exactly its range, and only at the frame's deadline", () => {
    const frags = fragments(1);
    const gone = frags.find((f, i) => i > 0 && f.payload[0] === 0 && range(f)[0] < range(f)[1]) as VideoFragment;
    const [first, end] = range(gone);
    const r = new PyroWaveReceiver(3, N, FRAME_MS);
    expect(feed(r, frags.filter((f) => f !== gone), 0)).toEqual([]);
    expect(r.nextDeadline()).toBeCloseTo(FRAME_MS);
    expect(r.due(FRAME_MS - 1)).toBeNull();
    const frame = r.due(FRAME_MS) as PyroWaveFrame;
    expect(frame.complete).toBe(false);
    const up = upload(frame);
    const lost = [...up.offsets.keys()].filter((i) => up.offsets[i] === BLOCK_LOST);
    expect(lost).toEqual(Array.from({ length: end - first }, (_, k) => first + k));
    expect(up.received + up.zero + up.lost).toBe(N);
    expect(r.nextDeadline()).toBeNull();
  });

  it("the fixture's real oversized blocks rejoin, and a lost part loses only its block", () => {
    const frags = fragments(1);
    const parts = frags.filter((f) => f.payload[0] === 1);
    expect(parts.length).toBeGreaterThan(1);
    const gone = parts[0];
    const [own, end] = range(gone);
    const r = new PyroWaveReceiver(3, N, FRAME_MS);
    feed(r, frags.filter((f) => f !== gone));
    const up = upload(r.due(FRAME_MS) as PyroWaveFrame);
    expect(up.offsets[own]).toBe(BLOCK_LOST);
    expect(up.lost).toBe(1);
    for (let i = own + 1; i < end; i++) expect(up.offsets[i]).not.toBe(BLOCK_LOST);
  });

  it("a late datagram of a decoded frame, or a duplicate, is ignored; a newer frame drops an older pending one", () => {
    const r = new PyroWaveReceiver(3, N, FRAME_MS);
    const one = fragments(1);
    const two = fragments(2);
    // Frame 1 short one datagram; frame 2 arrives whole and wins.
    feed(r, one.slice(1));
    const [done] = feed(r, two);
    expect(done.frameSeq).toBe(2);
    expect(r.nextDeadline()).toBeNull();
    expect(r.push(one[0], 1), "the late datagram completes nothing").toBeNull();
    expect(r.push(two[0], 1), "a duplicate of a decoded frame").toBeNull();
    expect(r.due(100)).toBeNull();
    expect(feed(r, fragments(3))[0].frameSeq).toBe(3);
  });

  it("frames interleaved across wrap-around come out in frame_seq order", () => {
    const r = new PyroWaveReceiver(3, N, FRAME_MS);
    const a = fragments(0xffff);
    const b = fragments(0);
    const out = feed(r, [...a.slice(0, -1), ...b.slice(0, 2), a[a.length - 1], ...b.slice(2)]);
    expect(out.map((f) => f.frameSeq)).toEqual([0xffff, 0]);
  });

  describe("oversized blocks", () => {
    // SOF + block 2 in the first datagram; block 5 in three parts; block 6 last.
    const frame = synthetic([
      [2, 10],
      [5, 250],
      [6, 10],
    ]);
    const frags = () => fragments(1, frame, 12 + 400);

    it("rejoined from every part, the block is received", () => {
      expect(frags().length).toBe(5);
      const [f] = feed(new PyroWaveReceiver(3, N, FRAME_MS), frags());
      const up = upload(f);
      expect([up.offsets[5] !== BLOCK_LOST && up.offsets[5] !== BLOCK_ZERO, up.lost]).toEqual([true, 0]);
    });

    // #1057's review note: a partial block's own index is lost, never covered by its parts.
    it("with a part missing, its own index is lost and the rest of its range is zero", () => {
      const r = new PyroWaveReceiver(3, N, FRAME_MS);
      const all = frags();
      feed(r, all.filter((_, i) => i !== 2));
      const f = r.due(FRAME_MS) as PyroWaveFrame;
      expect(f.covered[5]).toBe(0);
      const up = upload(f);
      expect(up.offsets[5]).toBe(BLOCK_LOST);
      expect(up.lost).toBe(1);
      expect(up.offsets[2]).not.toBe(BLOCK_LOST);
      expect(up.offsets[6]).not.toBe(BLOCK_LOST);
    });

    it("parts that disagree lose the block", () => {
      const all = frags();
      const bad = new Uint8Array(all[3].payload);
      bad[2] = 4; // part_count
      const r = new PyroWaveReceiver(3, N, FRAME_MS);
      const [f] = feed(r, [...all.slice(0, 3), { ...all[3], payload: bad }, all[4]]);
      expect(upload(f).offsets[5]).toBe(BLOCK_LOST);
    });

    // N2: an invalid part 0 marks no coverage, so the zeros after its block read lost.
    it("a part 0 naming another block covers nothing", () => {
      const gap = synthetic([
        [2, 10],
        [5, 250],
        [9, 10],
      ]);
      const all = fragments(1, gap, 12 + 400);
      expect(range(all[1])).toEqual([5, 9]);
      const bad = new Uint8Array(all[1].payload);
      new DataView(bad.buffer).setUint32(12 + 4, 7 << 8, true); // the block header's index
      const r = new PyroWaveReceiver(3, N, FRAME_MS);
      feed(r, [all[0], { ...all[1], payload: bad }, all[4]]);
      const up = upload(r.due(FRAME_MS) as PyroWaveFrame);
      for (const i of [5, 6, 7, 8]) expect(up.offsets[i]).toBe(BLOCK_LOST);
      expect(up.offsets[9]).not.toBe(BLOCK_LOST);
    });

    it("block 0 oversized leaves datagram 0 with the SOF alone and an empty range", () => {
      const f0 = synthetic([[0, 250]]);
      const fr = fragments(1, f0, 12 + 400);
      expect(range(fr[0])).toEqual([0, 0]);
      const [f] = feed(new PyroWaveReceiver(3, N, FRAME_MS), fr);
      expect(upload(f).lost).toBe(0);
    });
  });

  describe("validation: a dropped datagram counts as lost", () => {
    const frame = synthetic([
      [1, 100],
      [40, 100],
      [90, 100],
    ]);
    const tamper = (i: number, edit: (p: Uint8Array) => void) => {
      const all = fragments(1, frame, 12 + 420);
      const p = new Uint8Array(all[i].payload);
      edit(p);
      all[i] = { ...all[i], payload: p };
      const [f] = feed(new PyroWaveReceiver(3, N, FRAME_MS), all);
      return upload(f);
    };

    it.each([
      ["an unknown kind", (p: Uint8Array) => (p[0] = 2)],
      ["a range past N", (p: Uint8Array) => new DataView(p.buffer).setUint32(8, N + 1, true)],
      ["a block outside its range", (p: Uint8Array) => new DataView(p.buffer).setUint32(4, 41, true)],
      ["an empty range that is not the SOF datagram", (p: Uint8Array) => new DataView(p.buffer).setUint32(8, 40, true)],
      ["a packet overrunning the payload", (p: Uint8Array) => new DataView(p.buffer).setUint32(12, 200 << 16, true)],
    ])("%s", (_, edit) => {
      const up = tamper(1, edit);
      expect(up.offsets[40]).toBe(BLOCK_LOST);
      expect(up.offsets[1]).not.toBe(BLOCK_LOST);
      expect(up.offsets[90]).not.toBe(BLOCK_LOST);
    });
  });
});
