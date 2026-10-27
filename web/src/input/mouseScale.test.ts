import { describe, expect, it } from "vitest";
import type { RegionInputWire } from "../api/me";
import { createMouseScaler, mouseGain, resolveMouseSettings, streamPerShown } from "./mouseScale";

const stream = { streamW: 1920, streamH: 1080 };

describe("resolveMouseSettings", () => {
  const wire = (input: object, defaults: object = { match_cursor: "auto", mouse_sensitivity: 1 }) =>
    ({ region: {}, input, defaults: { region: {}, input: defaults } }) as RegionInputWire;

  it("Auto is on for a desktop and off for every other kind", () => {
    const s = wire({ match_cursor: "auto" });
    expect(resolveMouseSettings(s, "desktop").match).toBe(true);
    for (const kind of ["game", "launcher", undefined] as const) {
      expect(resolveMouseSettings(s, kind).match).toBe(false);
    }
  });

  it("Always on and Off ignore the kind", () => {
    expect(resolveMouseSettings(wire({ match_cursor: "on" }), "game").match).toBe(true);
    expect(resolveMouseSettings(wire({ match_cursor: "off" }), "desktop").match).toBe(false);
  });

  it("falls back to the install default, then to Auto at 1×", () => {
    const s = wire({ match_cursor: null, mouse_sensitivity: null }, { match_cursor: "on", mouse_sensitivity: 2 });
    expect(resolveMouseSettings(s, "game")).toEqual({ match: true, sensitivity: 2 });
    expect(resolveMouseSettings(null, "desktop")).toEqual({ match: true, sensitivity: 1 });
  });

  it("keeps sensitivity inside 0.25 to 4", () => {
    expect(resolveMouseSettings(wire({ mouse_sensitivity: 40 }), "game").sensitivity).toBe(4);
    expect(resolveMouseSettings(wire({ mouse_sensitivity: 0 }), "game").sensitivity).toBe(0.25);
  });
});

describe("mouseGain", () => {
  it("is exactly 1 for a window equal to the stream, matching on", () => {
    expect(mouseGain({ match: true, sensitivity: 1 }, { ...stream, boxW: 1920, boxH: 1080 })).toEqual([1, 1]);
  });

  it("scales by stream size over shown size when the window is smaller", () => {
    expect(mouseGain({ match: true, sensitivity: 1 }, { ...stream, boxW: 960, boxH: 540 })).toEqual([2, 2]);
  });

  it("uses the shown picture, not the box, when the stream is letterboxed", () => {
    // 1920x1080 in a 960x800 box is drawn 960 wide.
    expect(streamPerShown({ ...stream, boxW: 960, boxH: 800 })).toEqual([2, 2]);
  });

  it("covers and stretches follow what is drawn", () => {
    expect(streamPerShown({ ...stream, boxW: 960, boxH: 1080, fit: "cover" })).toEqual([1, 1]);
    expect(streamPerShown({ ...stream, boxW: 960, boxH: 1080, fit: "stretch" })).toEqual([2, 1]);
  });

  it("integer scaling is drawn as contain", () => {
    expect(streamPerShown({ ...stream, boxW: 960, boxH: 800, fit: "integer" })).toEqual(
      streamPerShown({ ...stream, boxW: 960, boxH: 800, fit: "contain" }),
    );
  });

  it("matching off leaves only the sensitivity", () => {
    expect(mouseGain({ match: false, sensitivity: 1.5 }, { ...stream, boxW: 960, boxH: 540 })).toEqual([1.5, 1.5]);
  });

  it("matching comes first, then sensitivity", () => {
    expect(mouseGain({ match: true, sensitivity: 0.5 }, { ...stream, boxW: 960, boxH: 540 })).toEqual([1, 1]);
  });

  it("does not scale before the stream or window has a size", () => {
    expect(mouseGain({ match: true, sensitivity: 1 }, { streamW: 0, streamH: 0, boxW: 960, boxH: 540 })).toEqual([1, 1]);
  });
});

describe("createMouseScaler", () => {
  const run = (gain: number, deltas: number[]) => {
    const sc = createMouseScaler();
    sc.setGain(gain, gain);
    const out = { dx: 0, dy: 0 };
    return deltas.map((d) => (sc.apply(out, d, 0) ? out.dx : 0));
  };

  it("passes a delta through unchanged at gain 1, fractions included", () => {
    const sc = createMouseScaler();
    const out = { dx: 0, dy: 0 };
    expect(sc.apply(out, 0.5, -2.25)).toBe(true);
    expect(out).toEqual({ dx: 0.5, dy: -2.25 });
    expect(sc.apply(out, 0, 0)).toBe(false);
  });

  it("keeps the fractional remainder: slow movement is not lost", () => {
    // 10 steps of 1px at 0.3x is 3px in total.
    const sent = run(0.3, Array(10).fill(1));
    expect(sent.reduce((a, b) => a + b, 0)).toBe(3);
  });

  it("sends integers and drops zero results", () => {
    const sent = run(0.4, [1, 1, 1]);
    expect(sent).toEqual([0, 1, 0]);
  });

  it("the remainder carries negative motion too", () => {
    expect(run(0.3, Array(10).fill(-1)).reduce((a, b) => a + b, 0)).toBe(-3);
  });

  it("a new gain starts without the old remainder", () => {
    const sc = createMouseScaler();
    const out = { dx: 0, dy: 0 };
    sc.setGain(0.4, 0.4);
    sc.apply(out, 1, 0);
    sc.setGain(1, 1);
    sc.apply(out, 3, 0);
    expect(out.dx).toBe(3);
  });
});
