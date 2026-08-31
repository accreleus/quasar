// telemetrySink — the per-tick ordering that no test could reach while this
// body lived four closures deep inside SessionPage's mount effect (the suite
// there mocks SessionTelemetry with a no-op onUpdate, so none of it ever ran).
// No DOM, no renderer, no fakes of our own code: the effects interface IS the
// unit's output.

import { describe, expect, it, vi } from "vitest";
import { createTelemetrySink, type TelemetrySinkEffects } from "./telemetrySink";
import type { TelemetrySnapshot } from "../../webrtc/telemetry";

/** A telemetry window. Defaults describe a healthy tick; each test overrides
 *  only the fields it is about. */
function snap(over: Partial<TelemetrySnapshot> = {}): TelemetrySnapshot {
  return {
    fps: 60,
    bitrateKbps: 8000,
    rttMs: 20,
    jbMs: null,
    decodeMs: null,
    packetsLost: 0,
    framesDropped: 0,
    freezeCount: 0,
    presentFps: 60,
    presentSdMs: 4,
    presentCadence: {
      n: 60, medianMs: 16.7, meanMs: 16.7, p95Ms: 17, maxMs: 18, sdMs: 4,
      fpsFromMedian: 60, fpsFromMean: 60, doubledFraction: 0, longFrames: 0,
      driftMs: 0, inherentBeat: true,
    },
    playoutTargetMs: 30,
    encodeMs: null,
    networkMs: null,
    decodeDisplayMs: null,
    inputMetrics: null,
    displayRefreshHz: 60,
    clientHealth: "smooth",
    clientHealthReason: "",
    negotiatedCodec: "h264",
    framesDecodedTotal: 1000,
    bytesReceivedTotal: 500_000,
    g2gMs: 45,
    ...over,
  } as TelemetrySnapshot;
}

function harness(overrides: Partial<TelemetrySinkEffects> = {}) {
  let clock = 0;
  const calls: string[] = [];
  const fx: TelemetrySinkEffects = {
    fanOut: vi.fn(() => calls.push("fanOut")),
    playoutSample: vi.fn(() => calls.push("playoutSample")),
    recoverMediaPath: vi.fn(() => calls.push("recoverMediaPath")),
    mediaPathFlowing: vi.fn(() => calls.push("mediaPathFlowing")),
    emitFreezeDetected: vi.fn(() => calls.push("emitFreezeDetected")),
    setDecodeFailed: vi.fn(() => calls.push("setDecodeFailed")),
    onClientUnsupported: vi.fn(() => calls.push("onClientUnsupported")),
    onDisplayRefreshHz: vi.fn(() => calls.push("onDisplayRefreshHz")),
    now: () => clock,
    isHidden: () => false,
    ...overrides,
  };
  return {
    fx,
    calls,
    sink: createTelemetrySink(fx),
    advance: (ms: number) => {
      clock += ms;
    },
  };
}

describe("per-tick ordering", () => {
  it("fans out before it accumulates or feeds the controller", () => {
    const { sink, calls } = harness();
    sink.push(snap());
    expect(calls.slice(0, 3)).toEqual(["fanOut", "onDisplayRefreshHz", "playoutSample"]);
  });

  it("passes the window's jitter and freeze count to the playout controller", () => {
    const { sink, fx } = harness();
    sink.push(snap({ presentSdMs: 18, freezeCount: 2 }));
    expect(fx.playoutSample).toHaveBeenCalledWith({ sdMs: 18, freezeCount: 2 });
  });
});

describe("summary accumulation", () => {
  it("collects fps and prefers glass-to-glass over rtt for latency", () => {
    const { sink } = harness();
    sink.push(snap({ fps: 59, g2gMs: 44, rttMs: 20 }));
    sink.push(snap({ fps: 61, g2gMs: null, rttMs: 22 }));
    const s = sink.summary();
    expect(s.fps).toEqual([59, 61]);
    expect(s.latency).toEqual([44, 22]);
  });

  it("drops a zero-fps window and a negative latency", () => {
    const { sink } = harness();
    sink.push(snap({ fps: 0, bitrateKbps: 1, g2gMs: -1, rttMs: null }));
    const s = sink.summary();
    expect(s.fps).toEqual([]);
    expect(s.latency).toEqual([]);
  });

  it("reports elapsed as a delta on its own clock, whatever that clock's origin", () => {
    // `now` is monotonic-since-page-load in production, so its origin is
    // nowhere near the Unix epoch. Exposing the start instead of the delta let
    // a caller subtract it from Date.now() and report the epoch as a duration.
    let clock = 1_234.5;
    const { sink } = harness({ now: () => clock });
    expect(sink.summary().elapsedMs).toBe(0);
    clock += 42_000;
    expect(sink.summary().elapsedMs).toBe(42_000);
  });
});

