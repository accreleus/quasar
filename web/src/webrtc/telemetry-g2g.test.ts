import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ABS_CAPTURE_TIME_URI,
  hasAbsCaptureTimeExtension,
  rvfcCaptureTimeSample,
  SessionTelemetry,
} from "./telemetry";

function metadata(overrides: Partial<VideoFrameCallbackMetadata>): VideoFrameCallbackMetadata {
  return {
    captureTime: 100,
    expectedDisplayTime: 140,
    presentedFrames: 1,
    processingDuration: 0,
    receiveTime: 100,
    rtpTimestamp: 1,
    width: 1920,
    height: 1080,
    ...overrides,
  } as VideoFrameCallbackMetadata;
}

describe("rvfcCaptureTimeSample", () => {
  it("accepts a frame-correlated RVFC captureTime sample", () => {
    expect(rvfcCaptureTimeSample(metadata({ captureTime: 100, expectedDisplayTime: 142.5 })))
      .toEqual({ sampleMs: 42.5 });
  });

  it("rejects malformed capture timing", () => {
    expect(rvfcCaptureTimeSample(metadata({ captureTime: Number.NaN })))
      .toEqual({ sampleMs: null });
  });

  it("rejects a negative sample without substituting another timestamp domain", () => {
    expect(rvfcCaptureTimeSample(metadata({ captureTime: 150, expectedDisplayTime: 140 })))
      .toEqual({ sampleMs: null });
  });

  it("rejects an implausible outlier", () => {
    expect(rvfcCaptureTimeSample(metadata({ captureTime: 0, expectedDisplayTime: 5000 })))
      .toEqual({ sampleMs: null });
  });

  it("does not manufacture a sample when captureTime is missing", () => {
    expect(rvfcCaptureTimeSample(metadata({ captureTime: undefined })))
      .toEqual({ sampleMs: null });
  });
});

describe("hasAbsCaptureTimeExtension", () => {
  it("accepts only the negotiated absolute-capture-time URI", () => {
    expect(hasAbsCaptureTimeExtension([
      { uri: "urn:ietf:params:rtp-hdrext:transport-wide-cc-02" },
      { uri: ABS_CAPTURE_TIME_URI },
    ])).toBe(true);
    expect(hasAbsCaptureTimeExtension([
      { uri: "urn:ietf:params:rtp-hdrext:transport-wide-cc-02" },
    ])).toBe(false);
    expect(hasAbsCaptureTimeExtension(undefined)).toBe(false);
  });
});

function telemetry(): SessionTelemetry {
  const video = { videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: () => 1 } as unknown as HTMLVideoElement;
  const channel = { readyState: "open", bufferedAmount: 0, onmessage: null, send: () => {} } as unknown as RTCDataChannel;
  return new SessionTelemetry(video, async () => new Map() as unknown as RTCStatsReport, channel, "session", "token");
}

/** Telemetry wired with a controllable receiver-parameter probe, so the
 *  negotiation lifecycle can be driven without a real RTCPeerConnection. */
function telemetryWithProbe(negotiated: () => boolean): SessionTelemetry {
  const video = { videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: () => 1 } as unknown as HTMLVideoElement;
  const channel = { readyState: "open", bufferedAmount: 0, onmessage: null, send: () => {} } as unknown as RTCDataChannel;
  return new SessionTelemetry(
    video,
    async () => new Map() as unknown as RTCStatsReport,
    channel,
    "session",
    "token",
    undefined,
    undefined,
    undefined,
    negotiated,
  );
}

type TelemetryInternals = {
  getSnapshot(): ReturnType<SessionTelemetry["getSnapshot"]>;
  recordRvfcCaptureTimeSample(sample: number | null): void;
  expireStaleRvfcCaptureTime(nowMs?: number): void;
  resolveAbsCaptureTime(): void;
  buildMetrics(): Record<string, number>;
};

function internals(tm: SessionTelemetry): TelemetryInternals {
  return tm as unknown as TelemetryInternals;
}

afterEach(() => vi.useRealTimers());

