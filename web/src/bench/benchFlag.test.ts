import { describe, expect, it } from "vitest";
import { isBenchModeEnabled } from "./benchFlag";

describe("isBenchModeEnabled", () => {
  it("is OFF by default — no query, no storage", () => {
    expect(isBenchModeEnabled("", null)).toBe(false);
  });

  it("is OFF for an unrelated query string", () => {
    expect(isBenchModeEnabled("?playout=50", null)).toBe(false);
  });

  it("is ON for ?bench=1 (how the harness opens the peer page)", () => {
    expect(isBenchModeEnabled("?bench=1", null)).toBe(true);
    expect(isBenchModeEnabled("?playout=50&bench=1", null)).toBe(true);
    expect(isBenchModeEnabled("?bench=true", null)).toBe(true);
  });

  it("is ON for the sticky localStorage flag", () => {
    expect(isBenchModeEnabled("", "1")).toBe(true);
    expect(isBenchModeEnabled("", "true")).toBe(true);
  });

  it("lets ?bench=0 override a sticky flag", () => {
    expect(isBenchModeEnabled("?bench=0", "1")).toBe(false);
    expect(isBenchModeEnabled("?bench=false", "1")).toBe(false);
  });

  it("treats an unrecognised value as OFF, never as ON", () => {
    expect(isBenchModeEnabled("?bench=yes", null)).toBe(false);
    expect(isBenchModeEnabled("", "yes")).toBe(false);
  });
});
