import { describe, expect, it } from "vitest";
import { percentileFraction, recommendation } from "./sessionSummary";

describe("session summary", () => {
  it("computes bounded nearest-rank percentiles", () => {
    expect(percentileFraction([60, 58, 59, 30], 0.5)).toBe(58);
    expect(percentileFraction([60, 58, 59, 30], 0.95)).toBe(60);
    expect(percentileFraction([], 0.5)).toBeNull();
  });

  it("recommends a lower profile only for a sustained cadence miss", () => {
    expect(recommendation(50, 60)).toContain("lower");
    expect(recommendation(59, 60)).toContain("sustained");
  });
});
