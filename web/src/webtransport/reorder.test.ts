// #1330: a datagram that arrives late is not lost (netem jitter with no rate reorders).
import { describe, expect, it } from "vitest";
import { FeedbackRecorder, HOLD_BACK_MS, REORDER_HISTORY_MS, REORDER_MIN_SAMPLES, type FeedbackMessage } from "./feedback";
import { parseVideoDatagram } from "./framing";
import { FrameAssembler, type Reassembled } from "./reassembly";
import { bytes, packetize } from "./testSupport";

/** Each datagram's own extra delay, 0 to `spanMs`, by a hash of its number. */
const spread = (spanMs: number) => (n: number) => (spanMs ? ((Math.imul(n, 2_654_435_761) >>> 0) % (spanMs * 1000)) / 1000 : 0);
const ONE_WAY_MS = 5;
/** The agent's retransmit hold (`retransmit::WINDOW`). */
const AGENT_HOLD_MS = 250;
const FLAG_RETRANSMIT = 4;
/** quinn acknowledges a packet of the browser's within its 25 ms max_ack_delay. */
const AGENT_ACK_DELAY_MS = 12;
/** What reordering costs before the window is measured (no rate before the first message, fewer
 *  than REORDER_MIN_SAMPLES measurements): read as loss, as before #1330. */
const WARM_UP = 60;

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
  /** Where the recorder and assembler did not do what the pre-#1330 ones did (`baseline` runs). */
  differences: string[];
}

/** 60 fps at about 8 Mbit/s for `secs` through the path, with the agent answering NACKs and
 *  keyframe requests; the client is the recorder and the assembler wired as in the media path.
 *  With `baseline`, every message and every NACK is also checked against the pre-#1330 rules: a
 *  message reports up to the newest datagram {@link HOLD_BACK_MS} old, and an assembler with no
 *  reorder window NACKs the same fragments at the same instants. */
