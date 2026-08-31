import { describe, expect, it } from "vitest";
import { mean, median, percentile, stddev } from "./stats";

describe("percentile", () => {
  it("is null on an empty array", () => {
    expect(percentile([], 50)).toBeNull();
    expect(percentile([], 95)).toBeNull();
  });

  it("uses nearest-rank with a ceiling", () => {
    const v = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10];
    // ceil(0.95 * 10) - 1 = 9
    expect(percentile(v, 95)).toBe(10);
    // ceil(0.50 * 10) - 1 = 4
    expect(percentile(v, 50)).toBe(5);
  });

  it("clamps p at both ends instead of indexing out of range", () => {
    expect(percentile([3, 1, 2], 0)).toBe(1);
    expect(percentile([3, 1, 2], 100)).toBe(3);
  });

  it("does not mutate its input", () => {
    const v = [3, 1, 2];
    percentile(v, 50);
    expect(v).toEqual([3, 1, 2]);
  });
});

describe("median", () => {
  it("is null on an empty array", () => {
    expect(median([])).toBeNull();
  });

  it("takes the middle value for an odd count", () => {
    expect(median([5, 1, 3])).toBe(3);
  });

  it("averages the middle pair for an even count", () => {
    // This is the behaviour change from the three old implementations: a
    // rank-based p50 returned 2 (floor) or 3 (round/ceil), never 2.5.
    expect(median([1, 2, 3, 4])).toBe(2.5);
  });

  it("is not the same as percentile(v, 50) on an even sample", () => {
    const v = [1, 2, 3, 4];
    expect(median(v)).not.toBe(percentile(v, 50));
  });
});

describe("mean / stddev", () => {
  it("are null on an empty array", () => {
    expect(mean([])).toBeNull();
    expect(stddev([])).toBeNull();
  });

  it("compute the population statistics", () => {
    expect(mean([2, 4, 6])).toBe(4);
    // population σ of [2,4,6] = sqrt(8/3)
    expect(stddev([2, 4, 6])).toBeCloseTo(Math.sqrt(8 / 3), 10);
  });

  it("reports zero σ for a constant series", () => {
    expect(stddev([7, 7, 7, 7])).toBe(0);
  });
});