describe("SessionTelemetry abs-capture-time negotiation lifecycle", () => {
  it("holds pending through the grace period, then reports unavailable", () => {
    const tm = internals(telemetryWithProbe(() => false));
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("pending");

    // Each call stands for one stats poll that DID find an inbound-rtp report —
    // the caller only reaches this after that check, which is what makes a
    // negative answer trustworthy rather than "we polled too early".
    tm.resolveAbsCaptureTime();
    tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("pending");

    tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("unavailable");
    expect(tm.buildMetrics()).toMatchObject({ abs_capture_time_negotiated: 0 });
  });

  it("latches negotiated and stays there through a later probe miss", () => {
    let negotiated = false;
    const tm = internals(telemetryWithProbe(() => negotiated));

    tm.resolveAbsCaptureTime();
    negotiated = true;
    tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("negotiated");

    // One transport and one codec per session: a later miss is a stats hiccup,
    // not a renegotiation, so it must not downgrade the answer.
    negotiated = false;
    tm.resolveAbsCaptureTime();
    tm.resolveAbsCaptureTime();
    tm.resolveAbsCaptureTime();
    tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("negotiated");
    expect(tm.buildMetrics()).toMatchObject({ abs_capture_time_negotiated: 1 });
  });

  it("recovers to negotiated if the receiver reports the extension late", () => {
    let negotiated = false;
    const tm = internals(telemetryWithProbe(() => negotiated));
    for (let i = 0; i < 4; i++) tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("unavailable");

    negotiated = true;
    tm.resolveAbsCaptureTime();
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("negotiated");
  });

  it("stays pending forever when no probe is wired at all", () => {
    const tm = internals(telemetry());
    for (let i = 0; i < 10; i++) tm.resolveAbsCaptureTime();
    // No probe means nothing was ever asked — "unavailable" would be a claim
    // this instance has no evidence for.
    expect(tm.getSnapshot().absCaptureTimeNegotiation).toBe("pending");
  });
});

describe("SessionTelemetry RVFC capability lifecycle", () => {
  it("uses a grace period, then reports unavailable with no stale series", () => {
    const tm = internals(telemetry());
    tm.recordRvfcCaptureTimeSample(null);
    tm.recordRvfcCaptureTimeSample(null);
    expect(tm.getSnapshot().rvfcCaptureTimeCapability).toBe("pending");

    tm.recordRvfcCaptureTimeSample(null);
    const snap = tm.getSnapshot();
    expect(snap.rvfcCaptureTimeCapability).toBe("unavailable");
    expect(snap.g2gMs).toBeNull();
    expect(tm.buildMetrics()).toMatchObject({
      rvfc_capture_time_available: 0,
      abs_capture_time_negotiated: 0,
    });
  });

  it("latches available on first valid sample and keeps later gaps from invalidating series", () => {
    const tm = internals(telemetry());
    tm.recordRvfcCaptureTimeSample(null);
    tm.recordRvfcCaptureTimeSample(31);
    tm.recordRvfcCaptureTimeSample(null);

    const snap = tm.getSnapshot();
    expect(snap.rvfcCaptureTimeCapability).toBe("available");
    expect(snap.absCaptureTimeNegotiation).toBe("pending");
    expect(snap.g2gMs).toBe(31);
    expect(tm.buildMetrics()).toMatchObject({
      rvfc_capture_time_available: 1,
      abs_capture_time_negotiated: 0,
      glass_to_glass_ms: 31,
    });
  });

  it("expires stale timing through repeated null RVFC callbacks and recovers", () => {
    vi.useFakeTimers({ toFake: ["performance"] });
    const tm = internals(telemetry());
    tm.recordRvfcCaptureTimeSample(31);
    for (let i = 0; i < 4; i++) {
      vi.advanceTimersByTime(1_000);
      tm.recordRvfcCaptureTimeSample(null);
    }
    tm.expireStaleRvfcCaptureTime();
    expect(tm.getSnapshot().g2gMs).toBe(31);

    vi.advanceTimersByTime(1_001);
    tm.recordRvfcCaptureTimeSample(null);
    tm.expireStaleRvfcCaptureTime();
    expect(tm.getSnapshot().rvfcCaptureTimeCapability).toBe("unavailable");
    expect(tm.getSnapshot().g2gMs).toBeNull();
    expect(tm.buildMetrics()).toMatchObject({ rvfc_capture_time_available: 0 });
    expect(tm.buildMetrics()).not.toHaveProperty("glass_to_glass_ms");

    tm.recordRvfcCaptureTimeSample(24);
    expect(tm.getSnapshot().rvfcCaptureTimeCapability).toBe("available");
    expect(tm.getSnapshot().g2gMs).toBe(24);
  });
});