function run(secs: number, dropEvery: number, reorder: (n: number) => number, baseline = false): Run {
  let t = 0;
  const rec = new FeedbackRecorder(0);
  const asm = new FrameAssembler(() => t, (now) => rec.reorderMs(now));
  const base = new FrameAssembler(() => t);
  const pkt = { n: 0 };
  const sent = new Map<number, { d: Uint8Array; at: number }>();
  /** Datagrams arriving at the client and NACKs arriving at the agent, in time order. */
  const queue: { at: number; d?: Uint8Array; agent?: () => void }[] = [];
  const enqueue = (e: (typeof queue)[number]) => {
    let i = queue.length;
    while (i > 0 && queue[i - 1].at > e.at) i--;
    queue.splice(i, 0, e);
  };
  const out: Run = {
    framesSent: 0, released: [], longestGapMs: 0, keyframeAsks: 0, nacks: 0, reportedLost: 0, dropped: 0, discarded: 0, datagrams: 0, differences: [],
  };
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
  /** Baseline bookkeeping: every pkt_seq that reached the recorder, and when. */
  const seen = new Map<number, number>();
  let reportedTo = 0;
  let lastRelease: number | null = null;
  let wantKey = false;
  const handle = (r: Reassembled, b: Reassembled) => {
    if (baseline && JSON.stringify(r.nacks) !== JSON.stringify(b.nacks)) out.differences.push(`t=${t} nacks ${JSON.stringify(r.nacks)} vs ${JSON.stringify(b.nacks)}`);
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
      enqueue({
        at: t + ONE_WAY_MS,
        agent: () => {
          for (const [a, z] of n.frags) {
            for (let i = a; i <= z; i++) {
              const s = sent.get(n.frameSeq * 65_536 + i);
              if (!s) continue;
              if (t - s.at > AGENT_HOLD_MS) {
                handle(asm.miss(n.frameSeq, [[i, i]]), base.miss(n.frameSeq, [[i, i]]));
                continue;
              }
              // A resend is a new datagram: its own pkt_seq, sent now.
              const d = s.d.slice();
              d[5] |= FLAG_RETRANSMIT;
              new DataView(d.buffer).setUint32(1, pkt.n, true);
              enqueue({ at: t + ONE_WAY_MS + reorder(pkt.n++), d });
            }
          }
        },
      });
    }
  };
  const take = () => {
    const m = rec.take(t);
    out.reportedLost += m?.lost ?? 0;
    if (m) browserSent();
    if (!baseline) return;
    let last = -1;
    for (const [s, at] of seen) if (s >= reportedTo && at <= t - HOLD_BACK_MS) last = Math.max(last, s);
    const want = last < 0 ? null : { first: reportedTo, count: last - reportedTo + 1 };
    const got = m && { first: m.first_pkt_seq, count: m.count };
    if (JSON.stringify(got) !== JSON.stringify(want)) out.differences.push(`t=${t} message ${JSON.stringify(got)} vs ${JSON.stringify(want)}`);
    if (m) reportedTo = m.first_pkt_seq + m.count;
  };
  const end = secs * 1000;
  let nextFrame = 0;
  let nextFeedback = 50;
  for (let frame = 0; t <= end + 500; ) {
    t = Math.min(nextFrame <= end ? nextFrame : Infinity, nextFeedback, queue[0]?.at ?? Infinity, Math.floor(t) + 1);
    if (queue.length && queue[0].at <= t) {
      const e = queue.shift()!;
      if (e.agent) {
        e.agent();
        continue;
      }
      const d = e.d!;
      for (const a of acks) if (a.at <= t) awaitedFrom = Math.max(awaitedFrom, a.from);
      acks = acks.filter((a) => a.at > t);
      const n = new DataView(d.buffer).getUint32(1, true);
      if (n < awaitedFrom) {
        out.discarded++;
        continue;
      }
      largestIn = Math.max(largestIn, n);
      rec.record(d, t);
      seen.set(n, t);
      const f = parseVideoDatagram(d)!;
      handle(asm.push(f), base.push(f));
    } else if (t >= nextFeedback) {
      take();
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
        else enqueue({ at: at + ONE_WAY_MS + reorder(n), d });
      });
      frame++;
      out.framesSent = frame;
      nextFrame += 1000 / 60;
    } else handle(asm.tick(), base.tick());
  }
  return out;
}

/** A datagram on the video channel carrying `pktSeq`. */
const dgram = (pktSeq: number) => {
  const d = new Uint8Array(100);
  d[0] = 0x01;
  new DataView(d.buffer).setUint32(1, pktSeq, true);
  return d;
};

/** `n` datagrams from 0, `s` arriving at `at(s)` ms (Infinity: lost), with a message every 50 ms
 *  until `until`. */
function stream(rec: FeedbackRecorder, n: number, at: (s: number) => number, until: number): FeedbackMessage[] {
  const arrivals = Array.from({ length: n }, (_, s) => [at(s), s]).sort((a, b) => a[0] - b[0]);
  const out: FeedbackMessage[] = [];
  let i = 0;
  for (let t = 50; t <= until; t += 50) {
    for (; i < arrivals.length && arrivals[i][0] <= t; i++) rec.record(dgram(arrivals[i][1]), arrivals[i][0]);
    const m = rec.take(t);
    if (m) out.push(m);
  }
  return out;
}

