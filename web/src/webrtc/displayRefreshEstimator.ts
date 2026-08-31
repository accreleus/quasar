/** Pure display-refresh-rate estimator extracted from SessionTelemetry.drainPresentWindow(). */

import { median } from "../lib/stats";

export { median };

/** Null when fewer than 10 samples, or median interval is zero/non-positive. */
export function estimateDisplayRefreshHz(intervals: number[]): number | null {
  if (intervals.length < 10) return null;
  const med = median(intervals);
  return med != null && med > 0 ? Math.round(1000 / med) : null;
}

/** Intervals longer than this are throttled ticks (hidden tab, compositor pause), discarded. */
const THROTTLE_THRESHOLD_MS = 50;

/** Floor for stability: ~20-30 frames at 60Hz is ~333-500ms. */
const RAF_MIN_SAMPLES = 20;

/** Give up after this many rAF ticks without enough clean samples (persistently hidden tab). */
const RAF_MAX_TICKS = 200;

/** Wall-clock cap (ms); resolves null if hit before RAF_MIN_SAMPLES clean samples. */
const RAF_TIMEOUT_MS = 2000;

type RafFn = (cb: FrameRequestCallback) => number;
type NowFn = () => number;

export interface MeasurementHandle {
  /** Stops the rAF loop; {@link result} then resolves null. */
  cancel(): void;
  result: Promise<number | null>;
}

/**
 * Measures vsync refresh rate via requestAnimationFrame — independent of the
 * video stream, so it reflects the true monitor Hz (60/120/144...). `cancel()`
 * stops the loop early (e.g. unmount); resolves null without throwing.
 *
 * @param raf,now injectable for tests; `tick()` reads now() directly rather than
 *                the rAF timestamp arg so the clock stays injectable.
 */
export function measureDisplayRefreshHz(
  raf: RafFn = (cb) => requestAnimationFrame(cb),
  now: NowFn = () => performance.now(),
): MeasurementHandle {
  let cancelled = false;
  let resolved = false;

  const result = new Promise<number | null>((resolve) => {
    const cleanIntervals: number[] = [];
    let prev: number | null = null;
    let ticks = 0;
    const deadline = now() + RAF_TIMEOUT_MS;

    function tick() {
      if (cancelled) {
        if (!resolved) {
          resolved = true;
          resolve(null);
        }
        return;
      }

      const t = now();

      if (prev !== null) {
        const iv = t - prev;
        if (iv <= THROTTLE_THRESHOLD_MS) {
          cleanIntervals.push(iv);
        }
      }
      prev = t;
      ticks++;

      if (
        cleanIntervals.length >= RAF_MIN_SAMPLES ||
        ticks >= RAF_MAX_TICKS ||
        t >= deadline
      ) {
        // Safe: estimateDisplayRefreshHz returns null below its own 10-sample floor.
        if (!resolved) {
          resolved = true;
          resolve(estimateDisplayRefreshHz(cleanIntervals));
        }
        return;
      }

      raf(tick);
    }

    raf(tick);
  });

  return {
    cancel() {
      cancelled = true;
    },
    result,
  };
}
