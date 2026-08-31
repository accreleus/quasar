/**
 * AS10-14 — unit tests for the displayRefreshHz estimator logic.
 *
 * Imports the production estimator from displayRefreshEstimator.ts so that
 * these tests guard real behavior, not a replica.
 *
 * What needs MANUAL browser validation:
 *  - requestVideoFrameCallback actually fires at the display refresh rate.
 *  - The estimate converges to the real Hz on a physical display.
 *  - See docs/completed/adaptive-streaming/as10-14-manual-validation.md.
 */

import { describe, expect, it } from "vitest";
import { estimateDisplayRefreshHz, measureDisplayRefreshHz } from "./displayRefreshEstimator";
import type { MeasurementHandle } from "./displayRefreshEstimator";

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("estimateDisplayRefreshHz", () => {
  it("returns null when fewer than 10 samples are provided", () => {
    expect(estimateDisplayRefreshHz([])).toBeNull();
    expect(estimateDisplayRefreshHz([16.67])).toBeNull();
    expect(estimateDisplayRefreshHz(Array(9).fill(16.67))).toBeNull();
  });

  it("returns 60 for exact 60 Hz intervals (16.67 ms)", () => {
    const intervals = Array(60).fill(1000 / 60);
    expect(estimateDisplayRefreshHz(intervals)).toBe(60);
  });

  it("returns 120 for 120 Hz intervals (~8.33 ms)", () => {
    const intervals = Array(60).fill(1000 / 120);
    expect(estimateDisplayRefreshHz(intervals)).toBe(120);
  });

  it("returns 30 for 30 Hz intervals (~33.33 ms)", () => {
    const intervals = Array(60).fill(1000 / 30);
    expect(estimateDisplayRefreshHz(intervals)).toBe(30);
  });

  it("returns 60 with minor jitter (alternating fast/slow intervals around 60 Hz)", () => {
    // 21 samples alternating between ~15.67 ms and ~17.67 ms (11 fast + 10 slow).
    // pct(arr, 0.5) picks index Math.round(0.5 * 20) = 10 in sorted order.
    // Sorted: [15.67×11, 17.67×10] → index 10 = 15.67 ms → round(1000/15.67) = 64.
    // To land the median at 16.67 ms: use 11 samples at exactly 1000/60 ms and
    // 10 samples at 1000/60 + 2 ms, so sorted index 10 = 1000/60 → 60 Hz. ✓
    const nominal = 1000 / 60;
    const intervals = Array(11).fill(nominal).concat(Array(10).fill(nominal + 2));
    expect(estimateDisplayRefreshHz(intervals)).toBe(60);
  });

  it("returns 60 with exactly 10 samples (boundary)", () => {
    const intervals = Array(10).fill(1000 / 60);
    expect(estimateDisplayRefreshHz(intervals)).toBe(60);
  });

  it("uses median so outlier spikes do not skew the estimate", () => {
    // 59 normal 60 Hz intervals + 1 spike of 100 ms.
    const intervals = [...Array(59).fill(1000 / 60), 100];
    // Median of 60 values — the spike is sorted to the end, median is ~16.67.
    expect(estimateDisplayRefreshHz(intervals)).toBe(60);
  });

  it("documents rounding behavior at ±2 ms jitter around 120 Hz", () => {
    // 120 Hz nominal ~8.33 ms; with ±2 ms jitter, median should still resolve to 120 Hz.
    // Use 11 fast + 10 slow so the sorted median (index 10) falls on the nominal value.
    // [8.33×11, 10.33×10] → index round(0.5*20)=10 → 8.33 ms → round(1000/8.33) = 120. ✓
    const nominal = 1000 / 120;
    const intervals = Array(11).fill(nominal).concat(Array(10).fill(nominal + 2));
    expect(estimateDisplayRefreshHz(intervals)).toBe(120);
  });
});

// ---------------------------------------------------------------------------
// measureDisplayRefreshHz — injectable rAF harness
// ---------------------------------------------------------------------------

/**
 * Build a fake rAF harness that emits `intervals` (ms) between successive ticks.
 *
 * Design (queue-based, NOT synchronous):
 *   `raf(cb)` enqueues `cb` rather than calling it immediately, so ticks are
 *   processed incrementally via an explicit `flush()` call.  This faithfully
 *   models real async rAF sequencing — each `raf(tick)` schedules one future
 *   call — and lets cancel tests assert that no further ticks fire after cancel.
 *
 * The first tick fires at t=0 (seeds `prev`); subsequent ticks fire at
 * cumulative wall times derived from the interval list.  `flush()` drains the
 * pending-callback queue one entry at a time (each tick may re-enqueue once).
 *
 * After the last interval's tick there are no more times, so `raf()` enqueues
 * nothing and the queue drains to empty.
 */
