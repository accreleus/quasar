import { describe, expect, it } from "vitest";
import { FeedbackRecorder, REORDER_LOSS_S } from "./feedback";
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

  describe("reorderingLoss (#1330)", () => {
    /** `secs` of one datagram a millisecond, reported every 50 ms and judged every second as the media
     *  path does. `swap`: each pair arrives second one first; `dropEvery`: that pkt_seq never comes;
     *  `outageAt`: that second delivers nothing. The seconds reorderingLoss said true. */
    function judge(secs: number, o: { swap?: boolean; dropEvery?: number; outageAt?: number }): number[] {
      const r = new FeedbackRecorder(0);
      const fired: number[] = [];
      let seq = 0;
      for (let ms = 0; ms < secs * 1000; ms += 2) {
        const pair = [seq, seq + 1];
        seq += 2;
        if (o.swap) pair.reverse();
        const silent = o.outageAt !== undefined && Math.floor(ms / 1000) === o.outageAt;
        pair.forEach((s, i) => {
          if (!silent && !(o.dropEvery && s % o.dropEvery === 0)) r.record(dgram(s), ms + i);
        });
        if ((ms + 2) % 50 === 0) r.take(ms + 2);
        if ((ms + 2) % 1000 === 0 && r.reorderingLoss()) fired.push((ms + 2) / 1000);
      }
      return fired;
    }

    it(`ends a path that reorders and loses 5 % after ${REORDER_LOSS_S} s`, () => {
      expect(judge(6, { swap: true, dropEvery: 20 })[0]).toBe(REORDER_LOSS_S);
    });

    it("never on a lossy path that does not reorder (2 % and 10 %), nor on one that reorders without loss", () => {
      expect(judge(10, { dropEvery: 50 })).toEqual([]);
      expect(judge(10, { dropEvery: 10 })).toEqual([]);
      expect(judge(10, { swap: true })).toEqual([]);
    });

    it("never after an outage: a gap too wide to time is not reordering", () => {
      expect(judge(10, { outageAt: 3 })).toEqual([]);
      expect(judge(10, { dropEvery: 25, outageAt: 3 })).toEqual([]);
    });

    it("a second without loss starts the count again", () => {
      const r = new FeedbackRecorder(0);
      let seq = 0;
      let ms = 0;
      const second = (lossy: boolean) => {
        for (const end = ms + 1000; ms < end; ms += 2, seq += 2) {
          if (!(lossy && seq % 20 === 0)) r.record(dgram(seq + 1), ms);
          r.record(dgram(seq), ms + 1);
          if ((ms + 2) % 50 === 0) r.take(ms + 2);
        }
        return r.reorderingLoss();
      };
      expect([second(true), second(true), second(false), second(true), second(true), second(true)]).toEqual([false, false, false, false, false, true]);
    });
  });
});
