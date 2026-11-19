import { describe, expect, it } from "vitest";
import { summarizePresentWindow } from "./presentCadence";
import {
  feedPresentedFrame,
  feedPresentedFrames,
  presentedDisplayMs,
  presentedFrameKey,
  INITIAL_FRAME_DEDUP_STATE,
  INITIAL_PRESENT_FRAME_METADATA_STATE,
  type FrameDedupState,
  type PresentFrameMetadataState,
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

// #686: Firefox reports mediaTime 0 on every WebRTC frame (the spec allows it) and
// carries the frame's identity in rtpTimestamp. Traces are shaped like Firefox 142 and
// Chrome 14x loopback captures (320x240, 30 fps source).
describe("presentedFrameKey", () => {
  type Frame = { mediaTime: number; rtpTimestamp?: number; nowMs: number };
  const run = (frames: Frame[], key: (f: Frame) => number | undefined): number[] => {
    let state: FrameDedupState = INITIAL_FRAME_DEDUP_STATE;
    const out: number[] = [];
    for (const f of frames) {
      const r = feedPresentedFrame(state, key(f), f.nowMs);
      state = r.nextState;
      if (r.intervalMs != null) out.push(r.intervalMs);
    }
    return out;
  };
  const viaKey = (f: Frame) => presentedFrameKey(f);
  const viaMediaTime = (f: Frame) => f.mediaTime;

  const firefox = (gapsMs: number[]): Frame[] => {
    let nowMs = 0;
    let rtp = 1347462624;
    return [0, ...gapsMs].map((gap) => {
      nowMs += gap;
      rtp += Math.round(gap * 90);
      return { mediaTime: 0, rtpTimestamp: rtp, nowMs };
    });
  };

  it("Firefox: a constant mediaTime 0 no longer empties the window", () => {
    const frames = firefox(Array(40).fill(33));
    expect(run(frames, viaMediaTime)).toEqual([]); // the pre-fix behaviour
    const intervals = run(frames, viaKey);
    expect(intervals).toHaveLength(40);
    const c = summarizePresentWindow(intervals, null, null);
    expect(c.n).toBe(40);
    expect(c.fpsFromMedian).toBeCloseTo(1000 / 33, 5);
    expect(c.sdMs).toBe(0);
  });

  it("Firefox: a stalled stream shows as long frames", () => {
    const gaps = Array(30).fill(33);
    gaps.splice(10, 0, 400, 400, 400);
    const c = summarizePresentWindow(run(firefox(gaps), viaKey), null, null);
    expect(c.longFrames).toBe(3);
    expect(c.sdMs).toBeGreaterThan(60);
  });

  it("Firefox: a repeated tick for the same RTP frame is still dropped", () => {
    const frames = firefox([33, 33]);
    frames.splice(2, 0, { ...frames[1]!, nowMs: 50 });
    expect(run(frames, viaKey)).toEqual([33, 33]);
  });

  it("Chrome: the same trace gives the same intervals as keying on mediaTime alone", () => {
    // First frame mediaTime 0, vsync duplicates of a frame, one stalled frame.
    const rtp = 1595657622;
    const chrome: Frame[] = [
      { mediaTime: 0, rtpTimestamp: rtp, nowMs: 0 },
      { mediaTime: 0, rtpTimestamp: rtp, nowMs: 10 },
      { mediaTime: 0.0167, rtpTimestamp: rtp + 1503, nowMs: 17 },
      { mediaTime: 0.0167, rtpTimestamp: rtp + 1503, nowMs: 27 },
      { mediaTime: 0.0333, rtpTimestamp: rtp + 3000, nowMs: 34 },
      { mediaTime: 0.0667, rtpTimestamp: rtp + 6003, nowMs: 67 },
    ];
    expect(run(chrome, viaKey)).toEqual([17, 17, 33]);
    expect(run(chrome, viaKey)).toEqual(run(chrome, viaMediaTime));
  });

  it("passes mediaTime through when there is no rtpTimestamp or no metadata", () => {
    expect(presentedFrameKey({ mediaTime: 0 })).toBe(0);
    expect(presentedFrameKey({ mediaTime: 0.5, rtpTimestamp: 9 })).toBe(0.5);
    expect(presentedFrameKey(undefined)).toBeUndefined();
  });
});

// #552: presentation pacing from RVFC frame metadata. A callback the UA coalesced
// over several presentations must count every presented frame and contribute one
// per-frame interval, never one callback-to-callback span read as a long frame.
describe("feedPresentedFrames (RVFC metadata, #552)", () => {
  type Frame = { presentedFrames: number; displayMs: number };
  const run = (frames: Frame[]) => {
    let state: PresentFrameMetadataState = INITIAL_PRESENT_FRAME_METADATA_STATE;
    const out: { frames: number; intervalMs: number | null }[] = [];
    for (const f of frames) {
      const r = feedPresentedFrames(state, f.presentedFrames, f.displayMs);
      state = r.nextState;
      out.push({ frames: r.frames, intervalMs: r.intervalMs });
    }
    return out;
  };

  it("a presentedFrames delta of 2 in one callback counts 2 frames and one 16.67 ms interval", () => {
    // Seeded at presentedFrames 10 / display 1000; the next callback coalesced
    // two presentations, 33.34 ms apart on the display clock.
    const r = run([
      { presentedFrames: 10, displayMs: 1000 },
      { presentedFrames: 12, displayMs: 1033.34 },
    ]);
    expect(r[0]).toEqual({ frames: 0, intervalMs: null });
    expect(r[1]!.frames).toBe(2);
    expect(r[1]!.intervalMs).toBeCloseTo(16.67, 6);
    // NOT 33.34: the two presented frames share the span; one long frame was the
    // callback-cadence bug #552.
    expect(r[1]!.intervalMs).not.toBeCloseTo(33.34, 6);
  });

  it("counts a deeper coalesce as its presented-frame delta with a per-frame interval", () => {
    const r = run([
      { presentedFrames: 0, displayMs: 0 },
      { presentedFrames: 3, displayMs: 50 },
    ]);
    expect(r[1]).toEqual({ frames: 3, intervalMs: 50 / 3 });
  });

  it("a duplicate tick (presentedFrames delta 0) yields no sample and preserves state", () => {
    const r = run([
      { presentedFrames: 5, displayMs: 0 },
      { presentedFrames: 5, displayMs: 10 }, // tick for the same presented frame
      { presentedFrames: 6, displayMs: 33.34 }, // next new frame: full gap, not 10 ms
    ]);
    expect(r[1]).toEqual({ frames: 0, intervalMs: null });
    expect(r[2]).toEqual({ frames: 1, intervalMs: 33.34 });
  });

  it("reseeds rather than suppressing samples when the UA counter restarts", () => {
    const r = run([
      { presentedFrames: 500, displayMs: 0 },
      { presentedFrames: 2, displayMs: 100 }, // counter restarted (new element/run)
      { presentedFrames: 3, displayMs: 116.67 },
    ]);
    expect(r[1]).toEqual({ frames: 0, intervalMs: null });
    expect(r[2]!.frames).toBe(1);
    expect(r[2]!.intervalMs).toBeCloseTo(16.67, 6);
  });

  it("a real dropped frame still reads as one long interval (delta 1)", () => {
    const r = run([
      { presentedFrames: 0, displayMs: 0 },
      { presentedFrames: 1, displayMs: 33.34 },
    ]);
    expect(r[1]).toEqual({ frames: 1, intervalMs: 33.34 });
  });

  it("the interval is the elapsed display time per frame, independent of callback run time", () => {
    // The interval is displayToDisplay / delta; nothing in this function reads a
    // callback clock, so a delayed callback cannot move it.
    const r = run([
      { presentedFrames: 0, displayMs: 1000 },
      { presentedFrames: 1, displayMs: 1016.67 },
      { presentedFrames: 2, displayMs: 1033.34 },
    ]);
    expect(r[1]!.intervalMs).toBeCloseTo(16.67, 6);
    expect(r[2]!.intervalMs).toBeCloseTo(16.67, 6);
  });
});

describe("presentedDisplayMs (#552)", () => {
  it("prefers expectedDisplayTime", () => {
    expect(presentedDisplayMs({ expectedDisplayTime: 500, presentationTime: 480 }))
      .toBe(500);
  });

  it("falls back to presentationTime when expectedDisplayTime is absent", () => {
    expect(presentedDisplayMs({ presentationTime: 480 })).toBe(480);
    expect(presentedDisplayMs({ expectedDisplayTime: Number.NaN, presentationTime: 480 }))
      .toBe(480);
  });

  it("is null when the UA supplied no display time", () => {
    expect(presentedDisplayMs({})).toBeNull();
    expect(presentedDisplayMs(undefined)).toBeNull();
  });
});
