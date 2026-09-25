// Hand-rolled SVG chart components for the P4-06 telemetry drill-down.
// Uses dark-theme CSS variables from styles.css; no external charting dependency.

import { useMemo } from "react";
import type { CSSProperties, ReactElement } from "react";

// Reduce-based min/max helpers — avoid Math.min/max spread which stack-overflows
// on large arrays (up to 1000 samples).
const arrMin = (a: number[]) => a.reduce((m, v) => (v < m ? v : m), Infinity);
const arrMax = (a: number[]) => a.reduce((m, v) => (v > m ? v : m), -Infinity);

// ── Line Chart ────────────────────────────────────────────────────────────────

export interface LineSeriesPoint {
  /** x position in data-space (unix ms, or sequential index). */
  x: number;
  y: number;
}

export interface LineSeries {
  label: string;
  color: string;
  points: LineSeriesPoint[];
}

interface LineChartProps {
  series: LineSeries[];
  /** Y-axis label / units (e.g. "fps", "ms", "kbps"). */
  unit?: string;
  height?: number;
}

const PAD = { top: 8, right: 8, bottom: 28, left: 42 };

function scalePoints(
  series: LineSeries[],
  width: number,
  height: number,
): Array<{ color: string; label: string; path: string; lastY: number }> {
  const allPoints = series.flatMap((s) => s.points);
  if (allPoints.length === 0) return [];

  const xValues = allPoints.map((p) => p.x);
  const yValues = allPoints.map((p) => p.y);
  const xMin = arrMin(xValues);
  const xMax = arrMax(xValues);
  const yMin = 0;
  const yMax = arrMax(yValues) * 1.1 || 1;

  const innerW = width - PAD.left - PAD.right;
  const innerH = height - PAD.top - PAD.bottom;

  const toSvgX = (x: number) =>
    PAD.left + ((xMax === xMin ? 0.5 : (x - xMin) / (xMax - xMin)) * innerW);
  const toSvgY = (y: number) =>
    PAD.top + innerH - ((y - yMin) / (yMax - yMin)) * innerH;

  return series
    .filter((s) => s.points.length > 0)
    .map((s) => {
      const sorted = [...s.points].sort((a, b) => a.x - b.x);
      const d = sorted
        .map((p, i) => `${i === 0 ? "M" : "L"}${toSvgX(p.x).toFixed(1)},${toSvgY(p.y).toFixed(1)}`)
        .join(" ");
      const lastY = toSvgY(sorted[sorted.length - 1].y);
      return { color: s.color, label: s.label, path: d, lastY };
    });
}

function yTicks(series: LineSeries[], count = 4): number[] {
  const allY = series.flatMap((s) => s.points.map((p) => p.y));
  if (allY.length === 0) return [];
  const max = arrMax(allY) * 1.1 || 1;
  return Array.from({ length: count + 1 }, (_, i) =>
    Math.round((max / count) * i),
  );
}

export function LineChart({ series, unit = "", height = 120 }: LineChartProps): ReactElement {
  const width = 340; // viewBox width; SVG scales with CSS width:100%

  const paths = useMemo(() => scalePoints(series, width, height), [series, width, height]);
  const ticks = useMemo(() => yTicks(series), [series]);
  const innerH = height - PAD.top - PAD.bottom;

  const yMin = 0;
  const yMax = ticks[ticks.length - 1] || 1;
  const toSvgY = (y: number) =>
    PAD.top + innerH - ((y - yMin) / (yMax - yMin)) * innerH;

  return (
    <div style={{ width: "100%" }}>
      {/* legend */}
      {series.length > 1 && (
        <div className="row mb1">
          {series.map((s) => (
            <span
              key={s.label}
              className="chart-key row gap1 t-xs"
              style={{ "--series-color": s.color } as CSSProperties}
            >
              <svg width={16} height={2}><line x1={0} y1={1} x2={16} y2={1} stroke={s.color} strokeWidth={2} /></svg>
              {s.label}
            </span>
          ))}
        </div>
      )}
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="chart-svg"
        style={{ width: "100%", height }}
      >
        {/* grid lines + y-axis ticks */}
        {ticks.map((t) => {
          const y = toSvgY(t);
          return (
            <g key={t}>
              <line
                x1={PAD.left}
                y1={y}
                x2={width - PAD.right}
                y2={y}
                stroke="var(--border)"
                strokeWidth={0.5}
              />
              <text
                x={PAD.left - 4}
                y={y + 4}
                textAnchor="end"
                fill="var(--muted)"
                fontSize={9}
              >
                {t}
              </text>
            </g>
          );
        })}

        {/* unit label */}
        {unit && (
          <text
            x={PAD.left - 4}
            y={PAD.top - 2}
            textAnchor="end"
            fill="var(--muted)"
            fontSize={9}
          >
            {unit}
          </text>
        )}

        {/* series lines */}
        {paths.map((p) => (
          <path
            key={p.label}
            d={p.path}
            fill="none"
            stroke={p.color}
            strokeWidth={1.5}
            strokeLinejoin="round"
            strokeLinecap="round"
          />
        ))}

        {/* x-axis baseline */}
        <line
          x1={PAD.left}
          y1={height - PAD.bottom}
          x2={width - PAD.right}
          y2={height - PAD.bottom}
          stroke="var(--border)"
          strokeWidth={0.5}
        />
      </svg>
      {series.length === 1 && (
        <div className="chart-caption muted">
          {series[0].label}
        </div>
      )}
    </div>
  );
}

// ── Stacked Bar (qualified RVFC capture-to-display breakdown) ─────────────────

export interface StackedBarSegment {
  label: string;
  value: number;
  color: string;
}

interface StackedBarProps {
  segments: StackedBarSegment[];
  totalLabel?: string;
}

export function StackedBar({ segments, totalLabel }: StackedBarProps): ReactElement {
  const total = segments.reduce((s, seg) => s + seg.value, 0);
  if (total === 0) return <span className="muted">—</span>;

  return (
    <div style={{ width: "100%" }}>
      {/* bar */}
      <div className="stacked-bar">
        {segments.map((seg) => (
          <div
            key={seg.label}
            title={`${seg.label}: ${seg.value.toFixed(1)} ms`}
            className="stacked-seg"
            style={{ width: `${(seg.value / total) * 100}%`, "--seg-color": seg.color } as CSSProperties}
          />
        ))}
      </div>
      {/* legend */}
      <div className="stacked-legend mt2 t-xs">
        {segments.map((seg) => (
          <span key={seg.label} className="row gap1">
            <span className="stacked-swatch" style={{ "--seg-color": seg.color } as CSSProperties} />
            <span className="muted">{seg.label}</span>
            <span className="text-1">{seg.value.toFixed(1)} ms</span>
          </span>
        ))}
        {totalLabel && (
          <span className="muted">
            total <span className="text-1">{total.toFixed(1)} ms</span>
          </span>
        )}
      </div>
    </div>
  );
}
