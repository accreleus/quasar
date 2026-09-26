const MAX_DECIMALS = 6;

/**
 * Y-axis tick values from 0 to `max` in `count` equal steps, used as React keys
 * and gridline positions by Charts.tsx and TelemetryChart.tsx.
 *
 * Integers whenever those are distinct, so a normal range keeps its labels
 * (max 66 → 0, 17, 33, 50, 66). A small range would repeat (max 1.1 → 0, 0, 1,
 * 1, 1), so it takes the fewest decimals that keep every tick distinct.
 */
export function yTicks(max: number, count = 4): number[] {
  const raw = Array.from({ length: count + 1 }, (_, i) => (max / count) * i);
  let ticks: number[] = [];
  for (let decimals = 0; decimals <= MAX_DECIMALS; decimals++) {
    const f = 10 ** decimals;
    ticks = raw.map((v) => Math.round(v * f) / f);
    if (new Set(ticks).size === ticks.length) return ticks;
  }
  return [...new Set(ticks)];
}
