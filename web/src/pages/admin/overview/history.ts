/**
 * Sparkline memory: the last few polls of a keyed number, held in the page.
 *
 * The wire serves levels, not series, so a trend is only what this page has
 * watched since it was opened. Two rules: a key the new sample does not
 * mention is dropped (a session that ended is not a series, and the map would
 * otherwise grow all session long), and a key present with no usable value keeps
 * its history — a missing sample is a gap, not a stall at zero.
 */

import { useRef } from "react";

/** How many polls a series remembers (mock §A.1 sparklines are ~10 points; 20
 *  is one minute of the 5 s fleet poll, which is what a trend should show). */
export const HISTORY_LIMIT = 20;

/** Oldest first — the order a sparkline draws left to right. */
export type Series = Readonly<Record<string, readonly number[]>>;

/** One poll's readings. `undefined` means "no reading this time", which is a
 *  different fact from a reading of zero. */
export type Samples = Readonly<Record<string, number | undefined>>;

export function pushSamples(previous: Series, samples: Samples): Series {
  const next: Record<string, readonly number[]> = {};
  for (const [key, value] of Object.entries(samples)) {
    const held = previous[key] ?? [];
    if (value === undefined || !Number.isFinite(value)) {
      next[key] = held;
      continue;
    }
    const grown = [...held, value];
    next[key] = grown.length > HISTORY_LIMIT ? grown.slice(grown.length - HISTORY_LIMIT) : grown;
  }
  return next;
}

/** The series for a key, or an empty one — never undefined, so a caller can
 *  ask `length >= 2` without a guard. */
export function seriesFor(series: Series, key: string): readonly number[] {
  return series[key] ?? [];
}

/**
 * One point per applied load, not per render.
 *
 * Pages that show ages re-render on a 1 s clock between 5 s polls; sampling in
 * the render body would draw five copies of every reading. `stamp` is the
 * resource's `updatedAt`, so a sample is taken exactly when new data landed.
 */
export function useSeries(samples: Samples, stamp: number | null): Series {
  const held = useRef<{ stamp: number | null; series: Series }>({ stamp: null, series: {} });
  if (stamp !== held.current.stamp) {
    held.current = { stamp, series: pushSamples(held.current.series, samples) };
  }
  return held.current.series;
}
