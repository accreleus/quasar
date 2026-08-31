import { describe, expect, it } from "vitest";
import type { BenchFrame } from "./aggregate";
import {
  diffInbound,
  frameStages,
  parseInboundRtp,
  stagesReconcile,
  summarizeStages,
  type InboundCounters,
} from "./stages";

// A synthetic 1080p60 frame set with a KNOWN stage split, so every assertion
// below is against arithmetic we chose rather than against whatever the code
// happens to produce.
const TIME_ORIGIN = 1_800_000_000_000;
const OFFSET = 3; // ST-05 client->host offset (ms)

interface SyntheticSpec {
  /** high-res ms of the frame's last packet. */
  receiveMs: number;
  hostToReceive: number;
  waitMs: number;
  decodeMs: number;
  displayMs: number;
  decoded?: boolean;
}

/** Build one frame whose stages are exactly the spec's numbers. */
function synth(s: SyntheticSpec): BenchFrame {
  const decoded = s.decoded ?? true;
  const presentation = s.receiveMs + s.waitMs + s.decodeMs;
  const expectedDisplay = presentation + s.displayMs;
  // Invert `host_to_receive = timeOrigin + receive + offset - host_time_ms`.
  const hostTime = TIME_ORIGIN + s.receiveMs + OFFSET - s.hostToReceive;
  const presentMs = TIME_ORIGIN + expectedDisplay;
  return {
    present_ms: presentMs,
    decoded,
    confidence: decoded ? 1 : 0,
    offset_ms: OFFSET,
    ...(decoded
      ? { host_time_ms: hostTime, frame_index: 1, g2g_ms: presentMs + OFFSET - hostTime }
      : {}),
    receive_ms: s.receiveMs,
    presentation_ms: presentation,
    expected_display_ms: expectedDisplay,
    processing_ms: s.decodeMs,
  };
}

const BASE: SyntheticSpec = {
  receiveMs: 10_000,
  hostToReceive: 20,
  waitMs: 9,
  decodeMs: 7,
  displayMs: 8,
};

describe("frameStages", () => {
  it("splits a frame into the exact stages it was built from", () => {
    const s = frameStages(synth(BASE), TIME_ORIGIN);
    expect(s.hostToReceiveMs).toBeCloseTo(20, 6);
    expect(s.receiveToPresentMs).toBeCloseTo(16, 6); // wait 9 + decode 7
    expect(s.decodeMs).toBeCloseTo(7, 6);
    expect(s.waitAndQueueMs).toBeCloseTo(9, 6);
    expect(s.presentToDisplayMs).toBeCloseTo(8, 6);
  });

  it("still reports the marker-independent stages for an undecoded frame", () => {
    // An undecoded frame has no host_time_ms, so only the cross-machine stage
    // is unavailable — the receive-path split does not depend on the marker.
    const s = frameStages(synth({ ...BASE, decoded: false }), TIME_ORIGIN);
    expect(s.hostToReceiveMs).toBeNull();
    expect(s.receiveToPresentMs).toBeCloseTo(16, 6);
    expect(s.decodeMs).toBeCloseTo(7, 6);
    expect(s.presentToDisplayMs).toBeCloseTo(8, 6);
  });

  it("refuses host_to_receive without a measured clock offset", () => {
    // Substituting 0 would turn a raw two-machine clock difference into
    // something that reads as a latency. Same rule g2g_ms follows.
    const frame = { ...synth(BASE), offset_ms: null };
    expect(frameStages(frame, TIME_ORIGIN).hostToReceiveMs).toBeNull();
    expect(frameStages(frame, TIME_ORIGIN).receiveToPresentMs).toBeCloseTo(16, 6);
  });

  it("yields all-null when the UA supplied no RVFC metadata at all", () => {
    const bare: BenchFrame = {
      present_ms: TIME_ORIGIN, decoded: true, confidence: 1, offset_ms: 0,
      host_time_ms: TIME_ORIGIN - 50,
    };
    expect(frameStages(bare, TIME_ORIGIN)).toEqual({
      hostToReceiveMs: null, receiveToPresentMs: null, decodeMs: null,
      waitAndQueueMs: null, presentToDisplayMs: null,
    });
  });

  it("omits only the decode-derived stage when processingDuration is absent", () => {
    const frame = { ...synth(BASE) };
    delete frame.processing_ms;
    const s = frameStages(frame, TIME_ORIGIN);
    expect(s.decodeMs).toBeNull();
    expect(s.waitAndQueueMs).toBeNull();
    expect(s.receiveToPresentMs).toBeCloseTo(16, 6);
  });
});

