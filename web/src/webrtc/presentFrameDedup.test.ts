import { describe, expect, it } from "vitest";
import {
  feedPresentedFrame,
  INITIAL_FRAME_DEDUP_STATE,
  type FrameDedupState,
} from "./presentFrameDedup";

/**
 * Fold a whole callback sequence through feedPresentedFrame(), collecting the
 * non-null intervals — mirrors how telemetry.ts's onDisplayedFrame() drives it.
 */
function runSequence(
  samples: Array<{ mediaTime: number | null | undefined; nowMs: number }>,
): number[] {
  let state: FrameDedupState = INITIAL_FRAME_DEDUP_STATE;
  const intervals: number[] = [];
  for (const { mediaTime, nowMs } of samples) {
    const { intervalMs, nextState } = feedPresentedFrame(state, mediaTime, nowMs);
    state = nextState;
    if (intervalMs != null) intervals.push(intervalMs);
  }
  return intervals;
}

/**
 * Simulate a source running at `sourceFps` presented on a display refreshing at
 * `displayHz`, for `frames` distinct source frames. Vsync ticks occur every
 * 1000/displayHz ms; a new distinct video frame becomes available roughly every
 * 1000/sourceFps ms, and every vsync tick invokes the RVFC callback — carrying
 * the CURRENT frame's mediaTime, which repeats across ticks until a new frame
 * lands. This is the exact mechanism #263 describes for a 98 Hz display.
 */
function simulateVsyncTicks(
  sourceFps: number,
  displayHz: number,
  frames: number,
): Array<{ mediaTime: number; nowMs: number }> {
  const vsyncMs = 1000 / displayHz;
  const frameMs = 1000 / sourceFps;
  const totalMs = frames * frameMs;
  const out: Array<{ mediaTime: number; nowMs: number }> = [];
  let mediaTime = 0; // seconds, the currently-available frame's presentation timestamp
  let nextFrameAtMs = frameMs;
  for (let t = vsyncMs; t <= totalMs; t += vsyncMs) {
    while (t >= nextFrameAtMs) {
      mediaTime = nextFrameAtMs / 1000;
      nextFrameAtMs += frameMs;
    }
    out.push({ mediaTime, nowMs: t });
  }
  return out;
}

describe("feedPresentedFrame", () => {
  it("first callback produces no interval (nothing to measure from)", () => {
    const { intervalMs, nextState } = feedPresentedFrame(
      INITIAL_FRAME_DEDUP_STATE,
      0,
      100,
    );
    expect(intervalMs).toBeNull();
    expect(nextState).toEqual({ lastMediaTime: 0, lastNow: 100 });
  });

  it("a distinct mediaTime produces the elapsed interval", () => {
    const s1 = feedPresentedFrame(INITIAL_FRAME_DEDUP_STATE, 0, 0);
    const s2 = feedPresentedFrame(s1.nextState, 1 / 60, 16.67);
    expect(s2.intervalMs).toBeCloseTo(16.67, 6);
  });

  it("a duplicate mediaTime (same frame re-presented) yields no sample and preserves state", () => {
    const s1 = feedPresentedFrame(INITIAL_FRAME_DEDUP_STATE, 5, 0);
    const s2 = feedPresentedFrame(s1.nextState, 5, 10);
    expect(s2.intervalMs).toBeNull();
    expect(s2.nextState).toEqual(s1.nextState);
  });

  it("a duplicate tick does not truncate the eventual distinct interval", () => {
    // frame at t=0, a duplicate vsync tick at t=10 (skipped), the next distinct
    // frame at t=20 — the recorded interval must span the FULL 20ms gap, not
    // just 10ms from the duplicate tick.
    const seq = [
      { mediaTime: 0, nowMs: 0 },
      { mediaTime: 0, nowMs: 10 }, // duplicate — same frame, no new sample
      { mediaTime: 1, nowMs: 20 }, // distinct — interval must be 20, not 10
    ];
    const intervals = runSequence(seq);
    expect(intervals).toEqual([20]);
  });

  it("no metadata (rAF fallback path) treats every callback as distinct — unchanged pre-#263 behavior", () => {
    const seq = [
      { mediaTime: undefined, nowMs: 0 },
      { mediaTime: undefined, nowMs: 16 },
      { mediaTime: undefined, nowMs: 33 },
    ];
    const intervals = runSequence(seq);
    expect(intervals).toEqual([16, 17]);
  });

  it("#263: 60fps source on a 98Hz display recovers ~60fps once vsync-duplicate ticks are dropped", () => {
    const seq = simulateVsyncTicks(60, 98, 300);
    const intervals = runSequence(seq);
    // Every recorded interval must be a genuine frame-to-frame gap (~16.67ms),
    // never a lone vsync tick (~10.2ms) — the #263 pollution this module removes.
    for (const iv of intervals) {
      expect(iv).toBeGreaterThan(9);
    }
    const meanMs = intervals.reduce((a, b) => a + b, 0) / intervals.length;
    const impliedFps = 1000 / meanMs;
    expect(impliedFps).toBeGreaterThan(58);
    expect(impliedFps).toBeLessThan(62);
  });

  it("#263: 60fps source on a 120Hz display (clean 2x multiple) also recovers ~60fps", () => {
    const seq = simulateVsyncTicks(60, 120, 300);
    const intervals = runSequence(seq);
    const meanMs = intervals.reduce((a, b) => a + b, 0) / intervals.length;
    expect(1000 / meanMs).toBeGreaterThan(59);
    expect(1000 / meanMs).toBeLessThan(61);
  });

  it("does not mask a real dropped frame: a genuinely missing source frame still reads as a long, low-fps interval", () => {
    // Two distinct frames 33.34ms apart (a real dropped 60fps frame), with one
    // vsync-duplicate tick in between that must be discarded, not counted as
    // evidence the frame arrived on time.
    const seq = [
      { mediaTime: 0, nowMs: 0 },
      { mediaTime: 0, nowMs: 10 }, // duplicate tick, no new frame yet
      { mediaTime: 0, nowMs: 20 }, // still a duplicate — frame dropped
      { mediaTime: 1 / 30, nowMs: 33.34 }, // the next distinct frame finally lands
    ];
    const intervals = runSequence(seq);
    expect(intervals).toEqual([33.34]);
    expect(1000 / intervals[0]!).toBeLessThan(31); // reads as ~30fps, the real drop
  });
});
