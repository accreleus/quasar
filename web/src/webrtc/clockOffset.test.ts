import { describe, it, expect } from "vitest";
import {
  estimateClockOffset,
  MIN_CLOCK_SAMPLES,
  type ClockSample,
} from "./clockOffset";

function samples(pairs: Array<[rtt: number, offset: number]>): ClockSample[] {
  return pairs.map(([rtt, offset]) => ({ rtt, offset }));
}

describe("estimateClockOffset", () => {
  it("returns null until there are enough samples (unmeasured, never offset 0)", () => {
    const few = samples(
      Array.from({ length: MIN_CLOCK_SAMPLES - 1 }, (_, i) => [10 + i, 100] as [number, number]),
    );
    expect(estimateClockOffset(few)).toBeNull();
    expect(estimateClockOffset([])).toBeNull();
  });

  it("uses the min-RTT sample's offset and minRtt/2 as uncertainty", () => {
    // The lowest-RTT sample (rtt=10 → offset=100) is the least-corrupted estimate.
    // Other samples have larger RTTs and skewed offsets that must be ignored.
    const s = samples([
      [40, 80],
      [30, 130],
      [10, 100], // min RTT → its offset is the estimate
      [25, 90],
      [50, 70],
      [18, 110],
      [22, 95],
      [35, 120],
      [60, 60],
    ]);
    const est = estimateClockOffset(s);
    expect(est).not.toBeNull();
    expect(est!.clientOffsetMs).toBe(100);
    expect(est!.uncertaintyMs).toBe(5); // minRtt(10) / 2
  });

  it("treats offset 0 as a real measured value, not a default", () => {
    // A genuinely-zero offset (perfectly aligned clocks) must round-trip as 0, with
    // a real uncertainty — distinct from the unmeasured (null) case.
    const s = samples(Array.from({ length: MIN_CLOCK_SAMPLES }, () => [12, 0] as [number, number]));
    const est = estimateClockOffset(s);
    expect(est).not.toBeNull();
    expect(est!.clientOffsetMs).toBe(0);
    expect(est!.uncertaintyMs).toBe(6);
  });

  it("ignores non-finite samples when selecting the minimum", () => {
    const s: ClockSample[] = [
      { rtt: NaN, offset: 999 },
      { rtt: Infinity, offset: -999 },
      ...samples([
        [20, 50],
        [14, 60], // min finite RTT
        [30, 40],
        [22, 55],
        [40, 30],
        [18, 58],
        [26, 45],
        [33, 35],
      ]),
    ];
    const est = estimateClockOffset(s);
    expect(est).not.toBeNull();
    expect(est!.clientOffsetMs).toBe(60);
    expect(est!.uncertaintyMs).toBe(7);
  });
});
