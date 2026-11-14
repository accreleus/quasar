// #1330: a datagram that arrives late is not lost (netem jitter with no rate reorders).
import { describe, expect, it } from "vitest";
import { FeedbackRecorder, HOLD_BACK_MS, MAX_REORDER_MS } from "./feedback";
import { parseVideoDatagram } from "./framing";
import { FrameAssembler, type Reassembled } from "./reassembly";
import { bytes, packetize } from "./testSupport";

/** Each datagram's own extra delay, 0 to `spanMs`, by a hash of its number. */
const spread = (spanMs: number) => (n: number) => ((Math.imul(n, 2_654_435_761) >>> 0) % (spanMs * 1000)) / 1000;
const ONE_WAY_MS = 5;
/** The agent's retransmit hold (`retransmit::WINDOW`). */
const AGENT_HOLD_MS = 250;
const FLAG_RETRANSMIT = 4;
/** quinn acknowledges a packet of the browser's within its 25 ms max_ack_delay. */
const AGENT_ACK_DELAY_MS = 12;

interface Run {
  framesSent: number;
  released: number[];
  /** Longest time between two frames released, after the first. */
  longestGapMs: number;
  keyframeAsks: number;
  nacks: number;
  /** `lost` summed over the feedback messages, and datagrams the path really dropped. */
  reportedLost: number;
  dropped: number;
  /** Datagrams the browser's QUIC threw away for arriving too late (below). */
  discarded: number;
  datagrams: number;
}

/** 60 fps at about 8 Mbit/s for `secs` through the path, with the agent answering NACKs and
 *  keyframe requests; the client is the recorder and the assembler wired as in the media path. */
function run(secs: number, dropEvery: number, reorder: (n: number) => number): Run {
  let t = 0;
  const rec = new FeedbackRecorder(0);
  const asm = new FrameAssembler(() => t, (now) => rec.reorderMs(now));
  const pkt = { n: 0 };
  const sent = new Map<number, { d: Uint8Array; at: number }>();
  /** Arrivals at the client, in time order. */
  const queue: { at: number; d: Uint8Array }[] = [];
  const enqueue = (at: number, d: Uint8Array) => {
    let i = queue.length;
    while (i > 0 && queue[i - 1].at > at) i--;
    queue.splice(i, 0, { at, d });
  };
  const out: Run = { framesSent: 0, released: [], longestGapMs: 0, keyframeAsks: 0, nacks: 0, reportedLost: 0, dropped: 0, discarded: 0, datagrams: 0 };
  // Chrome's QUIC (quiche `PostProcessAfterAckFrame`) stops waiting for every packet below the
  // largest one an ACK of its own named, once the agent has acknowledged that ACK: one arriving
  // later is discarded unread. Its ACK rides each message it sends; the agent's answer comes back
  // over the same reordering path. One datagram per packet, so packet numbers follow pkt_seq.
  let largestIn = -1;
  let awaitedFrom = 0;
  let acks: { at: number; from: number }[] = [];
  let ackNo = 1 << 30;
  const browserSent = () => {
    if (largestIn >= 0) acks.push({ at: t + ONE_WAY_MS + AGENT_ACK_DELAY_MS + ONE_WAY_MS + reorder(ackNo++), from: largestIn + 1 });
  };
  let lastRelease: number | null = null;
  let wantKey = false;
  const handle = (r: Reassembled) => {
    for (const u of r.units) {
      out.released.push(u.frameSeq);
      if (lastRelease !== null) out.longestGapMs = Math.max(out.longestGapMs, t - lastRelease);
      lastRelease = t;
    }
    if (r.needKeyframe) {
      out.keyframeAsks++;
      wantKey = true;
    }
    for (const n of r.nacks) {
      out.nacks++;
      browserSent();
      const reach = t + ONE_WAY_MS;
      for (const [a, b] of n.frags) {
        for (let i = a; i <= b; i++) {
          const s = sent.get(n.frameSeq * 65_536 + i);
          if (!s) continue;
          if (reach - s.at > AGENT_HOLD_MS) {
            handle(asm.miss(n.frameSeq, [[i, i]]));
            continue;
          }
          // A resend is a new datagram: its own pkt_seq.
          const d = s.d.slice();
          d[5] |= FLAG_RETRANSMIT;
          new DataView(d.buffer).setUint32(1, pkt.n, true);
          enqueue(reach + ONE_WAY_MS + reorder(pkt.n++), d);
        }
      }
    }
  };
  const end = secs * 1000;
  let nextFrame = 0;
  let nextFeedback = 50;
  for (let frame = 0; t <= end + 500; ) {
    const due = Math.min(nextFrame <= end ? nextFrame : Infinity, nextFeedback, queue[0]?.at ?? Infinity, Math.floor(t) + 1);
    t = due;
    if (queue.length && queue[0].at <= t) {
      const { d } = queue.shift()!;
      for (const a of acks) if (a.at <= t) awaitedFrom = Math.max(awaitedFrom, a.from);
      acks = acks.filter((a) => a.at > t);
      const n = new DataView(d.buffer).getUint32(1, true);
      if (n < awaitedFrom) {
        out.discarded++;
        continue;
      }
      largestIn = Math.max(largestIn, n);
      rec.record(d, t);
      handle(asm.push(parseVideoDatagram(d)!));
    } else if (t >= nextFeedback) {
      const m = rec.take(t);
      out.reportedLost += m?.lost ?? 0;
      if (m) browserSent();
      nextFeedback += 50;
    } else if (t >= nextFrame && nextFrame <= end) {
      const key = frame === 0 || wantKey;
      wantKey = false;
      const ds = packetize({ configId: 0, frameSeq: frame & 0xffff, keyframe: key, data: bytes(key ? 60_000 : 16_000, frame) }, pkt);
      ds.forEach((d, i) => {
        const at = t + i * 0.1;
        sent.set((frame & 0xffff) * 65_536 + i, { d, at });
        const n = new DataView(d.buffer).getUint32(1, true);
        out.datagrams++;
        if (dropEvery && n % dropEvery === dropEvery - 1) out.dropped++;
        else enqueue(at + ONE_WAY_MS + reorder(n), d);
      });
      frame++;
      out.framesSent = frame;
      nextFrame += 1000 / 60;
    } else handle(asm.tick());
  }
  return out;
}