describe("stagesReconcile", () => {
  // Tolerance note: the residual floor is double-precision arithmetic on
  // unix-epoch milliseconds (~1.8e12), which is ~5e-5 ms of representation
  // error — six orders of magnitude below any stage being measured. Asserting
  // tighter than that tests the IEEE-754 mantissa, not the decomposition.
  it("is zero — the split is an identity, not a fit", () => {
    expect(stagesReconcile(synth(BASE), TIME_ORIGIN)).toBeCloseTo(0, 3);
  });

  it("is zero across a spread of stage shapes", () => {
    const specs: SyntheticSpec[] = [
      { receiveMs: 5_000, hostToReceive: 12.5, waitMs: 0.4, decodeMs: 1.2, displayMs: 16.6 },
      { receiveMs: 5_100, hostToReceive: 44, waitMs: 30, decodeMs: 12, displayMs: 0 },
      { receiveMs: 5_200, hostToReceive: 19.75, waitMs: 8.25, decodeMs: 6.5, displayMs: 8.33 },
    ];
    for (const spec of specs) {
      expect(stagesReconcile(synth(spec), TIME_ORIGIN)).toBeCloseTo(0, 3);
    }
  });

  it("exposes the mismatch when present_ms did not come from expectedDisplayTime", () => {
    // presentEpochMs falls back to Date.now() when the UA gives no
    // expectedDisplayTime. The g2g is then a different quantity than the stage
    // sum, and this is how that shows rather than silently skewing a stage.
    const frame = synth(BASE);
    const drifted: BenchFrame = { ...frame, g2g_ms: (frame.g2g_ms ?? 0) + 5 };
    expect(stagesReconcile(drifted, TIME_ORIGIN)).toBeCloseTo(5, 6);
  });
});

describe("parseInboundRtp", () => {
  const report = (entries: Record<string, unknown>[]): RTCStatsReport =>
    ({ forEach: (cb: (v: unknown) => void) => entries.forEach(cb) }) as unknown as RTCStatsReport;

  it("picks the VIDEO inbound-rtp, never the audio one", () => {
    const got = parseInboundRtp(report([
      { type: "inbound-rtp", kind: "audio", jitterBufferDelay: 99, jitterBufferEmittedCount: 1 },
      { type: "inbound-rtp", kind: "video", jitterBufferDelay: 1.5, jitterBufferEmittedCount: 60 },
    ]));
    expect(got?.jitterBufferDelay).toBe(1.5);
  });

  it("accepts the legacy mediaType spelling", () => {
    const got = parseInboundRtp(report([
      { type: "inbound-rtp", mediaType: "video", totalDecodeTime: 0.42, framesDecoded: 60 },
    ]));
    expect(got?.totalDecodeTime).toBe(0.42);
  });

  it("returns null with no video inbound-rtp, and drops non-finite fields", () => {
    expect(parseInboundRtp(report([{ type: "outbound-rtp", kind: "video" }]))).toBeNull();
    expect(parseInboundRtp(null)).toBeNull();
    const got = parseInboundRtp(report([
      { type: "inbound-rtp", kind: "video", jitterBufferDelay: NaN, framesDecoded: 60 },
    ]));
    expect(got?.jitterBufferDelay).toBeUndefined();
    expect(got?.framesDecoded).toBe(60);
  });
});

describe("diffInbound", () => {
  const prev: InboundCounters = {
    jitterBufferDelay: 1.0, jitterBufferEmittedCount: 100, jitterBufferTargetDelay: 2.0,
    totalAssemblyTime: 0.1, framesAssembledFromMultiplePackets: 50,
    totalProcessingDelay: 1.5, totalDecodeTime: 0.5, totalInterFrameDelay: 1.6,
    framesDecoded: 100, framesDropped: 2,
  };

  it("converts seconds to a per-frame millisecond mean over the window", () => {
    const cur: InboundCounters = {
      ...prev,
      jitterBufferDelay: 1.6, jitterBufferEmittedCount: 160, jitterBufferTargetDelay: 5.0,
      totalAssemblyTime: 0.13, framesAssembledFromMultiplePackets: 60,
      totalProcessingDelay: 2.1, totalDecodeTime: 0.92, totalInterFrameDelay: 2.6,
      framesDecoded: 160, framesDropped: 5,
    };
    const d = diffInbound(prev, cur)!;
    expect(d.jitterBufferMs).toBeCloseTo(10, 6); // 0.6 s / 60 frames
    expect(d.jitterBufferTargetMs).toBeCloseTo(50, 6);
    expect(d.assemblyMs).toBeCloseTo(3, 6); // 0.03 s / 10 frames
    expect(d.processingMs).toBeCloseTo(10, 6);
    expect(d.decodeMs).toBeCloseTo(7, 6); // 0.42 s / 60 frames
    expect(d.interFrameMs).toBeCloseTo(16.666, 2);
    expect(d.framesDecoded).toBe(60);
    expect(d.framesDropped).toBe(3);
  });

  it("is a WINDOW mean, so a bad settle period cannot leak into steady state", () => {
    // Same absolute totals, different window: the lifetime mean would be
    // dominated by the start-up transient forever.
    const settle: InboundCounters = { ...prev, jitterBufferDelay: 5.0, jitterBufferEmittedCount: 100 };
    const later: InboundCounters = { ...settle, jitterBufferDelay: 5.3, jitterBufferEmittedCount: 160 };
    expect(diffInbound(settle, later)!.jitterBufferMs).toBeCloseTo(5, 6);
  });

  it("reports null rather than zero for a window that emitted no frames", () => {
    const d = diffInbound(prev, { ...prev })!;
    expect(d.jitterBufferMs).toBeNull();
    expect(d.decodeMs).toBeNull();
    expect(d.framesDecoded).toBe(0);
  });

  it("needs both reads", () => {
    expect(diffInbound(null, prev)).toBeNull();
    expect(diffInbound(prev, null)).toBeNull();
  });
});