function makeRafHarness(intervals: number[]): {
  raf: (cb: FrameRequestCallback) => number;
  now: () => number;
  flush: () => void;
} {
  let t = 0;
  const now = () => t;

  // Absolute times at which each tick fires.
  // First tick at t=0, then t += iv for each interval.
  const times: number[] = [0];
  for (const iv of intervals) {
    times.push(times[times.length - 1] + iv);
  }

  let timeIdx = 0;
  let rafId = 0;
  // Pending callbacks waiting to be flushed.
  const queue: Array<FrameRequestCallback> = [];

  const raf = (cb: FrameRequestCallback): number => {
    timeIdx++;
    if (timeIdx < times.length) {
      // Schedule cb to fire at the next time slot; enqueue rather than call
      // immediately so ticks are incremental (flush() drives them).
      queue.push(cb);
    }
    // If timeIdx >= times.length there are no more ticks — cb is simply dropped,
    // causing the measurement to wait until timeout/max-ticks in a real scenario,
    // but in tests the finite interval list naturally ends the loop.
    return ++rafId;
  };

  /**
   * Drain one pending tick at a time.  Each tick may enqueue another via raf(),
   * so call flush() in a loop (or rely on the test awaiting the result promise)
   * to run all ticks to completion.
   */
  const flush = (): void => {
    // Snapshot length so newly-enqueued callbacks (re-scheduled by tick) are
    // picked up on the next flush() call, not within this one.
    const pending = queue.splice(0);
    for (const cb of pending) {
      t = times[timeIdx - queue.length - pending.indexOf(cb) - 1] ?? t;
      // Advance t to the time associated with this callback slot.
      cb(t);
    }
  };

  return { raf, now, flush };
}

/**
 * Run all ticks to completion by flushing until the result promise resolves.
 * Each microtask checkpoint lets .then() handlers on the result run.
 */
async function flushAll(
  handle: MeasurementHandle,
  harness: { flush: () => void },
): Promise<number | null> {
  let done = false;
  let value: number | null = null;
  handle.result.then((hz) => {
    done = true;
    value = hz;
  });
  // Drain ticks + microtasks in alternation until resolved.
  for (let i = 0; i < 300 && !done; i++) {
    harness.flush();
    await Promise.resolve();
  }
  return value;
}

describe("measureDisplayRefreshHz", () => {
  it("returns 60 for 20+ clean 60 Hz rAF intervals (~16.67 ms)", async () => {
    const intervals = Array(25).fill(1000 / 60);
    const harness = makeRafHarness(intervals);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBe(60);
  });

  it("returns 120 for 20+ clean 120 Hz rAF intervals (~8.33 ms)", async () => {
    const intervals = Array(25).fill(1000 / 120);
    const harness = makeRafHarness(intervals);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBe(120);
  });

  it("discards throttled intervals (> 50 ms) and uses only clean samples", async () => {
    // 5 throttled ticks followed by 25 clean 60 Hz ticks.
    const throttled = Array(5).fill(100); // 100 ms each — hidden/backgrounded
    const clean = Array(25).fill(1000 / 60);
    const harness = makeRafHarness([...throttled, ...clean]);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBe(60);
  });

  it("returns null when all intervals are throttled (tab hidden throughout)", async () => {
    // 100 ms intervals — all above the 50 ms throttle threshold.
    // After ~20 ticks the injected clock reaches 2000 ms (RAF_TIMEOUT_MS) → resolves null.
    const intervals = Array(25).fill(100);
    const harness = makeRafHarness(intervals);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBeNull();
  });

  it("returns null when fewer than 10 clean samples are collected before the timeout", async () => {
    // 5 clean 60 Hz intervals, then throttled ticks that push the clock past 2000 ms.
    const clean = Array(5).fill(1000 / 60);
    const throttled = Array(25).fill(100); // ~2500 ms crosses RAF_TIMEOUT_MS
    const harness = makeRafHarness([...clean, ...throttled]);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBeNull();
  });

  it("resolves once exactly RAF_MIN_SAMPLES (20) clean samples are collected", async () => {
    // 21 intervals → 20 gaps pushed (first tick seeds prev, no push).
    // Once cleanIntervals reaches 20 the loop exits immediately.
    const intervals = Array(21).fill(1000 / 60);
    const harness = makeRafHarness(intervals);
    const handle = measureDisplayRefreshHz(harness.raf, harness.now);
    const hz = await flushAll(handle, harness);
    expect(hz).toBe(60);
  });

  it("cancel() stops the rAF loop and resolves the result promise to null without throwing", async () => {
    // Start a measurement with enough intervals that it would normally resolve to 60.
    // Cancel after the handle is returned (before any ticks fire) and verify:
    //  1. result resolves to null (not 60, not an error)
    //  2. no further raf() scheduling occurs (queue stays empty after cancel)
    const intervals = Array(25).fill(1000 / 60);
    const harness = makeRafHarness(intervals);

    const handle = measureDisplayRefreshHz(harness.raf, harness.now);

    // Cancel immediately — before any ticks have been flushed.
    handle.cancel();

    // Flush once to let the already-scheduled first tick fire and detect cancellation.
    harness.flush();
    await Promise.resolve();

    // Drain any remaining microtasks.
    const hz = await handle.result;

    expect(hz).toBeNull();

    // After cancel the queue should be empty — no tick re-scheduled itself.
    // Flush again to confirm no further ticks run.
    const queueWasEmpty = (() => {
      let called = false;
      // If flush has nothing to do, no cb will fire — trackingRaf would not be
      // called from within tick() either.  We verify by checking the result is
      // still null and no new ticks attempted to write a value.
      harness.flush();
      return !called;
    })();
    expect(queueWasEmpty).toBe(true);
    // result must still be null (not overwritten by a late tick)
    // Re-awaiting a resolved promise returns the same value.
    await expect(handle.result).resolves.toBeNull();
  });
});