describe("a path that holds each datagram on its own (#1330)", () => {
  it("does not reorder: every message and every NACK is what it was before the reorder window", () => {
    const r = run(10, 300, spread(0), true);
    expect(r.dropped).toBeGreaterThan(0);
    expect(r.nacks).toBeGreaterThan(0);
    expect(r.differences).toEqual([]);
  });

  it("0–20 ms: reports only the real loss, where every reordered datagram used to count lost", () => {
    const r = run(30, 1000, spread(20));
    expect(r.discarded).toBe(0);
    expect(r.reportedLost).toBeGreaterThanOrEqual(r.dropped);
    expect(r.reportedLost - r.dropped).toBeLessThan(WARM_UP);
  });

  it("0–40 ms: in this simulation of Chrome every frame comes and no keyframe is asked for; the loss reported is what never arrived", () => {
    const r = run(30, 1000, spread(40));
    expect(r.keyframeAsks).toBe(0);
    expect(r.released.length).toBe(r.framesSent);
    // Frames are released in order, so one held for its slowest fragment holds those behind it.
    expect(r.longestGapMs).toBeLessThan(200);
    expect(r.reportedLost).toBeGreaterThanOrEqual(r.dropped + r.discarded);
    expect(r.reportedLost - r.dropped - r.discarded).toBeLessThan(WARM_UP);
  });

  it("past the window, what the browser's QUIC discards is reported as the loss it is", () => {
    const r = run(30, 1000, spread(80));
    expect(r.discarded).toBeGreaterThan(0);
    expect(r.reportedLost).toBeGreaterThanOrEqual(r.dropped + r.discarded);
    expect(r.reportedLost - r.dropped - r.discarded).toBeLessThan(WARM_UP);
  });

  it("still reports 5 % real loss as loss, within the reorder window", () => {
    const rec = new FeedbackRecorder(0);
    // The last one arrives: a loss shows only behind a later arrival.
    const msgs = stream(rec, 20_001, (s) => (s % 20 === 19 && s < 20_000 ? Infinity : s * 0.5 + spread(80)(s)), 10_500);
    const last = msgs[msgs.length - 1];
    expect(last.first_pkt_seq + last.count).toBe(20_001);
    const lost = msgs.reduce((n, m) => n + m.lost, 0);
    expect(lost).toBeGreaterThanOrEqual(1000);
    expect(lost - 1000).toBeLessThan(WARM_UP);
  });

  it("reports an outage at once: a gap wider than reordering could open is not held", () => {
    const rec = new FeedbackRecorder(0);
    // A datagram a millisecond, reordered by up to 40 ms; then 530 ms with none (530 lost), back 20 ms
    // before a message.
    const msgs = stream(rec, 1600, (s) => (s < 1000 ? s + spread(40)(s) : s < 1530 ? Infinity : s), 1600);
    expect(rec.reorderMs(1600)).toBeGreaterThan(20);
    // That message reports all of it: nothing waits for the window.
    const back = msgs.find((m) => m.first_pkt_seq + m.count > 1530)!;
    expect(back.lost).toBe(530);
    expect(back.first_pkt_seq + back.count).toBe(1546);
  });

  it("does not take one late datagram, or a short burst, for a path that reorders", () => {
    const rec = new FeedbackRecorder(0);
    // In order, except datagram 500, which comes 150 ms late.
    stream(rec, 700, (s) => (s === 500 ? 650 : s), 700);
    expect(rec.reorderMs(700)).toBe(0);
    // A burst of late ones opens the window for no longer than the history it is measured over.
    const burst = new FeedbackRecorder(0);
    const late = (s: number) => s >= 1000 && s < 1000 + REORDER_MIN_SAMPLES;
    stream(burst, 1200, (s) => (late(s) ? s + 100 : s), 1200);
    expect(burst.reorderMs(1200)).toBeGreaterThan(90);
    expect(burst.reorderMs(1110 + REORDER_HISTORY_MS)).toBe(0);
  });

  it("never holds a gap longer than 150 ms, however late the datagrams come (#1330, #1436)", () => {
    const rec = new FeedbackRecorder(0);
    // A window far wider than the bound: only the 150 ms clamp decides.
    rec.capMs = () => 10_000;
    stream(rec, 3_000, (s) => s + spread(400)(s), 3_000);
    expect(rec.reorderMs(3_000)).toBe(150);
  });
});
