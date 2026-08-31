import { describe, expect, it } from "vitest";
import { summarizePresentWindow } from "./presentCadence";

/** A window of `count` intervals at `hz`, with `doubled` of them at 2×. */
function beatWindow(hz: number, count: number, doubled: number): number[] {
  const nominal = 1000 / hz;
  const out: number[] = [];
  for (let i = 0; i < count; i++) {
    out.push(i < doubled ? nominal * 2 : nominal);
  }
  return out;
}

describe("summarizePresentWindow", () => {
  it("reports nulls (and the honest n) below the minimum sample count", () => {
    const c = summarizePresentWindow([8.3, 8.3, 8.3, 8.3], 120, 120);
    expect(c.n).toBe(4);
    expect(c.medianMs).toBeNull();
    expect(c.meanMs).toBeNull();
    expect(c.fpsFromMedian).toBeNull();
    expect(c.doubledFraction).toBeNull();
    expect(c.longFrames).toBeNull();
    expect(c.inherentBeat).toBe(false);
  });

  it("summarises a clean 120 Hz window", () => {
    const c = summarizePresentWindow(beatWindow(120, 120, 0), 120, 120);
    expect(c.n).toBe(120);
    expect(c.medianMs).toBeCloseTo(1000 / 120, 6);
    expect(c.fpsFromMedian).toBeCloseTo(120, 6);
    expect(c.fpsFromMean).toBeCloseTo(120, 6);
    expect(c.doubledFraction).toBe(0);
    expect(c.longFrames).toBe(0);
    expect(c.sdMs).toBeCloseTo(0, 9);
    expect(c.driftMs).toBeCloseTo(0, 9);
    expect(c.inherentBeat).toBe(true);
  });

  it("is the 2026-08-22 case: 12% doubled drags the mean, not the median", () => {
    // 106 intervals of 8.33 ms + 14 of 16.67 ms — exactly the shape that made
    // the panel read "fps (shown) 88-108" on a healthy 1440p120 h265 session.
    const c = summarizePresentWindow(beatWindow(120, 120, 14), 120, 120);

    expect(c.fpsFromMedian).toBeCloseTo(120, 6);
    expect(c.fpsFromMean).toBeGreaterThan(105);
    expect(c.fpsFromMean).toBeLessThan(110);
    expect(c.doubledFraction).toBeCloseTo(14 / 120, 6);
    expect(c.longFrames).toBe(0);
    expect(c.maxMs).toBeCloseTo(1000 / 60, 6);
    // The whole point: the window is fine, and the summary says so.
    expect(c.inherentBeat).toBe(true);
  });

  it("refuses to call a real stall an inherent beat", () => {
    const iv = beatWindow(120, 120, 0);
    iv[40] = 200;
    const c = summarizePresentWindow(iv, 120, 120);

    expect(c.longFrames).toBe(1);
    expect(c.maxMs).toBe(200);
    expect(c.inherentBeat).toBe(false);
    // The median is still 120 fps — which is exactly why longFrames has to be
    // reported beside it rather than folded into a single scalar.
    expect(c.fpsFromMedian).toBeCloseTo(120, 6);
  });

  it("lets the median move once the doubling stops being occasional", () => {
    // Half the window doubled is no longer a beat around 120 fps: the stream is
    // presenting at 80. The median says 80 — which is the honest reading, and
    // the reason inherentBeat is a claim about the SHAPE, never a health verdict.
    const c = summarizePresentWindow(beatWindow(120, 120, 60), 120, 120);
    expect(c.fpsFromMedian).toBeCloseTo(80, 6);
    expect(c.driftMs).toBeCloseTo(12.5 - 1000 / 120, 6);
  });

  it("refuses the beat claim when source fps and display Hz differ", () => {
    // 60 fps source on a 120 Hz panel: a doubled interval there means something
    // else, so the beat explanation must not be offered.
    const c = summarizePresentWindow(beatWindow(60, 60, 6), 120, 60);
    expect(c.inherentBeat).toBe(false);
    // …and the drift against the panel is reported rather than hidden.
    expect(c.driftMs).toBeCloseTo(1000 / 60 - 1000 / 120, 6);
  });

  it("has no drift and no beat claim without a display Hz", () => {
    const c = summarizePresentWindow(beatWindow(120, 120, 0), null, 120);
    expect(c.driftMs).toBeNull();
    expect(c.inherentBeat).toBe(false);
  });

  it("has no beat claim without a source fps", () => {
    const c = summarizePresentWindow(beatWindow(120, 120, 0), 120, null);
    expect(c.inherentBeat).toBe(false);
  });

  it("tolerates a 59.94 vs 60 mismatch", () => {
    const c = summarizePresentWindow(beatWindow(60, 60, 3), 60, 59.94);
    expect(c.inherentBeat).toBe(true);
  });
});