describe("a path that holds each datagram on its own (#1330)", () => {
  it("0–20 ms: reports only the real loss, where every reordered datagram used to count lost", () => {
    const r = run(30, 1000, spread(20));
    expect(r.discarded).toBe(0);
    expect(r.reportedLost).toBe(r.dropped);
  });

  it("0–40 ms: every frame comes, no keyframe is asked for, and the loss reported is what never arrived", () => {
    const r = run(30, 1000, spread(40));
    expect(r.keyframeAsks).toBe(0);
    expect(r.released.length).toBe(r.framesSent);
    // Frames are released in order, so one held for its slowest fragment holds those behind it.
    expect(r.longestGapMs).toBeLessThan(200);
    expect(r.reportedLost).toBe(r.dropped + r.discarded);
    expect(r.reportedLost).toBeLessThan(0.01 * r.datagrams);
  });

  it("known limit, 0–80 ms: the browser's QUIC discards a quarter of what it receives, and that is reported as the loss it is", () => {
    const r = run(30, 1000, spread(80));
    expect(r.discarded).toBeGreaterThan(0.15 * r.datagrams);
    expect(r.reportedLost).toBe(r.dropped + r.discarded);
  });

  it("still reports 5 % real loss as loss, within the reorder window", () => {
    const rec = new FeedbackRecorder(0);
    let lost = 0;
    let dropped = 0;
    const arrivals: [number, number][] = [];
    // The last one arrives: a loss shows only behind a later arrival.
    for (let n = 0; n <= 20_000; n++) {
      if (n % 20 === 19) dropped++;
      else arrivals.push([n * 0.5 + spread(80)(n), n]);
    }
    arrivals.sort((a, b) => a[0] - b[0]);
    const d = new Uint8Array(100);
    d[0] = 0x01;
    let i = 0;
    const lastSent = 20_000 * 0.5;
    for (let t = 50; t <= lastSent + 80 + MAX_REORDER_MS + HOLD_BACK_MS + 50; t += 50) {
      for (; i < arrivals.length && arrivals[i][0] <= t; i++) {
        new DataView(d.buffer).setUint32(1, arrivals[i][1], true);
        rec.record(d, arrivals[i][0]);
      }
      const m = rec.take(t);
      lost += m?.lost ?? 0;
      if (t > lastSent + 80 + MAX_REORDER_MS + HOLD_BACK_MS) expect(m === null || m.first_pkt_seq + m.count === 20_001).toBe(true);
    }
    expect(lost).toBeGreaterThanOrEqual(dropped);
    expect(lost).toBeLessThan(dropped * 1.05);
  });

  it("forgets the reordering once it stops, back to the 5 ms hold of a path that does not reorder", () => {
    const rec = new FeedbackRecorder(0);
    const d = (n: number) => {
      const b = new Uint8Array(10);
      b[0] = 0x01;
      new DataView(b.buffer).setUint32(1, n, true);
      return b;
    };
    rec.record(d(0), 0);
    rec.record(d(2), 0);
    rec.record(d(1), 80);
    expect(rec.reorderMs(80)).toBe(80);
    expect(rec.reorderMs(2_080)).toBe(40);
    expect(rec.reorderMs(20_080)).toBeLessThan(0.1);
  });
});
