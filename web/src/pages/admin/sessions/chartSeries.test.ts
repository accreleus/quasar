import { describe, expect, it } from "vitest";

import type { MetricPoint } from "../../../api/types";
import { sessionChartSeries, splitBySource } from "./chartSeries";

function agent(ts: number, metrics: Record<string, number>): MetricPoint {
  return { source: "agent", ts_unix_ms: ts, metrics } as MetricPoint;
}

function browser(ts: number, metrics: Record<string, number>): MetricPoint {
  return { source: "browser", ts_unix_ms: ts, metrics } as MetricPoint;
}

describe("splitBySource", () => {
  it("separates the two sources and puts each in time order", () => {
    // The wire serves newest-first; a chart draws oldest-first.
    const { agent: a, browser: b } = splitBySource([
      browser(3000, { fps: 60 }),
      agent(3000, { fps: 59 }),
      browser(1000, { fps: 58 }),
      agent(1000, { fps: 57 }),
    ]);
    expect(a.map((r) => r.ts)).toEqual([1000, 3000]);
    expect(b.map((r) => r.ts)).toEqual([1000, 3000]);
  });

  it("returns two empty arrays for no points", () => {
    expect(splitBySource([])).toEqual({ agent: [], browser: [] });
  });
});

describe("sessionChartSeries — which source feeds which chart", () => {
  const points = [
    agent(1000, { fps: 12, bitrate_kbps: 24_000, encode_ms_p50: 3.1 }),
    browser(1000, { fps: 60, rtt_ms: 14 }),
    agent(2000, { fps: 13, bitrate_kbps: 18_500, encode_ms_p50: 4.2 }),
    browser(2000, { fps: 58, rtt_ms: 19 }),
  ];

  it("reads fps and latency from the browser, never from the agent", () => {
    const s = sessionChartSeries(points);
    // 12/13 is the agent's rate on the same samples — reading it here would
    // draw the encoder's fps under a chart about what the player saw.
    expect(s.fps.values).toEqual([60, 58]);
    expect(s.fps.current).toBe(58);
    expect(s.latency.values).toEqual([14, 19]);
    expect(s.latency.current).toBe(19);
  });

  it("reads bitrate from the agent and converts kb/s to Mb/s", () => {
    const s = sessionChartSeries(points);
    expect(s.bitrate.values).toEqual([24, 18.5]);
    expect(s.bitrate.current).toBe(18.5);
  });

  it("reads encode time from the agent's p50 (spec §9)", () => {
    const s = sessionChartSeries(points);
    expect(s.encode.values).toEqual([3.1, 4.2]);
    expect(s.encode.current).toBe(4.2);
  });

  it("falls back to encode_ms when an older agent sends no percentile", () => {
    const s = sessionChartSeries([agent(1000, { encode_ms: 7.5 })]);
    expect(s.encode.values).toEqual([7.5]);
  });

  it("prefers the p50 when both are present", () => {
    const s = sessionChartSeries([agent(1000, { encode_ms: 7.5, encode_ms_p50: 3.1 })]);
    expect(s.encode.values).toEqual([3.1]);
  });
});

describe("sessionChartSeries — gaps and emptiness", () => {
  it("skips a sample that carries no reading instead of plotting a zero", () => {
    const s = sessionChartSeries([
      browser(1000, { fps: 60, rtt_ms: 14 }),
      browser(2000, { fps: 59 }), // no rtt this window
      browser(3000, { fps: 58, rtt_ms: 16 }),
    ]);
    expect(s.latency.values).toEqual([14, 16]);
    expect(s.fps.values).toEqual([60, 59, 58]);
  });

  it("reports a current of null, not zero, for a series with no samples", () => {
    const s = sessionChartSeries([browser(1000, { fps: 60 })]);
    expect(s.bitrate).toEqual({ values: [], current: null });
    expect(s.encode.current).toBeNull();
  });

  it("returns four empty series for no points at all", () => {
    const s = sessionChartSeries([]);
    for (const key of ["fps", "latency", "bitrate", "encode"] as const) {
      expect(s[key]).toEqual({ values: [], current: null });
    }
  });

  it("drops a non-finite reading rather than breaking the path", () => {
    const s = sessionChartSeries([browser(1000, { fps: Number.NaN }), browser(2000, { fps: 60 })]);
    expect(s.fps.values).toEqual([60]);
  });
});
