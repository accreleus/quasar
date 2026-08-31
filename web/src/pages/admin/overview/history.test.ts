import { describe, expect, it } from "vitest";
import { HISTORY_LIMIT, pushSamples, seriesFor } from "./history";

describe("pushSamples", () => {
  it("appends one value per key", () => {
    const a = pushSamples({}, { live: 3, "fps:s1": 60 });
    const b = pushSamples(a, { live: 4, "fps:s1": 59 });
    expect(b.live).toEqual([3, 4]);
    expect(b["fps:s1"]).toEqual([60, 59]);
  });

  it("keeps only the newest HISTORY_LIMIT samples of a key", () => {
    let series = {};
    for (let i = 0; i < HISTORY_LIMIT + 5; i += 1) series = pushSamples(series, { live: i });
    expect(series).toHaveProperty("live");
    const live = seriesFor(series, "live");
    expect(live).toHaveLength(HISTORY_LIMIT);
    expect(live[0]).toBe(5);
    expect(live[HISTORY_LIMIT - 1]).toBe(HISTORY_LIMIT + 4);
  });

  it("forgets a key the new sample does not mention — a session that ended is not a series", () => {
    const a = pushSamples({}, { "fps:s1": 60, "fps:s2": 30 });
    const b = pushSamples(a, { "fps:s1": 61 });
    expect(Object.keys(b)).toEqual(["fps:s1"]);
  });

  it("holds a key whose sample is missing rather than inventing a value", () => {
    const a = pushSamples({}, { "fps:s1": 60 });
    const b = pushSamples(a, { "fps:s1": undefined });
    expect(b["fps:s1"]).toEqual([60]);
  });

  it("ignores a non-finite sample", () => {
    const a = pushSamples({}, { live: Number.NaN });
    expect(seriesFor(a, "live")).toEqual([]);
  });

  it("leaves the previous series untouched", () => {
    const a = pushSamples({}, { live: 1 });
    pushSamples(a, { live: 2 });
    expect(a.live).toEqual([1]);
  });

  it("seriesFor answers with an empty array for a key it has never seen", () => {
    expect(seriesFor({}, "nope")).toEqual([]);
  });
});
