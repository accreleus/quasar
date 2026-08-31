/**
 * The session detail's four charts from one `MetricPoint[]`.
 *
 * Which source feeds which chart is a claim about who can see what: the agent
 * knows what the host encoded, the browser what the player saw, and they
 * diverge exactly when a session is going wrong. Spec §9 fixes two of the four:
 * latency is browser `rtt_ms`, "Encode time" is agent `encode_ms_p50`.
 */

import type { AgentMetrics, BrowserMetrics, MetricPoint } from "../../../api/types";

export interface ChartSeries {
  /** Oldest first — the order the chart draws left to right. */
  values: number[];
  /** The newest value, or null when the series is empty. Shown as the card's
   *  headline figure, so it is the last sample, never an average. */
  current: number | null;
}

export interface SessionChartSeries {
  /** Browser `fps` — the rate the player saw. */
  fps: ChartSeries;
  /** Browser `rtt_ms` (spec §9). */
  latency: ChartSeries;
  /** Agent `bitrate_kbps`, in Mb/s. */
  bitrate: ChartSeries;
  /** Agent `encode_ms_p50` (spec §9), falling back to the plain `encode_ms` an
   *  older agent sends — an absent percentile is a build's age, not a session
   *  with no encode time. */
  encode: ChartSeries;
}

export interface SplitPoints {
  agent: Array<{ ts: number; m: AgentMetrics }>;
  browser: Array<{ ts: number; m: BrowserMetrics }>;
}

/** Split a mixed, newest-first metrics page into two time-ordered arrays. */
export function splitBySource(points: readonly MetricPoint[]): SplitPoints {
  const agent: Array<{ ts: number; m: AgentMetrics }> = [];
  const browser: Array<{ ts: number; m: BrowserMetrics }> = [];
  for (const p of points) {
    if (p.source === "agent") agent.push({ ts: p.ts_unix_ms, m: p.metrics as AgentMetrics });
    else browser.push({ ts: p.ts_unix_ms, m: p.metrics as BrowserMetrics });
  }
  agent.sort((a, b) => a.ts - b.ts);
  browser.sort((a, b) => a.ts - b.ts);
  return { agent, browser };
}

const EMPTY: ChartSeries = { values: [], current: null };

/**
 * One series from one reading per sample. A sample that carries no reading is
 * Skipped rather than zeroed: the metric dicts are omit-when-absent, and a gap
 * plotted at zero reads as a stall that never happened.
 */
function series<T>(
  rows: Array<{ ts: number; m: T }>,
  read: (m: T) => number | undefined,
): ChartSeries {
  const values: number[] = [];
  for (const row of rows) {
    const value = read(row.m);
    if (value === undefined || !Number.isFinite(value)) continue;
    values.push(value);
  }
  if (values.length === 0) return EMPTY;
  return { values, current: values[values.length - 1] };
}

export function sessionChartSeries(points: readonly MetricPoint[]): SessionChartSeries {
  const { agent, browser } = splitBySource(points);
  return {
    fps: series(browser, (m) => m.fps),
    latency: series(browser, (m) => m.rtt_ms),
    bitrate: series(agent, (m) =>
      m.bitrate_kbps === undefined ? undefined : m.bitrate_kbps / 1000,
    ),
    encode: series(agent, (m) => m.encode_ms_p50 ?? m.encode_ms),
  };
}