describe("summarizeStages", () => {
  // Ten frames, stage values chosen so p50/p95 are unambiguous under the
  // nearest-rank percentile aggregate.ts uses.
  const frames = Array.from({ length: 10 }, (_, i) =>
    synth({
      receiveMs: 10_000 + i * 16.67,
      hostToReceive: 20 + i,      // 20..29
      waitMs: 9,
      decodeMs: 7,
      displayMs: 8,
    }));

  const inbound = diffInbound(
    { jitterBufferDelay: 0, jitterBufferEmittedCount: 0, jitterBufferTargetDelay: 0,
      totalDecodeTime: 0, framesDecoded: 0, framesDropped: 0,
      totalAssemblyTime: 0, framesAssembledFromMultiplePackets: 0,
      totalProcessingDelay: 0, totalInterFrameDelay: 0 },
    { jitterBufferDelay: 0.36, jitterBufferEmittedCount: 60, jitterBufferTargetDelay: 3.0,
      totalDecodeTime: 0.42, framesDecoded: 60, framesDropped: 1,
      totalAssemblyTime: 0.02, framesAssembledFromMultiplePackets: 10,
      totalProcessingDelay: 0.9, totalInterFrameDelay: 1.0 },
  );

  it("emits p50/p95 for every RVFC-derived stage", () => {
    const out = summarizeStages(frames, TIME_ORIGIN, inbound);
    expect(out["stage_host_to_receive_p50_ms"]).toBeCloseTo(24, 6);
    expect(out["stage_host_to_receive_p95_ms"]).toBeCloseTo(29, 6);
    expect(out["stage_receive_to_present_p50_ms"]).toBeCloseTo(16, 6);
    expect(out["stage_decode_p50_ms"]).toBeCloseTo(7, 6);
    expect(out["stage_wait_queue_p50_ms"]).toBeCloseTo(9, 6);
    expect(out["stage_present_to_display_p50_ms"]).toBeCloseTo(8, 6);
  });

  it("carries the reconciliation residual on the wire, and it is ~0", () => {
    const out = summarizeStages(frames, TIME_ORIGIN, inbound);
    expect(out["stage_reconcile_p95_ms"]).toBeLessThan(1e-3);
  });

  it("reports sample counts so a null is distinguishable from a zero", () => {
    const mixed = [...frames, synth({ ...BASE, decoded: false })];
    const out = summarizeStages(mixed, TIME_ORIGIN, inbound);
    expect(out["stage_n"]).toBe(11);              // marker-independent stages
    expect(out["stage_host_to_receive_n"]).toBe(10); // only decoded frames
    expect(out["stage_decode_n"]).toBe(11);
  });

  it("folds in the getStats window means", () => {
    const out = summarizeStages(frames, TIME_ORIGIN, inbound);
    expect(out["stage_jb_mean_ms"]).toBeCloseTo(6, 6);   // 0.36 s / 60
    expect(out["stage_jb_target_mean_ms"]).toBeCloseTo(50, 6);
    expect(out["stage_decode_stats_mean_ms"]).toBeCloseTo(7, 6);
    expect(out["stage_assembly_mean_ms"]).toBeCloseTo(2, 6);
    expect(out["stage_frames_dropped"]).toBe(1);
  });

  it("derives the render queue as wait_queue minus the jitter buffer", () => {
    const out = summarizeStages(frames, TIME_ORIGIN, inbound);
    expect(out["stage_render_queue_derived_p50_ms"]).toBeCloseTo(9 - 6, 6);
  });

  it("leaves a negative derived render queue visible rather than clamped", () => {
    // wait_queue 9 ms against a 12 ms buffer mean is a real disagreement between
    // the per-frame path and the counter mean. Clamping to 0 would invent
    // agreement that the data does not support.
    const bigJb = diffInbound(
      { jitterBufferDelay: 0, jitterBufferEmittedCount: 0 },
      { jitterBufferDelay: 0.72, jitterBufferEmittedCount: 60 },
    );
    const out = summarizeStages(frames, TIME_ORIGIN, bigJb);
    expect(out["stage_render_queue_derived_p50_ms"]).toBeCloseTo(-3, 6);
  });

  it("emits every key as null (not absent, not 0) with no stage data", () => {
    const bare: BenchFrame[] = [
      { present_ms: TIME_ORIGIN, decoded: false, confidence: 0, offset_ms: null },
    ];
    const out = summarizeStages(bare, TIME_ORIGIN, null);
    expect(out["stage_host_to_receive_p50_ms"]).toBeNull();
    expect(out["stage_jb_mean_ms"]).toBeNull();
    expect(out["stage_render_queue_derived_p50_ms"]).toBeNull();
    expect(out["stage_n"]).toBe(0);
  });
});
