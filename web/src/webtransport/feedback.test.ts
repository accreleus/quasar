import { describe, expect, it } from "vitest";
import { FeedbackRecorder } from "./feedback";
import { base64Bytes } from "./framing";

/** A datagram on `channel` carrying `pktSeq` (§Channels: bytes 1..5). */
function dgram(pktSeq: number, channel = 0x01, size = 100): Uint8Array {
  const d = new Uint8Array(size);
  d[0] = channel;
  new DataView(d.buffer).setUint32(1, pktSeq >>> 0, true);
  return d;
}

const arrivals = (b64: string) => {
  const b = base64Bytes(b64);
  return Array.from({ length: b.length / 2 }, (_, i) => new DataView(b.buffer).getUint16(i * 2, true));
};

describe("FeedbackRecorder", () => {
  it("reports each pkt_seq in order: arrival in 50 µs units after the earliest, 0xFFFF for a gap", () => {
    const r = new FeedbackRecorder(0);
    r.record(dgram(10), 5.0);
    r.record(dgram(12, 0x02), 5.25);
    r.record(dgram(13, 0x04), 6.0);
    const m = r.take(50)!;
    expect(m).toMatchObject({ type: "feedback", seq: 1, first_pkt_seq: 10, count: 4, ref_us: 5000, lost: 1, interval_ms: 50 });
    expect(arrivals(m.arrivals_b64)).toEqual([0, 0xffff, 5, 20]);
    // 300 bytes over 50 ms.
    expect(m.recv_kbps).toBe(48);
  });

  it("continues the next message where the last ended, and sends nothing while no datagram arrives", () => {
    const r = new FeedbackRecorder(0);
    r.record(dgram(1), 1);
    expect(r.take(50)).toMatchObject({ first_pkt_seq: 1, count: 1 });
    expect(r.take(100)).toBeNull();
    r.record(dgram(1), 101); // already reported: ignored
    r.record(dgram(3), 102);
    expect(r.take(150)).toMatchObject({ seq: 2, first_pkt_seq: 2, count: 2, lost: 1, interval_ms: 100 });
  });

  it("holds back the newest 5 ms, so a datagram reordered by less is no loss", () => {
    const r = new FeedbackRecorder(0);
    r.record(dgram(1), 10);
    r.record(dgram(3), 12);
    expect(r.take(15.5)).toMatchObject({ first_pkt_seq: 1, count: 1, lost: 0 });
    r.record(dgram(2), 16);
    expect(r.take(25)).toMatchObject({ first_pkt_seq: 2, count: 2, lost: 0 });
  });

  it("wraps pkt_seq and ignores channels without one", () => {
    const r = new FeedbackRecorder(0);
    r.record(dgram(0xffff_ffff), 1);
    r.record(dgram(0), 2);
    r.record(dgram(1, 0x03), 2);
    expect(r.take(50)).toMatchObject({ first_pkt_seq: 0xffff_ffff, count: 2, lost: 0 });
  });

  it("reports a 0x06 audio copy by its own pkt_seq, so the agent does not read it as lost", () => {
    const r = new FeedbackRecorder(0);
    r.record(dgram(1, 0x02), 1);
    r.record(dgram(2, 0x06), 2);
    r.record(dgram(3, 0x02), 3);
    expect(r.take(50)).toMatchObject({ first_pkt_seq: 1, count: 3, lost: 0 });
  });

  it("reports at most 8192 per message and the rest in the next", () => {
    const r = new FeedbackRecorder(0);
    for (let s = 0; s < 9000; s++) r.record(dgram(s, 0x01, 5), 1);
    expect(r.take(50)).toMatchObject({ first_pkt_seq: 0, count: 8192 });
    expect(r.take(100)).toMatchObject({ first_pkt_seq: 8192, count: 808 });
  });

  describe("takeLate (#1330)", () => {
    it("counts each datagram that fills a gap a later one opened, once, and starts again from 0", () => {
      const r = new FeedbackRecorder(0);
      for (let ms = 0; ms < 100; ms++) r.record(dgram(ms), ms); // a rate, for the gap bound
      r.take(100);
      r.record(dgram(102), 101);
      r.record(dgram(100), 102);
      r.record(dgram(101), 103);
      expect(r.takeLate()).toBe(2);
      expect(r.takeLate()).toBe(0);
    });

    it("counts none on a path that loses without reordering", () => {
      const r = new FeedbackRecorder(0);
      for (let s = 0; s < 1_000; s++) if (s % 10 !== 3) r.record(dgram(s), s);
      r.take(1_000);
      expect(r.takeLate()).toBe(0);
    });

    it("does not time a gap wider than reordering could open, even when the burst fills it", () => {
      const run = (gap: number) => {
        const r = new FeedbackRecorder(0);
        let t = 0;
        for (let s = 0; s < 100; s++) r.record(dgram(s), t++);
        r.take(t); // about one datagram a millisecond
        for (let s = 100 + gap; s < 200 + gap; s++) r.record(dgram(s), t++);
        for (let s = 100; s < 100 + gap; s++) r.record(dgram(s), t);
        return r.takeLate();
      };
      expect(run(50)).toBe(50); // within the 150 ms bound at this rate: reordering
      expect(run(300)).toBe(0); // an outage, filled late: not reordering
    });
  });
});
