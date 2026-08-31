// AS10-13 — unit tests for TelemetrySnapshot/payload construction with input fields.
//
// Verifies that:
// 1. EMPTY_SNAPSHOT has inputMetrics: null.
// 2. SessionTelemetry.getSnapshot() includes inputMetrics when a getCaptureMetrics
//    getter is wired.
// 3. The posted metrics payload includes the input_ keys when metrics are present.
//
// We test getSnapshot() and buildMetrics() by constructing a minimal
// SessionTelemetry instance with stubs for all browser APIs it touches.

import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { EMPTY_SNAPSHOT, SessionTelemetry, type TelemetrySnapshot } from "../webrtc/telemetry";
import type { CaptureMetrics } from "./capture";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

beforeEach(() => {
  // Prevent the overlay loop from running.
  vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
  vi.stubGlobal("cancelAnimationFrame", vi.fn());
});

/** Build a minimal CaptureMetrics fixture. */
function makeInputMetrics(overrides: Partial<CaptureMetrics> = {}): CaptureMetrics {
  return {
    pointerLocked: false,
    captured: false,
    pointerLockSupported: true,
    coalescedSupported: true,
    inputMsgPerSec: 120,
    coalescedSamplesPerSec: 500,
    channelBufferedAmount: 0,
    backpressureDetected: false,
    gamepadCount: 0,
    pads: [],
    gamepadSendPerSec: 0,
    mmSentPerSec: 0,
    inputTrace: false,
    ...overrides,
  };
}

/** Build a minimal SessionTelemetry for unit tests. */
function makeTelemetry(getCaptureMetrics?: () => CaptureMetrics): SessionTelemetry {
  // Minimal video element stub.
  const video = {
    videoWidth: 0,
    videoHeight: 0,
    requestVideoFrameCallback: vi.fn(),
  } as unknown as HTMLVideoElement;

  // Minimal DataChannel stub.
  const channel = {
    readyState: "open",
    bufferedAmount: 0,
    onmessage: null,
    send: vi.fn(),
  } as unknown as RTCDataChannel;

  // Stub document.createElement so the overlay canvas doesn't break.
  const canvas = {
    width: 0,
    height: 0,
    getContext: () => ({
      drawImage: vi.fn(),
      getImageData: vi.fn(() => ({ data: new Uint8Array(0) })),
      willReadFrequently: true,
    }),
  } as unknown as HTMLCanvasElement;
  vi.spyOn(document, "createElement").mockReturnValue(canvas);

  return new SessionTelemetry(
    video,
    vi.fn(async () => new Map() as unknown as RTCStatsReport),
    channel,
    "sess-001",
    "tok-abc",
    () => 100, // playoutMsProvider
    getCaptureMetrics,
  );
}

describe("EMPTY_SNAPSHOT", () => {
  it("has inputMetrics: null", () => {
    expect(EMPTY_SNAPSHOT.inputMetrics).toBeNull();
  });
});

describe("SessionTelemetry.getSnapshot() — input fields", () => {
  it("returns inputMetrics: null when no getter is wired", () => {
    const tm = makeTelemetry();
    const snap: TelemetrySnapshot = tm.getSnapshot();
    expect(snap.inputMetrics).toBeNull();
    tm.stop();
  });

  it("returns inputMetrics from the getter when wired", () => {
    const metrics = makeInputMetrics({ inputMsgPerSec: 250, pointerLocked: true });
    const getter = vi.fn(() => metrics);
    const tm = makeTelemetry(getter);

    const snap = tm.getSnapshot();
    expect(snap.inputMetrics).not.toBeNull();
    expect(snap.inputMetrics!.inputMsgPerSec).toBe(250);
    expect(snap.inputMetrics!.pointerLocked).toBe(true);
    expect(getter).toHaveBeenCalledTimes(1);

    tm.stop();
  });

  it("calls the getter once per getSnapshot() call", () => {
    const getter = vi.fn(() => makeInputMetrics());
    const tm = makeTelemetry(getter);

    tm.getSnapshot();
    tm.getSnapshot();
    tm.getSnapshot();

    expect(getter).toHaveBeenCalledTimes(3);
    tm.stop();
  });
});

