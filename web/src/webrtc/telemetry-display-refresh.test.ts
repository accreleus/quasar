/**
 * #85 — when the display-refresh estimate is (re-)measured.
 *
 * The bug: the estimate was taken once inside start() — during session
 * startup, the busiest moment the main thread ever has — and then treated as a
 * permanent property of the display for the rest of the session. Only a tab
 * visibility change re-measured it. Entering fullscreen, or dragging the
 * window to a different monitor, did not.
 *
 * Counts measurements at the estimator seam rather than counting rAF ticks:
 * one measurement is ~20 ticks, so tick counts say nothing legible about how
 * many measurements ran.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { MeasurementHandle } from "./displayRefreshEstimator";

const measurements: { resolve: (hz: number | null) => void; cancelled: boolean }[] = [];

vi.mock("./displayRefreshEstimator", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./displayRefreshEstimator")>();
  return {
    ...actual,
    measureDisplayRefreshHz: (): MeasurementHandle => {
      const entry = { resolve: (_: number | null) => {}, cancelled: false };
      const result = new Promise<number | null>((res) => {
        entry.resolve = res;
      });
      measurements.push(entry);
      return {
        cancel() {
          entry.cancelled = true;
          entry.resolve(null);
        },
        result,
      };
    },
  };
});

const { SessionTelemetry } = await import("./telemetry");

function makeTelemetry() {
  const video = {
    videoWidth: 0,
    videoHeight: 0,
    requestVideoFrameCallback: () => 1,
  } as unknown as HTMLVideoElement;
  const channel = {
    readyState: "open",
    bufferedAmount: 0,
    onmessage: null,
    send: () => {},
  } as unknown as RTCDataChannel;
  return new SessionTelemetry(
    video,
    async () => new Map() as unknown as RTCStatsReport,
    channel,
    "session",
    "token",
  );
}

/** Settle the in-flight measurement so the next trigger is not coalesced away. */
async function settleLatest(hz: number | null = 60) {
  measurements[measurements.length - 1]?.resolve(hz);
  await Promise.resolve();
  await Promise.resolve();
}

/** The debounce is internal; anything past it settles every pending trigger. */
const PAST_DEBOUNCE_MS = 1_000;

beforeEach(() => {
  measurements.length = 0;
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("SessionTelemetry display-refresh re-measurement", () => {
  it("measures once at start()", () => {
    const tm = makeTelemetry();
    tm.start();
    expect(measurements).toHaveLength(1);
    tm.stop();
  });

  it("coalesces a trigger that arrives while a measurement is still in flight", () => {
    const tm = makeTelemetry();
    tm.start();
    document.dispatchEvent(new Event("fullscreenchange"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    expect(measurements).toHaveLength(1);
    tm.stop();
  });

  it("#85: re-measures on fullscreenchange", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest();

    document.dispatchEvent(new Event("fullscreenchange"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    expect(measurements).toHaveLength(2);
    tm.stop();
  });

  it("#85: re-measures on window resize (window moved to another monitor)", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest();

    window.dispatchEvent(new Event("resize"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    expect(measurements).toHaveLength(2);
    tm.stop();
  });

  it("still re-measures when the tab becomes visible again", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest();

    vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    expect(measurements).toHaveLength(2);
    tm.stop();
    vi.restoreAllMocks();
  });

  it("#85: debounces a burst of resizes into one measurement", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest();

    // A drag across a monitor edge — and entering fullscreen fires both events.
    for (let i = 0; i < 20; i++) window.dispatchEvent(new Event("resize"));
    document.dispatchEvent(new Event("fullscreenchange"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    expect(measurements).toHaveLength(2);
    tm.stop();
  });

  it("stop() detaches the triggers, so a late resize measures nothing", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest();
    tm.stop();

    window.dispatchEvent(new Event("resize"));
    document.dispatchEvent(new Event("fullscreenchange"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    expect(measurements).toHaveLength(1);
  });

  it("a null result (throttled tab) leaves the previous estimate in place", async () => {
    const tm = makeTelemetry();
    tm.start();
    await settleLatest(144);
    expect(tm.getSnapshot().displayRefreshHz).toBe(144);

    window.dispatchEvent(new Event("resize"));
    vi.advanceTimersByTime(PAST_DEBOUNCE_MS);
    await settleLatest(null);
    expect(tm.getSnapshot().displayRefreshHz).toBe(144);
    tm.stop();
  });
});
