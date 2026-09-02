/**
 * #85 — the display-cadence pill's predicate.
 *
 * The bug it pins: a session at 2560×1440@120 on a 120 Hz+ display showed
 * "61 Hz display · frames dropped" while the same pane reported
 * `drops / freezes  0 / 0` and `fps (recv) 119`.
 */
import { describe, it, expect } from "vitest";
import {
  displayHzWarning,
  parseTierFps,
  DROPPED_WINDOWS_REQUIRED,
  DISPLAY_HZ_TOLERANCE,
} from "./displayHzWarning";

describe("parseTierFps", () => {
  it("reads the fps off a tier string", () => {
    expect(parseTierFps("2560×1440@120")).toBe(120);
    expect(parseTierFps("1920×1080@60")).toBe(60);
  });

  it("is null for anything that isn't a tier with an fps", () => {
    expect(parseTierFps(undefined)).toBeNull();
    expect(parseTierFps(null)).toBeNull();
    expect(parseTierFps("")).toBeNull();
    expect(parseTierFps("1920×1080")).toBeNull();
    expect(parseTierFps("1920×1080@")).toBeNull();
    expect(parseTierFps("1920×1080@abc")).toBeNull();
  });
});

describe("displayHzWarning", () => {
  const dropping = DROPPED_WINDOWS_REQUIRED;

  it("warns when the display is genuinely slower AND frames are dropping", () => {
    expect(displayHzWarning({ streamFps: 120, displayHz: 60, droppedWindows: dropping })).toEqual({
      displayHz: 60,
      streamFps: 120,
    });
  });

  it("#85: stays silent while nothing is being dropped, however big the gap", () => {
    expect(displayHzWarning({ streamFps: 120, displayHz: 61, droppedWindows: 0 })).toBeNull();
  });

  it("#85: needs the drops sustained, not a single window", () => {
    expect(
      displayHzWarning({ streamFps: 120, displayHz: 60, droppedWindows: dropping - 1 }),
    ).toBeNull();
  });

  it("#85: absorbs the estimator's rounding rather than warning about it", () => {
    // A true 120 Hz panel routinely measures 119; that is not a display limit.
    expect(
      displayHzWarning({ streamFps: 120, displayHz: 119, droppedWindows: dropping }),
    ).toBeNull();
    // Exactly at the tolerance edge is still silent...
    expect(
      displayHzWarning({
        streamFps: 120,
        displayHz: 120 - DISPLAY_HZ_TOLERANCE,
        droppedWindows: dropping,
      }),
    ).toBeNull();
    // ...one Hz past it is not.
    expect(
      displayHzWarning({
        streamFps: 120,
        displayHz: 120 - DISPLAY_HZ_TOLERANCE - 1,
        droppedWindows: dropping,
      }),
    ).not.toBeNull();
  });

  it("stays silent when the display is faster than the stream", () => {
    expect(displayHzWarning({ streamFps: 60, displayHz: 144, droppedWindows: dropping })).toBeNull();
  });

  it("stays silent until both the tier and a measurement are known", () => {
    expect(displayHzWarning({ streamFps: null, displayHz: 60, droppedWindows: dropping })).toBeNull();
    expect(displayHzWarning({ streamFps: 120, displayHz: null, droppedWindows: dropping })).toBeNull();
  });
});