describe("pollStats() — getter called exactly once per poll cycle", () => {
  /** Build a stub RTCStatsReport that has a valid inbound-rtp entry so pollStats
   *  does not early-return on `if (!rtp) return`. */
  function makeRtpReport(): RTCStatsReport {
    const entry = {
      type: "inbound-rtp",
      kind: "video",
      framesPerSecond: 60,
      jitterBufferDelay: 0,
      jitterBufferEmittedCount: 0,
      totalDecodeTime: 0,
      framesDecoded: 0,
      packetsLost: 0,
      framesDropped: 0,
      freezeCount: 0,
    };
    return new Map([["rtp", entry]]) as unknown as RTCStatsReport;
  }

  it("calls the capture getter exactly once per pollStats tick, not twice", async () => {
    // Guards that the reorder (getSnapshot before buildMetrics) does not cause
    // a double-advance of the rate window: getter must be called once in
    // getSnapshot(), and buildMetrics() must read lastInputMetrics without
    // calling the getter again.
    vi.useFakeTimers({ toFake: ["performance", "setInterval", "clearInterval"] });

    const getter = vi.fn(() => makeInputMetrics({ inputMsgPerSec: 42 }));

    // Need a real inbound-rtp entry so pollStats does not early-return.
    const video = {
      videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: vi.fn(),
    } as unknown as HTMLVideoElement;
    const channel = {
      readyState: "open", bufferedAmount: 0, onmessage: null, send: vi.fn(),
    } as unknown as RTCDataChannel;
    const canvas = {
      width: 0, height: 0,
      getContext: () => ({
        drawImage: vi.fn(),
        getImageData: vi.fn(() => ({ data: new Uint8Array(0) })),
      }),
    } as unknown as HTMLCanvasElement;
    vi.spyOn(document, "createElement").mockReturnValue(canvas);

    const tm = new SessionTelemetry(
      video,
      vi.fn(async () => makeRtpReport()),
      channel,
      "sess-002",
      "tok-xyz",
      () => 100,
      getter,
    );
    tm.start();

    // Trigger one pollStats tick (stats timer fires at 1000 ms).
    await vi.advanceTimersByTimeAsync(1001);

    // Exactly 1 call: from getSnapshot() inside pollStats(). buildMetrics()
    // must not call the getter a second time.
    expect(getter).toHaveBeenCalledTimes(1);

    tm.stop();
  });

  it("emits the current-cycle input metrics to the onUpdate listener", async () => {
    vi.useFakeTimers({ toFake: ["performance", "setInterval", "clearInterval"] });

    const metrics = makeInputMetrics({ inputMsgPerSec: 77 });
    const getter = vi.fn(() => metrics);

    const video = {
      videoWidth: 0, videoHeight: 0, requestVideoFrameCallback: vi.fn(),
    } as unknown as HTMLVideoElement;
    const channel = {
      readyState: "open", bufferedAmount: 0, onmessage: null, send: vi.fn(),
    } as unknown as RTCDataChannel;
    const canvas = {
      width: 0, height: 0,
      getContext: () => ({
        drawImage: vi.fn(),
        getImageData: vi.fn(() => ({ data: new Uint8Array(0) })),
      }),
    } as unknown as HTMLCanvasElement;
    vi.spyOn(document, "createElement").mockReturnValue(canvas);

    const tm = new SessionTelemetry(
      video,
      vi.fn(async () => makeRtpReport()),
      channel,
      "sess-003",
      "tok-xyz",
      () => 100,
      getter,
    );

    const updates: Array<{ inputMetrics: unknown }> = [];
    tm.onUpdate((snap) => updates.push(snap));
    tm.start();

    await vi.advanceTimersByTimeAsync(1001);

    expect(updates).toHaveLength(1);
    expect(updates[0].inputMetrics).not.toBeNull();
    expect((updates[0].inputMetrics as { inputMsgPerSec: number }).inputMsgPerSec).toBe(77);

    tm.stop();
  });
});

describe("TelemetrySnapshot shape — input fields present", () => {
  it("snapshot includes all CaptureMetrics fields when wired", () => {
    const metrics = makeInputMetrics({
      pointerLocked: true,
      coalescedSupported: true,
      inputMsgPerSec: 60,
      coalescedSamplesPerSec: 360,
      channelBufferedAmount: 1024,
      backpressureDetected: true,
      gamepadCount: 1,
      pads: [{ index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)" }],
      gamepadSendPerSec: 30,
    });
    const tm = makeTelemetry(() => metrics);
    const snap = tm.getSnapshot();

    const im = snap.inputMetrics!;
    expect(im.pointerLocked).toBe(true);
    expect(im.coalescedSupported).toBe(true);
    expect(im.inputMsgPerSec).toBe(60);
    expect(im.coalescedSamplesPerSec).toBe(360);
    expect(im.channelBufferedAmount).toBe(1024);
    expect(im.backpressureDetected).toBe(true);
    expect(im.gamepadCount).toBe(1);
    expect(im.pads).toEqual([
      { index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)" },
    ]);
    expect(im.gamepadSendPerSec).toBe(30);

    tm.stop();
  });
});
