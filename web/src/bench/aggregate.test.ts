import { describe, expect, it } from "vitest";
import {
  classifyIndexDelta,
  percentile,
  summarizeWindow,
  type BenchFrame,
  type IndexDeltaCounts,
} from "./aggregate";

const NO_DELTAS: IndexDeltaCounts = { missing: 0, duplicated: 0, reordered: 0 };

const BOUNDS = { t_start_ms: 1_000, t_end_ms: 2_000, offset_ms: 0, no_image: 0, i2p_missed: 0 };

function frame(over: Partial<BenchFrame> = {}): BenchFrame {
  return {
    present_ms: 1_000,
    decoded: true,
    confidence: 0.99,
    frame_index: 1,
    host_time_ms: 900,
    render_w: 1920,
    render_h: 1080,
    g2g_ms: 100,
    offset_ms: 0,
    ...over,
  };
}

describe("classifyIndexDelta", () => {
  it("counts nothing for the first frame", () => {
    expect(classifyIndexDelta(null, 42)).toEqual(NO_DELTAS);
  });

  it("counts nothing for a consecutive index", () => {
    expect(classifyIndexDelta(10, 11)).toEqual(NO_DELTAS);
  });

  it("counts a repeat as duplicated", () => {
    expect(classifyIndexDelta(10, 10)).toEqual({ ...NO_DELTAS, duplicated: 1 });
  });

  it("counts a gap as (delta - 1) MISSING indices — not 'dropped'", () => {
    expect(classifyIndexDelta(10, 14)).toEqual({ ...NO_DELTAS, missing: 3 });
  });

  it("counts a backwards index as one reorder, never as a negative gap", () => {
    expect(classifyIndexDelta(10, 7)).toEqual({ ...NO_DELTAS, reordered: 1 });
  });
});

describe("percentile", () => {
  it("is null on an empty series rather than 0", () => {
    expect(percentile([], 50)).toBeNull();
  });

  it("uses nearest-rank and does not mutate the input", () => {
    const values = [5, 1, 4, 2, 3];
    expect(percentile(values, 50)).toBe(3);
    expect(percentile(values, 95)).toBe(5);
    expect(percentile(values, 0)).toBe(1);
    expect(values).toEqual([5, 1, 4, 2, 3]);
  });
});

describe("summarizeWindow", () => {
  it("aggregates decoded frames and carries the delta tally through", () => {
    const frames = [
      frame({ frame_index: 1, g2g_ms: 80 }),
      frame({ frame_index: 2, g2g_ms: 120 }),
      frame({ frame_index: 3, g2g_ms: 100 }),
      { present_ms: 1, decoded: false, confidence: 0, offset_ms: 0 } as BenchFrame,
    ];
    const out = summarizeWindow(frames, { missing: 2, duplicated: 1, reordered: 0 }, [55, 61], BOUNDS);
    expect(out.n).toBe(4);
    expect(out.decoded).toBe(3);
    expect(out.undecoded).toBe(1);
    expect(out.g2g_p50_ms).toBe(100);
    expect(out.g2g_p95_ms).toBe(120);
    expect(out.g2g_max_ms).toBe(120);
    expect(out.missing_indices).toBe(2);
    expect(out.duplicated).toBe(1);
    expect(out.reordered).toBe(0);
    expect(out.i2p_ms).toEqual([55, 61]);
    expect(out.render_w).toBe(1920);
    expect(out.render_h).toBe(1080);
    expect(out.offset_unknown).toBe(false);
  });

  it("marks offset_unknown when no ST-05 estimate was available", () => {
    const out = summarizeWindow([frame({ offset_ms: null })], NO_DELTAS, [], { ...BOUNDS, offset_ms: null });
    expect(out.offset_ms).toBeNull();
    expect(out.offset_unknown).toBe(true);
  });

  it("reports the render size the window ENDED at", () => {
    const frames = [
      frame({ frame_index: 1, render_w: 1920, render_h: 1080 }),
      frame({ frame_index: 2, render_w: 1280, render_h: 720 }),
    ];
    const out = summarizeWindow(frames, NO_DELTAS, [], BOUNDS);
    expect(out.render_w).toBe(1280);
    expect(out.render_h).toBe(720);
  });

  it("returns null percentiles, not zeros, when nothing decoded", () => {
    const frames: BenchFrame[] = [
      { present_ms: 1, decoded: false, confidence: 0, offset_ms: 3 },
      { present_ms: 2, decoded: false, confidence: 0, offset_ms: 3 },
    ];
    const out = summarizeWindow(frames, NO_DELTAS, [], BOUNDS);
    expect(out.decoded).toBe(0);
    expect(out.undecoded).toBe(2);
    expect(out.g2g_p50_ms).toBeNull();
    expect(out.g2g_max_ms).toBeNull();
  });

  it("copies i2p_ms so a later window cannot mutate a posted payload", () => {
    const samples = [10];
    const out = summarizeWindow([frame()], NO_DELTAS, samples, BOUNDS);
    samples.push(999);
    expect(out.i2p_ms).toEqual([10]);
  });
});

describe("summarizeWindow — the window carries its own clock (C1)", () => {
  it("stamps every window with its own start/end, frames or not", () => {
    const out = summarizeWindow([], NO_DELTAS, [], {
      t_start_ms: 5_000, t_end_ms: 6_000, offset_ms: 7, no_image: 0, i2p_missed: 0,
    });
    expect(out.t_start_ms).toBe(5_000);
    expect(out.t_end_ms).toBe(6_000);
    // t_end on the HOST clock: browser epoch + offset.
    expect(out.t_end_host_ms).toBe(6_007);
  });

  it("is emitted with n=0 for a frameless second, so a freeze is representable", () => {
    const out = summarizeWindow([], NO_DELTAS, [], BOUNDS);
    expect(out.n).toBe(0);
    expect(out.decoded).toBe(0);
    expect(out.undecoded).toBe(0);
    expect(out.g2g_p50_ms).toBeNull();
    expect(out.last_host_time_ms).toBeNull();
  });

  it("prefers the last decoded frame's own host_time_ms as the exact join key", () => {
    const out = summarizeWindow(
      [frame({ frame_index: 1, host_time_ms: 111 }), frame({ frame_index: 2, host_time_ms: 222 })],
      NO_DELTAS, [], BOUNDS,
    );
    expect(out.last_host_time_ms).toBe(222);
  });

  it("leaves t_end_host_ms null when the clock offset is unmeasured", () => {
    const out = summarizeWindow([], NO_DELTAS, [], { ...BOUNDS, offset_ms: null });
    expect(out.t_end_host_ms).toBeNull();
    expect(out.offset_unknown).toBe(true);
  });

  it("carries no_image and i2p_missed out of band", () => {
    const out = summarizeWindow([], NO_DELTAS, [], { ...BOUNDS, no_image: 4, i2p_missed: 2 });
    expect(out.no_image).toBe(4);
    expect(out.i2p_missed).toBe(2);
  });
});
