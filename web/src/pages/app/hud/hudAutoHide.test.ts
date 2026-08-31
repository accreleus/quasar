import { describe, expect, it } from "vitest";
import { hudVisible } from "./hudAutoHide";

describe("hudVisible", () => {
  it("never hides while open or swapping", () => {
    expect(
      hudVisible({ mode: "on_capture", captured: true, open: true, swapping: false, idleMs: 99999 }),
    ).toBe(true);
    expect(
      hudVisible({ mode: "never_visible", captured: false, open: false, swapping: true, idleMs: 0 }),
    ).toBe(true);
  });

  it("on_capture hides after 4 s idle only while captured", () => {
    expect(
      hudVisible({ mode: "on_capture", captured: true, open: false, swapping: false, idleMs: 4001 }),
    ).toBe(false);
    expect(
      hudVisible({ mode: "on_capture", captured: false, open: false, swapping: false, idleMs: 4001 }),
    ).toBe(true);
  });

  it("on_capture keeps it up through the grace period", () => {
    expect(
      hudVisible({ mode: "on_capture", captured: true, open: false, swapping: false, idleMs: 3999 }),
    ).toBe(true);
  });

  it("always_visible never hides; never_visible hides whenever closed", () => {
    expect(
      hudVisible({ mode: "always_visible", captured: true, open: false, swapping: false, idleMs: 1e6 }),
    ).toBe(true);
    expect(
      hudVisible({ mode: "never_visible", captured: false, open: false, swapping: false, idleMs: 0 }),
    ).toBe(false);
  });
});