// Multi-codec spec §6.1: negotiated-codec telemetry + the decodeFailed wiring.
function telemetryWithStats(getStatsFn: () => Promise<RTCStatsReport>): SessionTelemetry {
  const video = { videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: () => 1 } as unknown as HTMLVideoElement;
  const channel = { readyState: "open", bufferedAmount: 0, onmessage: null, send: () => {} } as unknown as RTCDataChannel;
  return new SessionTelemetry(video, getStatsFn, channel, "session", "token");
}

/** Builds a Map-backed fake RTCStatsReport from plain stat objects (each needs an `id`). */
function statsReport(entries: Array<Record<string, unknown>>): RTCStatsReport {
  const m = new Map<string, unknown>();
  for (const e of entries) m.set(e["id"] as string, e);
  return m as unknown as RTCStatsReport;
}

async function pollOnce(tm: SessionTelemetry): Promise<void> {
  await (tm as unknown as { pollStats(): Promise<void> }).pollStats();
}

describe("SessionTelemetry negotiated-codec resolution (multi-codec spec §6.1)", () => {
  it("resolves inbound-rtp.codecId -> the matching codec stat's mimeType", async () => {
    const report = statsReport([
      { id: "codec-1", type: "codec", mimeType: "video/H265" },
      {
        id: "rtp-1", type: "inbound-rtp", kind: "video", codecId: "codec-1",
        framesDecoded: 10, bytesReceived: 5000, framesPerSecond: 60,
      },
    ]);
    const tm = telemetryWithStats(async () => report);
    await pollOnce(tm);
    expect(tm.getSnapshot().negotiatedCodec).toBe("video/H265");
  });

  it("leaves negotiatedCodec null when no codec stat matches codecId", async () => {
    const report = statsReport([
      { id: "rtp-1", type: "inbound-rtp", kind: "video", codecId: "missing", framesDecoded: 1, bytesReceived: 1, framesPerSecond: 1 },
    ]);
    const tm = telemetryWithStats(async () => report);
    await pollOnce(tm);
    expect(tm.getSnapshot().negotiatedCodec).toBeNull();
  });

  it("exposes cumulative framesDecodedTotal/bytesReceivedTotal (not per-window deltas)", async () => {
    const report = statsReport([
      { id: "rtp-1", type: "inbound-rtp", kind: "video", framesDecoded: 0, bytesReceived: 12_345, framesPerSecond: 0 },
    ]);
    const tm = telemetryWithStats(async () => report);
    await pollOnce(tm);
    const snap = tm.getSnapshot();
    expect(snap.framesDecodedTotal).toBe(0);
    expect(snap.bytesReceivedTotal).toBe(12_345);
  });
});

describe("SessionTelemetry.setDecodeFailed (multi-codec spec §6.1)", () => {
  it("feeds classifyClientHealth's decodeFailed input, surfacing client_unsupported", async () => {
    const report = statsReport([
      { id: "rtp-1", type: "inbound-rtp", kind: "video", framesDecoded: 0, bytesReceived: 12_345, framesPerSecond: 0 },
    ]);
    const tm = telemetryWithStats(async () => report);
    // Before wiring: a stalled decode with no decodeFailed signal reads as smooth
    // (0 decodeMs delta — no frames decoded yet, so decode_degrading can't fire either).
    await pollOnce(tm);
    expect(tm.getSnapshot().clientHealth).toBe("smooth");

    tm.setDecodeFailed(true);
    await pollOnce(tm);
    expect(tm.getSnapshot().clientHealth).toBe("client_unsupported");
  });
});
