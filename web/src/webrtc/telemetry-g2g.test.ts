import { afterEach, describe, expect, it, vi } from "vitest";
import {
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

// #610: Firefox reports no freezeCount, and a window with too few presented frames has
// no σ. A sample for such a window must not claim `smooth`: 30 s of it records a pass
// that clears the profile's rung failures.
describe("SessionTelemetry posted client_health on a Firefox-shaped report (#610)", () => {
  const firefoxReport = (totalDecodeTime: number) =>
    statsReport([
      { id: "codec-1", type: "codec", mimeType: "video/H264" },
      {
        id: "rtp-1", type: "inbound-rtp", kind: "video", codecId: "codec-1",
        framesDecoded: 10, totalDecodeTime, bytesReceived: 5000, framesPerSecond: 30,
      },
    ]);

  async function queuedSample(totalDecodeTime: number) {
    const tm = telemetryWithStats(async () => firefoxReport(totalDecodeTime));
    await pollOnce(tm);
    return (tm as unknown as { pendingSamples: Array<Record<string, unknown>> }).pendingSamples[0]!;
  }

  it("omits client_health when the window is unmeasured and would read smooth", async () => {
    const sample = await queuedSample(0.05);
    expect(sample).not.toHaveProperty("client_health");
    expect(sample).not.toHaveProperty("client_health_reason");
  });

  it("posts no client_health from a page in bench mode, even for a degraded window", async () => {
    localStorage.setItem("quasar.bench", "1");
    try {
      const sample = await queuedSample(10);
      expect(sample).not.toHaveProperty("client_health");
      expect(sample).not.toHaveProperty("client_health_reason");
    } finally {
      localStorage.removeItem("quasar.bench");
    }
  });

  it("still posts decode_degrading when decode is over budget", async () => {
    const sample = await queuedSample(10);
    expect(sample["client_health"]).toBe("decode_degrading");
  });

  // #686: Firefox's frame callbacks carry mediaTime 0 and a per-frame rtpTimestamp.
  async function sampleAfterFrames(gapsMs: number[]) {
    const tm = telemetryWithStats(async () => firefoxReport(0.05));
    const t = tm as unknown as {
      overlayRunning: boolean;
      onDisplayedFrame(m: VideoFrameCallbackMetadata): void;
      pendingSamples: Array<Record<string, unknown>>;
    };
    t.overlayRunning = true;
    let nowMs = 1000;
    let rtp = 1347462624;
    const now = vi.spyOn(performance, "now");
    for (const gap of [0, ...gapsMs]) {
      nowMs += gap;
      rtp += Math.round(gap * 90);
      now.mockReturnValue(nowMs);
      // No presentedFrames/display metadata: this is the callback-cadence
      // fallback path that keys on mediaTime → rtpTimestamp (#686, #552).
      t.onDisplayedFrame(metadata({
        mediaTime: 0,
        rtpTimestamp: rtp,
        presentedFrames: undefined,
        expectedDisplayTime: undefined,
        presentationTime: undefined,
      }));
    }
    await pollOnce(tm);
    now.mockRestore();
    return t.pendingSamples[0]!;
  }

  it("posts smooth for a Firefox stream that presents evenly", async () => {
    const sample = await sampleAfterFrames(Array(30).fill(33));
    expect((sample["metrics"] as Record<string, number>)["present_n"]).toBe(30);
    expect(sample["client_health"]).toBe("smooth");
  });

  it("posts presentation_degrading for a Firefox stream that stalls", async () => {
    const gaps = Array(30).fill(33);
    gaps.splice(10, 0, 400, 400, 400);
    const sample = await sampleAfterFrames(gaps);
    expect(sample["client_health"]).toBe("presentation_degrading");
  });

  it("posts smooth once the report carries a freezeCount", async () => {
    const report = statsReport([
      {
        id: "rtp-1", type: "inbound-rtp", kind: "video", freezeCount: 0,
        framesDecoded: 10, totalDecodeTime: 0.05, bytesReceived: 5000, framesPerSecond: 30,
      },
    ]);
    const tm = telemetryWithStats(async () => report);
    await pollOnce(tm);
    const sample = (tm as unknown as { pendingSamples: Array<Record<string, unknown>> }).pendingSamples[0]!;
    expect(sample["client_health"]).toBe("smooth");
  });
});

// #552: bench present metrics must read the frame callback's metadata, not the
// page's callback cadence. A page with a mounted idle animation runs fewer
// callbacks per second with no change to what the display showed.
describe("SessionTelemetry present pacing from RVFC metadata (#552)", () => {
  function inboundReport(): RTCStatsReport {
    return statsReport([
      {
        id: "rtp-1", type: "inbound-rtp", kind: "video",
        framesDecoded: 10, totalDecodeTime: 0.05, bytesReceived: 5000, framesPerSecond: 60,
      },
    ]);
  }

  type PresentInternals = {
    overlayRunning: boolean;
    onDisplayedFrame(m?: VideoFrameCallbackMetadata): void;
    onVisibilityChange(): void;
    pendingSamples: Array<{ metrics: Record<string, number> }>;
  };

  async function sampleAfter(decide: (t: PresentInternals) => void) {
    const tm = telemetryWithStats(async () => inboundReport());
    const t = tm as unknown as PresentInternals;
    t.overlayRunning = true;
    decide(t);
    await pollOnce(tm);
    return { sample: t.pendingSamples[0]!, snapshot: tm.getSnapshot() };
  }

  it("takes intervals from expectedDisplayTime, not from when the callback runs", async () => {
    const now = vi.spyOn(performance, "now");
    try {
      const { sample, snapshot } = await sampleAfter((t) => {
        // 31 callbacks, each run 25 ms after the last (a page slowed by a mounted
        // animation), while the display presented distinct frames every 16.67 ms.
        for (let k = 0; k < 31; k++) {
          now.mockReturnValue(5000 + k * 25);
          t.onDisplayedFrame(metadata({
            presentedFrames: k + 1,
            expectedDisplayTime: 1000 + k * 16.67,
          }));
        }
      });
      expect(sample.metrics["present_n"]).toBe(30);
      expect(sample.metrics["present_interval_median_ms"]).toBeCloseTo(16.67, 2);
      expect(sample.metrics["present_fps_median"]).toBeGreaterThan(59);
      expect(sample.metrics["present_fps_median"]).toBeLessThan(61);
      expect(snapshot.presentSource).toBe("rvfc");
    } finally {
      now.mockRestore();
    }
  });

  it("counts coalesced presentations from presentedFrames deltas, not one long frame", async () => {
    const now = vi.spyOn(performance, "now");
    try {
      const { sample } = await sampleAfter((t) => {
        // Each callback coalesced two presented frames 33.34 ms apart on the
        // display clock. Callback runs are deliberately unrelated (40 ms apart).
        for (let k = 0; k <= 10; k++) {
          now.mockReturnValue(4000 + k * 40);
          t.onDisplayedFrame(metadata({
            presentedFrames: 10 + 2 * k,
            expectedDisplayTime: 1000 + k * 33.34,
          }));
        }
      });
      // One interval sample per PRESENTED frame: 10 coalesced callbacks × delta 2.
      expect(sample.metrics["present_n"]).toBe(20);
      expect(sample.metrics["present_interval_median_ms"]).toBeCloseTo(16.67, 2);
      expect(sample.metrics["present_interval_max_ms"]).toBeLessThan(17);
      expect(sample.metrics["present_long_frames"]).toBe(0);
      expect(sample.metrics["present_fps_median"]).toBeGreaterThan(59);
      expect(sample.metrics["present_fps_median"]).toBeLessThan(61);
    } finally {
      now.mockRestore();
    }
  });

  it("labels the requestAnimationFrame fallback path as callback cadence", async () => {
    const now = vi.spyOn(performance, "now");
    try {
      const { snapshot } = await sampleAfter((t) => {
        for (let k = 0; k < 10; k++) {
          now.mockReturnValue(k * 20);
          t.onDisplayedFrame(undefined); // no RVFC metadata: rAF fallback
        }
      });
      expect(snapshot.presentSource).toBe("callback");
    } finally {
      now.mockRestore();
    }
  });

  it("counts and weights a coalesced callback per presented frame, not per callback", async () => {
    const { sample } = await sampleAfter((t) => {
      t.onDisplayedFrame(metadata({ presentedFrames: 1, expectedDisplayTime: 1000 })); // seed
      // One callback the UA coalesced over 3 presentations across 50 ms.
      t.onDisplayedFrame(metadata({ presentedFrames: 4, expectedDisplayTime: 1050 }));
      // Three ordinary callbacks, 33 ms apart on the display clock.
      for (let k = 1; k <= 3; k++) {
        t.onDisplayedFrame(metadata({ presentedFrames: 4 + k, expectedDisplayTime: 1050 + k * 33 }));
      }
    });
    // 6 presented frames over 149 ms: 3 × (50/3) + 3 × 33 → mean 24.83 ms → ~40 fps.
    // One averaged interval per CALLBACK would read (16.67, 33, 33, 33), n 4, ~34.5 fps.
    expect(sample.metrics["present_n"]).toBe(6);
    expect(sample.metrics["present_fps"]).toBeCloseTo(40.27, 2);
    expect(sample.metrics["present_interval_max_ms"]).toBeCloseTo(33, 5);
  });

  it("does not read a hidden tab's gap as one long frame", async () => {
    const vis = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    const now = vi.spyOn(performance, "now");
    try {
      const { sample } = await sampleAfter((t) => {
        for (let k = 0; k < 30; k++) {
          now.mockReturnValue(5000 + k * 100); // callback runs unrelated to the display
          t.onDisplayedFrame(metadata({ presentedFrames: k + 1, expectedDisplayTime: 1000 + k * 16.67 }));
        }
        // The tab hides (RVFC stops), then returns 20 s later with one callback
        // whose presentedFrames delta is 1 and whose display clock spans the
        // whole hidden time. That gap is not a frame duration.
        vis.mockReturnValue("hidden");
        t.onVisibilityChange();
        vis.mockReturnValue("visible");
        t.onDisplayedFrame(metadata({ presentedFrames: 31, expectedDisplayTime: 20000 }));
      });
      // The hidden gap must not become the window's longest interval.
      expect(sample.metrics["present_interval_max_ms"]).toBeCloseTo(16.67, 2);
      expect(sample.metrics["present_long_frames"]).toBe(0);
      expect(sample.metrics["present_n"]).toBe(29); // the 30 callbacks, not the gap callback
    } finally {
      vis.mockRestore();
      now.mockRestore();
    }
  });

  it("reseeds the presentation clock when the RVFC loop restarts", async () => {
    type RestartInternals = PresentInternals & { startOverlayLoop(): void };
    const now = vi.spyOn(performance, "now");
    try {
      const tm = telemetryWithStats(async () => inboundReport());
      const t = tm as unknown as RestartInternals;
      t.overlayRunning = true;
      for (let k = 0; k < 30; k++) {
        now.mockReturnValue(0);
        t.onDisplayedFrame(metadata({ presentedFrames: k + 1, expectedDisplayTime: 1000 + k * 16.67 }));
      }
      // stop() cleared the loop; a restart cannot continue the old display clock.
      t.overlayRunning = false;
      t.startOverlayLoop();
      t.onDisplayedFrame(metadata({ presentedFrames: 31, expectedDisplayTime: 20000 }));
      await pollOnce(tm);
      const sample = t.pendingSamples[0]!;
      expect(sample.metrics["present_interval_max_ms"]).toBeCloseTo(16.67, 2);
      expect(sample.metrics["present_n"]).toBe(29);
    } finally {
      now.mockRestore();
    }
  });

  it("still reads a freeze while visible, spanning a drain, as long", async () => {
    const now = vi.spyOn(performance, "now");
    try {
      const tm = telemetryWithStats(async () => inboundReport());
      const t = tm as unknown as PresentInternals;
      t.overlayRunning = true;
      for (let k = 0; k < 11; k++) {
        now.mockReturnValue(0);
        t.onDisplayedFrame(metadata({ presentedFrames: k + 1, expectedDisplayTime: 1000 + k * 16.67 }));
      }
      await pollOnce(tm); // drain the window in the middle of the run
      // The page stayed VISIBLE: a 400 ms freeze, then the stream resumes.
      t.onDisplayedFrame(metadata({ presentedFrames: 12, expectedDisplayTime: 1000 + 10 * 16.67 + 400 }));
      for (let k = 0; k < 10; k++) {
        t.onDisplayedFrame(metadata({
          presentedFrames: 13 + k,
          expectedDisplayTime: 1000 + 10 * 16.67 + 400 + (k + 1) * 16.67,
        }));
      }
      await pollOnce(tm);
      const sample = t.pendingSamples[1]!;
      expect(sample.metrics["present_interval_max_ms"]).toBeCloseTo(400, 5);
      expect(sample.metrics["present_long_frames"]).toBe(1);
    } finally {
      now.mockRestore();
    }
  });

  it("measures intervals from presentationTime when expectedDisplayTime is absent", async () => {
    const { sample, snapshot } = await sampleAfter((t) => {
      for (let k = 0; k < 31; k++) {
        t.onDisplayedFrame(metadata({
          presentedFrames: k + 1,
          expectedDisplayTime: undefined,
          presentationTime: 1000 + k * 16.67,
        }));
      }
    });
    expect(sample.metrics["present_n"]).toBe(30);
    expect(sample.metrics["present_interval_median_ms"]).toBeCloseTo(16.67, 2);
    expect(snapshot.presentSource).toBe("rvfc");
  });
});
