/**
 * The four session charts (handoff §A.3): a 520x110 area chart per card.
 * Not `LineChart2` — one series, no axes, and the mock's geometry is the whole
 * visual. The scale is 0 → max*1.15, so a steady 60 fps draws flat near the top
 * rather than as noise filling the box.
 */

import { useId } from "react";

import type { ChartSeries, SessionChartSeries } from "./chartSeries";

const W = 520;
const H = 110;
const GRIDLINES = [0, 0.25, 0.5, 0.75, 1];

export interface ChartCardProps {
  title: string;
  unit: string;
  series: ChartSeries;
  color: string;
  /** Digits on the headline figure. */
  precision?: number;
}

export function ChartCard({ title, unit, series, color, precision = 0 }: ChartCardProps) {
  const gradientId = useId();
  const { values, current } = series;
  const max = Math.max(...values, 0) * 1.15 || 1;

  // One point cannot draw a line; the gridlines still hold the card's geometry.
  const points =
    values.length >= 2
      ? values
          .map((v, i) => `${((i / (values.length - 1)) * W).toFixed(1)},${(H - (v / max) * H).toFixed(1)}`)
          .join(" ")
      : "";

  return (
    <div className="card card-pad">
      <div style={{ display: "flex", alignItems: "baseline", gap: 10 }}>
        <span className="eyebrow">{title}</span>
        <span
          className="num"
          style={{ marginLeft: "auto", fontSize: "var(--t-lg)", color: "var(--text)" }}
        >
          {current === null ? "—" : current.toFixed(precision)}
          <span style={{ fontSize: "var(--t-xs)", color: "var(--text-3)", marginLeft: 3 }}>
            {unit}
          </span>
        </span>
      </div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        style={{ width: "100%", height: 88, marginTop: 10, overflow: "visible" }}
        aria-hidden="true"
      >
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0" stopColor={color} stopOpacity=".26" />
            <stop offset="1" stopColor={color} stopOpacity="0" />
          </linearGradient>
        </defs>
        {GRIDLINES.map((f) => (
          <line
            key={f}
            x1="0"
            y1={(H * f).toFixed(0)}
            x2={W}
            y2={(H * f).toFixed(0)}
            stroke="var(--line)"
            strokeWidth="1"
          />
        ))}
        {points && <polygon points={`0,${H} ${points} ${W},${H}`} fill={`url(#${gradientId})`} />}
        {points && (
          <polyline
            points={points}
            fill="none"
            stroke={color}
            strokeWidth="1.8"
            strokeLinejoin="round"
            strokeLinecap="round"
          />
        )}
      </svg>
    </div>
  );
}

/** Spec §9 fixes the two that could be read off either side: latency is the
 *  browser's RTT, and "Encode time" is the agent's encode p50. */
export function SessionCharts({ series }: { series: SessionChartSeries }) {
  return (
    <div className="grid g2" style={{ marginBottom: "var(--s4)" }}>
      <ChartCard title="Frame rate" unit="fps" series={series.fps} color="var(--success)" />
      <ChartCard title="Round-trip latency" unit="ms" series={series.latency} color="var(--info)" />
      <ChartCard title="Bitrate" unit="Mb/s" series={series.bitrate} color="var(--accent)" precision={1} />
      <ChartCard title="Encode time" unit="ms" series={series.encode} color="var(--lavender)" precision={1} />
    </div>
  );
}
