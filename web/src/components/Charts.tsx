/**
 * Charts — dependency-free SVG chart primitives (UI-04).
 *
 * Exports:
 *   Sparkline   — thin inline area/line over a value array
 *   LineChart2  — full-featured multi-series line chart with axes
 *
 * TelemetryChart.tsx (LineChart / StackedBar) is a separate module, still the
 * one sessions/DiagnosticsDisclosure.tsx imports.
 *
 * Both Sparkline and LineChart2 are resize-debounced via ResizeObserver.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement } from "react";

// ── helpers ────────────────────────────────────────────────

const arrMin = (a: number[]) => a.reduce((m, v) => (v < m ? v : m), Infinity);
const arrMax = (a: number[]) => a.reduce((m, v) => (v > m ? v : m), -Infinity);

function useContainerWidth(fallback = 300): [React.RefObject<HTMLDivElement>, number] {
  const ref = useRef<HTMLDivElement>(null!) as React.RefObject<HTMLDivElement>;
  const [width, setWidth] = useState(fallback);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width;
      if (w && w > 0) setWidth(w);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  return [ref, width];
}

// ── Sparkline ──────────────────────────────────────────────

interface SparklineProps {
  /** Raw data values (no x axis needed) */
  points: number[];
  color?: string;
  height?: number;
  /** Fill area under the line */
  fill?: boolean;
  /**
   * Low end of the vertical scale. Default is the series' own minimum, which
   * shows the shape of a wobble; `baseline={0}` (the v3 mock's spark) shows
   * its size instead, so 60 fps holding steady draws as a flat line near the
   * top rather than as noise filling the box.
   */
  baseline?: number;
}

export function Sparkline({
  points,
  color = "var(--info)",
  height = 34,
  fill = true,
  baseline,
}: SparklineProps): ReactElement {
  const [containerRef, width] = useContainerWidth(120);

  const { linePath, areaPath } = useMemo(() => {
    if (points.length < 2) return { linePath: "", areaPath: "" };
    const min = baseline ?? arrMin(points);
    const max = Math.max(arrMax(points), min);
    const range = max - min || 1;
    const pad = 1;
    const innerW = Math.max(width - 2, 1);
    const innerH = height - 2 * pad;

    const xs = points.map((_, i) => (i / (points.length - 1)) * innerW);
    const ys = points.map((v) => pad + innerH - ((v - min) / range) * innerH);

    const d = xs
      .map((x, i) => `${i === 0 ? "M" : "L"}${x.toFixed(1)},${ys[i].toFixed(1)}`)
      .join(" ");

    const area =
      `${d} L${xs[xs.length - 1].toFixed(1)},${(height - pad).toFixed(1)}` +
      ` L${xs[0].toFixed(1)},${(height - pad).toFixed(1)} Z`;

    return { linePath: d, areaPath: area };
  }, [points, width, height, baseline]);

  return (
    <div ref={containerRef} style={{ width: "100%", height }}>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width={width}
        height={height}
        style={{ display: "block", overflow: "visible" }}
        aria-hidden="true"
      >
        {fill && areaPath && (
          <path
            d={areaPath}
            fill={color}
            fillOpacity={0.22}
          />
        )}
        {linePath && (
          <path
            d={linePath}
            fill="none"
            stroke={color}
            strokeWidth={1.5}
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        )}
      </svg>
    </div>
  );
}

// ── LineChart2 ─────────────────────────────────────────────

export interface LineSeriesPoint2 {
  x: number;
  y: number;
}

export interface LineSeries2 {
  label: string;
  color: string;
  points: LineSeriesPoint2[];
}

interface LineChart2Props {
  series: LineSeries2[];
  unit?: string;
  height?: number;
}

const PAD2 = { top: 8, right: 12, bottom: 28, left: 42 };

export function LineChart2({ series, unit = "", height = 120 }: LineChart2Props): ReactElement {
  const [containerRef, width] = useContainerWidth(340);

  const { paths, ticks, toSvgY } = useMemo(() => {
    const allPoints = series.flatMap((s) => s.points);
    const innerH = height - PAD2.top - PAD2.bottom;
    const innerW = Math.max(width - PAD2.left - PAD2.right, 1);

    if (allPoints.length === 0) {
      return { paths: [], ticks: [], toSvgY: () => 0 };
    }

    const xValues = allPoints.map((p) => p.x);
    const yValues = allPoints.map((p) => p.y);
    const xMin = arrMin(xValues);
    const xMax = arrMax(xValues);
    const yMax = arrMax(yValues) * 1.1 || 1;

    const toSvgX = (x: number) =>
      PAD2.left + (xMax === xMin ? 0.5 : (x - xMin) / (xMax - xMin)) * innerW;
    const toSvgY = (y: number) =>
      PAD2.top + innerH - (y / yMax) * innerH;

    const tickCount = 4;
    const ticks = Array.from({ length: tickCount + 1 }, (_, i) =>
      Math.round((yMax / tickCount) * i),
    );

    const paths = series
      .filter((s) => s.points.length > 0)
      .map((s) => {
        const sorted = [...s.points].sort((a, b) => a.x - b.x);
        const d = sorted
          .map((p, i) => `${i === 0 ? "M" : "L"}${toSvgX(p.x).toFixed(1)},${toSvgY(p.y).toFixed(1)}`)
          .join(" ");
        return { label: s.label, color: s.color, path: d };
      });

    return { paths, ticks, toSvgY };
  }, [series, width, height]);

  return (
    <div ref={containerRef} style={{ width: "100%" }}>
      {/* legend */}
      {series.length > 1 && (
        <div style={{ display: "flex", gap: 12, marginBottom: 4 }}>
          {series.map((s) => (
            <span
              key={s.label}
              style={{
                fontSize: 11,
                color: s.color,
                display: "flex",
                alignItems: "center",
                gap: 4,
              }}
            >
              <svg width={16} height={2}>
                <line x1={0} y1={1} x2={16} y2={1} stroke={s.color} strokeWidth={2} />
              </svg>
              {s.label}
            </span>
          ))}
        </div>
      )}
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width={width}
        height={height}
        style={{ display: "block", overflow: "visible" }}
      >
        {/* grid + y ticks */}
        {ticks.map((t) => {
          const y = toSvgY(t);
          return (
            <g key={t}>
              <line
                x1={PAD2.left}
                y1={y}
                x2={width - PAD2.right}
                y2={y}
                stroke="var(--line)"
                strokeWidth={0.5}
              />
              <text
                x={PAD2.left - 4}
                y={y + 4}
                textAnchor="end"
                fill="var(--text-4)"
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
            x={PAD2.left - 4}
            y={PAD2.top - 2}
            textAnchor="end"
            fill="var(--text-4)"
            fontSize={9}
          >
            {unit}
          </text>
        )}
        {/* series */}
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
        {/* baseline */}
        <line
          x1={PAD2.left}
          y1={height - PAD2.bottom}
          x2={width - PAD2.right}
          y2={height - PAD2.bottom}
          stroke="var(--line)"
          strokeWidth={0.5}
        />
      </svg>
      {series.length === 1 && (
        <div style={{ textAlign: "center", fontSize: 10, color: "var(--text-3)" }}>
          {series[0].label}
        </div>
      )}
    </div>
  );
}
