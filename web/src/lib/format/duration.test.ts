import { describe, expect, it } from "vitest";
import { duration, durationBetween, durationMs } from "./duration";

describe("duration", () => {
  it("reads seconds under a minute", () => {
    expect(duration(8)).toBe("8s");
    expect(duration(0)).toBe("0s");
    expect(duration(59)).toBe("59s");
  });

  it("reads whole minutes under an hour", () => {
    expect(duration(47 * 60)).toBe("47m");
    expect(duration(60)).toBe("1m");
  });

  it("reads hours and minutes past an hour", () => {
    expect(duration(72 * 60)).toBe("1h 12m");
    expect(duration(185 * 60)).toBe("3h 5m");
    // The zero minute stays: "1h" and "1h 0m" would be two shapes for one fact.
    expect(duration(3600)).toBe("1h 0m");
  });

  it("rounds into the next unit rather than printing one that does not exist", () => {
    expect(duration(59.6)).toBe("1m");
    expect(duration(59 * 60 + 40)).toBe("1h 0m");
  });

  it("clamps nonsense rather than rendering it", () => {
    expect(duration(-5)).toBe("0s");
    expect(duration(Number.NaN)).toBe("0s");
  });
});

describe("durationBetween", () => {
  it("returns an empty string with no start", () => {
    expect(durationBetween(null, null)).toBe("");
    expect(durationBetween(undefined, null)).toBe("");
  });

  it("measures between two instants", () => {
    expect(durationBetween("2024-01-01T10:00:00Z", "2024-01-01T10:30:00Z")).toBe("30m");
    expect(durationBetween("2024-01-01T10:00:00Z", "2024-01-01T11:45:00Z")).toBe("1h 45m");
  });

  it("measures to now when the end is open", () => {
    const start = new Date(Date.now() - 8000).toISOString();
    expect(durationBetween(start, null)).toBe("8s");
  });
});

describe("durationMs", () => {
  it("keeps sub-second precision, which job runs need", () => {
    expect(durationMs(null)).toBe("—");
    expect(durationMs(500)).toBe("500 ms");
    expect(durationMs(1500)).toBe("1.5 s");
    expect(durationMs(150000)).toBe("2m 30s");
  });
});
