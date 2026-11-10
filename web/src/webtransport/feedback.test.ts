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

  it("reports at most 8192 per message and the rest in the next", () => {
    const r = new FeedbackRecorder(0);
    for (let s = 0; s < 9000; s++) r.record(dgram(s, 0x01, 5), 1);
    expect(r.take(50)).toMatchObject({ first_pkt_seq: 0, count: 8192 });
    expect(r.take(100)).toMatchObject({ first_pkt_seq: 8192, count: 808 });
  });
});
