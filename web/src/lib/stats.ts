/**
 * stats — the one set of summary estimators the web client uses (the estimator
 * is part of the number; divergent rank formulas once made two "p50" series
 * different statistics). Fixed decisions:
 *   - percentile: p as a percentage, nearest-rank with ceiling — the formula
 *     the long-lived bench reports were computed with.
 *   - median is the TRUE median (even count averages the middle pair), never
 *     percentile(v, 50).
 *   - Empty input returns null — a missing measurement is never a zero.
 * Inputs are never mutated.
 */

/**
 * Nearest-rank percentile (ceiling) over an unsorted or sorted array.
 * `p` is a PERCENTAGE in [0, 100]. `pages/app/sessionSummary`'s
 * `percentileFraction` is the same formula over a fraction — not the same
 * function, and folding the two would silently rescale every bench series.
 */
export function percentile(values: readonly number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.ceil((p / 100) * sorted.length) - 1;
  return sorted[Math.min(sorted.length - 1, Math.max(0, rank))] ?? null;
}

/**
 * True median: the middle value for an odd count, the average of the middle
 * pair for an even one. Returns null when there are no values.
 */
export function median(values: readonly number[]): number | null {
  const n = values.length;
  if (n === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = n >> 1;
  if (n % 2 === 1) return sorted[mid]!;
  return (sorted[mid - 1]! + sorted[mid]!) / 2;
}

/** Arithmetic mean. Returns null when there are no values. */
export function mean(values: readonly number[]): number | null {
  if (values.length === 0) return null;
  let sum = 0;
  for (const v of values) sum += v;
  return sum / values.length;
}

/**
 * Population standard deviation (divides by n, not n−1) — these are complete
 * windows of observations, not samples drawn from a larger population, and the
 * pre-existing present-σ series was computed this way. Returns null when empty.
 */
export function stddev(values: readonly number[]): number | null {
  const m = mean(values);
  if (m === null) return null;
  let acc = 0;
  for (const v of values) acc += (v - m) * (v - m);
  return Math.sqrt(acc / values.length);
}
