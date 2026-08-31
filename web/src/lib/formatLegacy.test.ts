import { describe, expect, it } from "vitest";
import { fmtClockTime, fmtDate, fmtTime } from "./formatLegacy";

describe("fmtDate", () => {
  it("returns em-dash for null", () => {
    expect(fmtDate(null)).toBe("—");
  });

  it("returns em-dash for undefined", () => {
    expect(fmtDate(undefined)).toBe("—");
  });

  it("returns em-dash for empty string", () => {
    expect(fmtDate("")).toBe("—");
  });

  it("formats a valid ISO date string", () => {
    // We only verify the output contains the year (locale-agnostic in CI).
    const result = fmtDate("2024-06-15T00:00:00Z");
    expect(result).toContain("2024");
    expect(result).not.toBe("—");
  });
});

describe("fmtTime", () => {
  it("returns em-dash for null, undefined, and empty string", () => {
    expect(fmtTime(null)).toBe("—");
    expect(fmtTime(undefined)).toBe("—");
    expect(fmtTime("")).toBe("—");
  });

  it("formats a valid ISO timestamp with hour/minute/second (locale-agnostic in CI)", () => {
    const result = fmtTime("2024-06-15T10:30:45Z");
    expect(result).not.toBe("—");
    // hour:minute:second, each 2 digits, regardless of 12h/24h locale rendering.
    expect(result).toMatch(/\d{2}:\d{2}:\d{2}/);
  });
});

describe("fmtClockTime", () => {
  it("returns em-dash for null, undefined, and empty string", () => {
    expect(fmtClockTime(null)).toBe("—");
    expect(fmtClockTime(undefined)).toBe("—");
    expect(fmtClockTime("")).toBe("—");
  });

  it("formats a valid ISO timestamp as a locale time string", () => {
    const result = fmtClockTime("2024-06-15T10:30:45Z");
    expect(result).not.toBe("—");
    expect(result).toBe(new Date("2024-06-15T10:30:45Z").toLocaleTimeString());
  });
});