describe("media-stall recovery", () => {
  const stalled = { bitrateKbps: 0, fps: 0 };

  it("recovers on the third consecutive zero window, and only once", () => {
    const { sink, fx } = harness();
    sink.push(snap(stalled));
    sink.push(snap(stalled));
    expect(fx.recoverMediaPath).not.toHaveBeenCalled();

    sink.push(snap(stalled));
    expect(fx.recoverMediaPath).toHaveBeenCalledTimes(1);

    sink.push(snap(stalled));
    sink.push(snap(stalled));
    expect(fx.recoverMediaPath).toHaveBeenCalledTimes(1); // still once for this stall
  });

  it("reports the path flowing again only after a stall that triggered recovery", () => {
    const { sink, fx } = harness();
    sink.push(snap(stalled));
    sink.push(snap(stalled));
    sink.push(snap()); // recovered after 2 — below the threshold
    expect(fx.mediaPathFlowing).not.toHaveBeenCalled();

    sink.push(snap(stalled));
    sink.push(snap(stalled));
    sink.push(snap(stalled));
    sink.push(snap());
    expect(fx.mediaPathFlowing).toHaveBeenCalledTimes(1);
  });

  it("re-arms after recovery, so a second stall recovers again", () => {
    const { sink, fx } = harness();
    for (let i = 0; i < 3; i++) sink.push(snap(stalled));
    sink.push(snap());
    for (let i = 0; i < 3; i++) sink.push(snap(stalled));
    expect(fx.recoverMediaPath).toHaveBeenCalledTimes(2);
  });

  it("treats a window with fps but no bitrate as flowing", () => {
    const { sink, fx } = harness();
    sink.push(snap({ bitrateKbps: 0, fps: 12 }));
    sink.push(snap({ bitrateKbps: 0, fps: 12 }));
    sink.push(snap({ bitrateKbps: 0, fps: 12 }));
    expect(fx.recoverMediaPath).not.toHaveBeenCalled();
  });
});

describe("freeze tracing", () => {
  it("emits on a changed non-zero count, carrying the window's real longest interval", () => {
    const { sink, fx } = harness();
    sink.push(snap({ freezeCount: 2 }));
    // 18 ms is the measured max from the window, not 1000/freezeCount.
    expect(fx.emitFreezeDetected).toHaveBeenCalledWith(18, false);
  });

  it("passes null rather than inventing a gap when the window had no cadence", () => {
    const { sink, fx } = harness();
    const noCadence = snap({ freezeCount: 1 });
    noCadence.presentCadence = { ...noCadence.presentCadence, n: 0, maxMs: null };
    sink.push(noCadence);
    expect(fx.emitFreezeDetected).toHaveBeenCalledWith(null, false);
  });

  it("does not re-emit while the count is unchanged", () => {
    const { sink, fx } = harness();
    sink.push(snap({ freezeCount: 2 }));
    sink.push(snap({ freezeCount: 2 }));
    expect(fx.emitFreezeDetected).toHaveBeenCalledTimes(1);
  });

  it("marks a hidden tab so its freezes are not blamed on the stream", () => {
    const { sink, fx } = harness({ isHidden: () => true });
    sink.push(snap({ freezeCount: 1 }));
    expect(fx.emitFreezeDetected).toHaveBeenCalledWith(18, true);
  });

  it("stays quiet on a zero count", () => {
    const { sink, fx } = harness();
    sink.push(snap({ freezeCount: 0 }));
    expect(fx.emitFreezeDetected).not.toHaveBeenCalled();
  });
});

describe("decode-failure round trip", () => {
  /** Bytes growing while frames stay at zero — a stream arriving that this
   *  client cannot decode. */
  function undecodable(i: number) {
    return snap({
      framesDecodedTotal: 0,
      bytesReceivedTotal: 100_000 * (i + 1),
      fps: 0,
      bitrateKbps: 800,
    });
  }

  it("latches exactly once, however many further windows arrive", () => {
    const { sink, fx, advance } = harness();
    for (let i = 0; i < 12; i++) {
      advance(1000);
      sink.push(undecodable(i));
    }
    expect(fx.setDecodeFailed).toHaveBeenCalledTimes(1);
  });

  it("never latches while frames are decoding", () => {
    const { sink, fx, advance } = harness();
    for (let i = 0; i < 12; i++) {
      advance(1000);
      sink.push(snap({ framesDecodedTotal: 100 * (i + 1), bytesReceivedTotal: 100_000 * (i + 1) }));
    }
    expect(fx.setDecodeFailed).not.toHaveBeenCalled();
  });

  it("reports the client_unsupported verdict when telemetry classifies it", () => {
    const { sink, fx } = harness();
    sink.push(snap({ clientHealth: "client_unsupported" }));
    expect(fx.onClientUnsupported).toHaveBeenCalledTimes(1);
  });

  it("does not report client_unsupported on the tick that merely latches the failure", () => {
    // The latch happens first; the classifier only reports it from the NEXT
    // poll on, which is why the page's verdict is always one tick behind.
    const { sink, fx, advance } = harness();
    for (let i = 0; i < 12; i++) {
      advance(1000);
      sink.push(undecodable(i)); // clientHealth stays "smooth" in these fixtures
    }
    expect(fx.setDecodeFailed).toHaveBeenCalledTimes(1);
    expect(fx.onClientUnsupported).not.toHaveBeenCalled();
  });
});
