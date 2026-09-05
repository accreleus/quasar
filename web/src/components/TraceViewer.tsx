/**
 * TraceViewer — the session-detail Trace card.
 *
 * Structure and styling follow the operator's standalone reference mockup
 * ("Quasar Session Trace", v3 tokens only): a `.card` with a `panel-head`, a
 * one-line verdict strip over an expandable evidence panel, then a body of
 * stacked lanes — a 96px eyebrow label beside a caption row and a 54px plot —
 * with the event markers on their OWN track under the lanes, a relative time
 * axis ending at "now", and an event legend.
 *
 * Reads GET /v1/admin/sessions/{id}/diagnostic-bundle. No external chart deps —
 * hand-rolled SVG, same as Charts.tsx / TelemetryChart.tsx.
 *
 * ST-07 — lazy-loaded into the session detail's Diagnostics disclosure.
 */

import { memo, useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { getDiagnosticBundle } from "../api/admin";
import { ApiError } from "../api/client";
import type { DiagnosticBundle, Falsifier, TraceSeriesPoint, TraceEvent } from "../api/types";
import { estimatorLabel, metricTooltip, seriesInfo } from "../lib/metricsManifest";
import { useContainerWidth } from "../lib/useContainerWidth";
import { Chip, type ChipVariant } from "./Chip";
import { SegmentedControl } from "./SegmentedControl";
import { IconRefresh } from "./icons";

// ── helpers ────────────────────────────────────────────────────────────────────

function arrMin(a: number[]): number {
  return a.reduce((m, v) => (v < m ? v : m), Infinity);
}
function arrMax(a: number[]): number {
  return a.reduce((m, v) => (v > m ? v : m), -Infinity);
}

/** A lane caption reads "fps · max 60": enough precision to be useful, never
 *  enough digits to wrap the caption row. */
function fmtScale(v: number): string {
  return String(Math.abs(v) >= 100 ? Math.round(v) : Math.round(v * 10) / 10);
}

function fmtSeconds(ms: number): string {
  return `${Math.round(ms / 1000)} s`;
}

/** Axis labels: seconds under a minute, minutes above it. Exact, not rounded —
 *  rounding to the nearest minute (the mockup's trFmt, which only ever sees a
 *  step that divides the window evenly) labels a 30 s grid "-4m -4m -3m -3m". */
function fmtAgo(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  return s === 0 ? `${m}m` : `${m}m${s}s`;
}

// ── Event marker color by type ─────────────────────────────────────────────────

// The loud events — the ones an operator is looking for when a session misbehaves.
// Any other type still renders as a marker in OTHER_EVENT_COLOR and is accounted
// for by a single "other event" legend entry, because the type vocabulary is
// open-ended and a legend that silently omits half the markers on the chart is
// worse than one that buckets them.
const EVENT_COLORS: Record<string, string> = {
  "abr.retarget": "var(--warning)",
  "encoder.stall": "var(--danger-text)",
  "client.freeze_detected": "var(--danger)",
  // ABR resolution ladder (T6): distinct color so a rung step reads apart from
  // a plain bitrate retarget.
  "abr.ladder.step": "var(--warning-text)",
  "playout.changed": "var(--lavender)",
  "pipeline.source_swapped": "var(--info)",
  "webrtc.state_changed": "var(--text-3)",
};

const OTHER_EVENT_COLOR = "var(--text-4)";

function eventColor(type: string): string {
  return EVENT_COLORS[type] ?? OTHER_EVENT_COLOR;
}

// ── Lane definitions ───────────────────────────────────────────────────────────

interface LaneSeries {
  key: string;
  color: string;
  label: string;
}

interface LaneDef {
  id: string;
  label: string;
  /** Unit of the lane's PRIMARY series — the big current-value readout. */
  unit: string;
  /** [0] is the primary series: it carries the gradient area and the readout. */
  series: [LaneSeries, ...LaneSeries[]];
  /** Keys for derived_windows highlight bands. */
  highlightKey?: "hitches" | "encoder_saturation" | "likely_network_congestion";
  highlightColor?: string;
}

const LANES: LaneDef[] = [
  {
    id: "enc",
    label: "Encoder",
    unit: "fps",
    series: [
      { key: "encoder.fps", color: "var(--accent)", label: "fps" },
      // Amber, not the console's usual lavender for encode time: inside ONE lane
      // the two series have to be told apart at a glance, and amber is also the
      // hue of this lane's encoder-saturation band — which encode_ms is what
      // detects.
      { key: "encoder.encode_ms", color: "var(--warning-text)", label: "encode ms" },
    ],
    highlightKey: "encoder_saturation",
    highlightColor: "var(--warning-bg)",
  },
  {
    id: "net",
    label: "Transport",
    unit: "ms",
    series: [
      { key: "transport.rtt_ms", color: "var(--info)", label: "rtt ms" },
      { key: "transport.packets_lost", color: "var(--danger)", label: "pkt lost" },
    ],
    highlightKey: "likely_network_congestion",
    highlightColor: "var(--danger-bg)",
  },
  {
    id: "cli",
    label: "Client",
    unit: "ms",
    series: [
      { key: "client.present_interval_sd_ms", color: "var(--lavender)", label: "present σ ms" },
      {
        key: "client.glass_to_glass_ms",
        color: "var(--accent)",
        label: "qualified RVFC capture-to-display ms",
      },
    ],
    highlightKey: "hitches",
    highlightColor: "var(--danger-bg)",
  },
  {
    id: "abr",
    label: "ABR",
    unit: "kbps",
    series: [{ key: "abr.setpoint_kbps", color: "var(--success)", label: "setpoint kbps" }],
  },
];

// ── Lane geometry ──────────────────────────────────────────────────────────────
//
// One lane = a caption row above a 54px plot, per the reference mockup. The
// caption is what makes the plot readable at all: series inside one lane are
// scaled INDEPENDENTLY (#124 — fps ~60 and encode_ms ~20 on one shared scale
// flattened encode_ms onto the baseline), so the only way to know what the top
// of a curve is worth is to say so.

const LANE_H = 54;
/** Keeps a curve at its maximum off the lane's top rule. */
const LANE_INSET = 3;
/** Headroom over a series' own maximum, matching the mockup and the v3 mock's
 *  own lineChart renderer. */
const SCALE_HEADROOM = 1.15;

// ── Tooltip state ──────────────────────────────────────────────────────────────

interface TooltipState {
  x: number;
  y: number;
  laneLabel: string;
  values: Array<{ label: string; v: number; color: string; qual?: string; title?: string }>;
  nearEvents: TraceEvent[];
}

interface SeriesRender {
  key: string;
  color: string;
  label: string;
  /** Highest sample in the visible window; the caption's "max". */
  max: number;
  /** Latest sample in the visible window; the lane's readout for series[0]. */
  last: number | null;
  /** The stroked outline. Empty when the series has no samples in the window. */
  d: string;
  /** Gradient area under the primary series. Never stroked: stroking a closed
   *  area paints its baseline edge too, drawing a hard rule across the lane. */
  fillD: string;
  /** Set when the series' last sample falls short of the shared x domain. */
  end: { x: number; y: number } | null;
}

interface LaneChartProps {
  laneDef: LaneDef;
  series: Record<string, TraceSeriesPoint[] | undefined>;
  /** Hover only — the markers themselves live on their own track. */
  events: TraceEvent[];
  xMin: number;
  xMax: number;
  width: number;
  gradientId: string;
  derivedWindows: DiagnosticBundle["derived_windows"];
  windowFromMs: number;
  onHover: (tooltip: TooltipState | null) => void;
}

const LaneChart = memo(function LaneChart({
  laneDef,
  series,
  events,
  xMin,
  xMax,
  width,
  gradientId,
  derivedWindows,
  windowFromMs,
  onHover,
}: LaneChartProps) {
  const svgRef = useRef<SVGSVGElement>(null);

  const xRange = xMax - xMin || 1;
  const plotH = LANE_H - LANE_INSET * 2;

  const toSvgX = useCallback((ts: number) => ((ts - xMin) / xRange) * width, [xMin, xRange, width]);

  const rendered = useMemo<SeriesRender[]>(() => {
    return laneDef.series.map((s, idx) => {
      const pts = [...(series[s.key] ?? [])]
        .filter((p) => p.ts_unix_ms >= xMin && p.ts_unix_ms <= xMax)
        .sort((a, b) => a.ts_unix_ms - b.ts_unix_ms);

      const empty: SeriesRender = {
        key: s.key,
        color: s.color,
        label: s.label,
        max: 0,
        last: null,
        d: "",
        fillD: "",
        end: null,
      };
      if (pts.length === 0) return empty;

      const max = arrMax(pts.map((p) => p.v));
      // Own scale, always anchored at zero, so a curve's height is proportional
      // to its value.
      const scaleMax = max > 0 ? max * SCALE_HEADROOM : 1;
      const toY = (v: number) => LANE_H - (v / scaleMax) * plotH - LANE_INSET;

      const d = pts
        .map(
          (p, i) =>
            `${i === 0 ? "M" : "L"}${toSvgX(p.ts_unix_ms).toFixed(1)},${toY(p.v).toFixed(1)}`,
        )
        .join(" ");

      // Primary series only: two stacked gradients in one 54px lane is mud.
      const fillD =
        idx === 0
          ? `M${toSvgX(pts[0].ts_unix_ms).toFixed(1)},${LANE_H} ${d.slice(1)} L${toSvgX(
              pts[pts.length - 1].ts_unix_ms,
            ).toFixed(1)},${LANE_H} Z`
          : "";

      // "This series stopped" has to be judged against the series' OWN cadence:
      // a client sampling once a second is always ~1 s behind the domain end,
      // and a flat 2%-of-range threshold would cap every live series on the
      // chart. Three missed samples is a stop; one is the cadence.
      const gaps = pts
        .slice(1)
        .map((p, i) => p.ts_unix_ms - pts[i].ts_unix_ms)
        .sort((a, b) => a - b);
      const medianGap = gaps.length > 0 ? gaps[Math.floor(gaps.length / 2)] : 0;
      const endThreshold = xMax - Math.max(xRange * 0.02, medianGap * 3);

      const last = pts[pts.length - 1];
      const end =
        last.ts_unix_ms < endThreshold ? { x: toSvgX(last.ts_unix_ms), y: toY(last.v) } : null;

      return { key: s.key, color: s.color, label: s.label, max, last: last.v, d, fillD, end };
    });
  }, [laneDef.series, series, toSvgX, plotH, xMin, xMax, xRange]);

  // Highlight bands from derived_windows
  const bands = useMemo(() => {
    const key = laneDef.highlightKey;
    if (!key) return [];
    // Spelled out per key rather than indexed: the three window arrays have
    // different element types, and TS will not call .map on that union.
    const windows: Array<{ from_ms: number; to_ms: number }> =
      key === "hitches"
        ? derivedWindows.hitches ?? []
        : key === "encoder_saturation"
          ? derivedWindows.encoder_saturation ?? []
          : derivedWindows.likely_network_congestion ?? [];
    return windows
      .map((w) => ({
        x1: Math.max(0, toSvgX(windowFromMs + w.from_ms)),
        x2: Math.min(width, toSvgX(windowFromMs + w.to_ms)),
      }))
      .filter((b) => b.x2 > b.x1);
  }, [laneDef.highlightKey, derivedWindows, toSvgX, windowFromMs, width]);

  const handleMouseMove = useCallback(
    (e: React.MouseEvent<SVGSVGElement>) => {
      const rect = svgRef.current?.getBoundingClientRect();
      if (!rect) return;
      const tsAtX = xMin + ((e.clientX - rect.left) / (rect.width || 1)) * xRange;

      const values: TooltipState["values"] = [];
      for (const s of laneDef.series) {
        const pts = series[s.key];
        if (!pts || pts.length === 0) continue;
        let closest = pts[0];
        let minDist = Math.abs(pts[0].ts_unix_ms - tsAtX);
        for (const p of pts) {
          const d = Math.abs(p.ts_unix_ms - tsAtX);
          if (d < minDist) {
            minDist = d;
            closest = p;
          }
        }
        // Manifest estimator/window/why ride the hover row: a lane label says
        // the unit only, and this chart puts a rolling multi-minute median on
        // the same axis as a ~5 s mean.
        const info = seriesInfo(s.key);
        values.push({
          label: s.label,
          v: closest.v,
          color: s.color,
          qual: estimatorLabel(info),
          title: metricTooltip(info),
        });
      }

      // Events within 2% of the domain read as "at" the cursor.
      const threshold = xRange * 0.02;
      const nearEvents = events.filter((ev) => Math.abs(ev.ts_unix_ms - tsAtX) < threshold);

      onHover({ x: e.clientX, y: e.clientY, laneLabel: laneDef.label, values, nearEvents });
    },
    [xMin, xRange, laneDef, series, events, onHover],
  );

  const handleMouseLeave = useCallback(() => onHover(null), [onHover]);

  const primary = rendered[0];

  return (
    <>
      <div className="trace-lane-caption">
        {rendered.map((r) => (
          <span key={r.key} data-scale-for={r.key} className="trace-lane-scale-item">
            <i className="trace-lane-swatch" style={{ background: r.color }} aria-hidden="true" />
            <span className="num">
              {r.label} · max {fmtScale(r.max)}
            </span>
          </span>
        ))}
        <span className="num trace-lane-value">
          {primary.last == null ? "—" : fmtScale(primary.last)}
          <span className="trace-lane-unit">{laneDef.unit}</span>
        </span>
      </div>

      <svg
        ref={svgRef}
        className="trace-lane-plot"
        viewBox={`0 0 ${width} ${LANE_H}`}
        preserveAspectRatio="none"
        onMouseMove={handleMouseMove}
        onMouseLeave={handleMouseLeave}
        aria-hidden="true"
      >
        {/* derived-window bands sit under everything: context, not content */}
        {laneDef.highlightColor &&
          bands.map((b, i) => (
            <rect
              key={i}
              x={b.x1}
              y={0}
              width={b.x2 - b.x1}
              height={LANE_H}
              fill={laneDef.highlightColor}
            />
          ))}

        {/* baseline / midline / top rule */}
        {[0, 0.5, 1].map((f) => (
          <line
            key={f}
            x1={0}
            y1={LANE_H * f}
            x2={width}
            y2={LANE_H * f}
            stroke="var(--line)"
            strokeWidth={1}
            vectorEffect="non-scaling-stroke"
          />
        ))}

        {primary.fillD && (
          <>
            <defs>
              <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
                <stop offset="0" stopColor={primary.color} stopOpacity={0.2} />
                <stop offset="1" stopColor={primary.color} stopOpacity={0} />
              </linearGradient>
            </defs>
            <path d={primary.fillD} fill={`url(#${gradientId})`} stroke="none" />
          </>
        )}

        {rendered.map((r) =>
          r.d ? (
            <path
              key={r.key}
              data-series={r.key}
              d={r.d}
              fill="none"
              stroke={r.color}
              strokeWidth={1.6}
              strokeLinejoin="round"
              strokeLinecap="round"
              vectorEffect="non-scaling-stroke"
            />
          ) : null,
        )}

        {/* where a series' samples stop short of the axis */}
        {rendered.map((r) =>
          r.end ? (
            <circle
              key={`${r.key}-end`}
              data-series-end={r.key}
              cx={r.end.x}
              cy={r.end.y}
              r={2}
              fill={r.color}
            />
          ) : null,
        )}
      </svg>
    </>
  );
});

// ── EventTrack ─────────────────────────────────────────────────────────────────
//
// Events get their OWN 16px track under the lane stack rather than a dashed line
// drawn through every lane (#124: a dense burst turned the chart into a barcode
// and the dashes cut through the lane captions). Markers closer together than
// MARKER_MIN_GAP_PX collapse into one, so a burst reads as a single annotated
// moment; the title says how many.

const MARKER_MIN_GAP_PX = 3;

interface EventTrackProps {
  events: TraceEvent[];
  xMin: number;
  xMax: number;
  width: number;
  trackRef: (el: HTMLElement | null) => void;
}

const EventTrack = memo(function EventTrack({
  events,
  xMin,
  xMax,
  width,
  trackRef,
}: EventTrackProps) {
  const marks = useMemo(() => {
    const xRange = xMax - xMin || 1;
    const out: Array<{ pct: number; x: number; type: string; n: number; ts: number }> = [];

    for (const ev of [...events].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms)) {
      if (ev.ts_unix_ms < xMin || ev.ts_unix_ms > xMax) continue;
      const frac = (ev.ts_unix_ms - xMin) / xRange;
      const x = frac * width;
      const prev = out[out.length - 1];
      if (prev && prev.type === ev.type && x - prev.x < MARKER_MIN_GAP_PX) {
        prev.n += 1;
        continue;
      }
      out.push({ pct: frac * 100, x, type: ev.type, n: 1, ts: ev.ts_unix_ms });
    }
    return out;
  }, [events, xMin, xMax, width]);

  return (
    // The measured element (#124): it is the lanes' own content column and it
    // renders only once the bundle has loaded, which is exactly the case a
    // mount-only effect could never observe.
    <div className="trace-markers" ref={trackRef}>
      {marks.map((m, i) => (
        <i
          key={i}
          className="trace-mark"
          data-event-type={m.type}
          title={`${m.type}${m.n > 1 ? ` ×${m.n}` : ""} · ${Math.round(
            (xMax - m.ts) / 1000,
          )}s before the end of the window`}
          style={{ left: `${m.pct.toFixed(2)}%`, background: eventColor(m.type) }}
        />
      ))}
    </div>
  );
});

// ── XAxis ──────────────────────────────────────────────────────────────────────
//
// Ticks land on a round number of seconds and are labelled as time BEFORE the
// right edge, which is where "now" is on a live session. Dividing the domain
// into `floor(width / 80)` equal parts — what this did before #124 — produces
// labels like +0/+6/+13/+19/+26s, which no operator can read a duration off.

const NICE_STEPS_S = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600];

/** Exported for the unit test: the tick step is the whole readability fix. */
export function niceTickStepSeconds(rangeSeconds: number, innerWidth: number): number {
  const target = Math.max(2, Math.floor(innerWidth / 80));
  const ideal = rangeSeconds / target;
  // Prefer the smallest round step that is at least `ideal`, but never one so
  // coarse that the axis would carry a single label.
  const usable = NICE_STEPS_S.filter((step) => step <= rangeSeconds || step === NICE_STEPS_S[0]);
  return usable.find((step) => step >= ideal) ?? usable[usable.length - 1] ?? NICE_STEPS_S[0];
}

interface XAxisProps {
  xMin: number;
  xMax: number;
  width: number;
  /** The right edge is "now" only while the session is still producing samples. */
  live: boolean;
}

const XAxis = memo(function XAxis({ xMin, xMax, width, live }: XAxisProps) {
  const ticks = useMemo(() => {
    const rangeMs = xMax - xMin || 1;
    const rangeS = rangeMs / 1000;
    const step = niceTickStepSeconds(rangeS, width);

    const out: Array<{ pct: number; label: string; anchor: "start" | "mid" | "end" }> = [];
    for (let ago = 0; ago <= rangeS + 0.001; ago += step) {
      const pct = ((rangeS - ago) / rangeS) * 100;
      out.push({
        pct,
        label: ago === 0 ? (live ? "now" : "end") : `-${fmtAgo(ago)}`,
        anchor: ago === 0 ? "end" : pct <= 0.01 ? "start" : "mid",
      });
    }
    return out.reverse();
  }, [xMin, xMax, width, live]);

  return (
    <div className="trace-axis">
      {ticks.map((t) => (
        <span
          key={t.pct}
          className="num trace-axis-tick"
          data-anchor={t.anchor}
          style={{
            left: `${t.pct.toFixed(2)}%`,
            transform:
              t.anchor === "end"
                ? "translateX(-100%)"
                : t.anchor === "start"
                  ? "none"
                  : "translateX(-50%)",
          }}
        >
          {t.label}
        </span>
      ))}
    </div>
  );
});

// ── Tooltip ────────────────────────────────────────────────────────────────────

function Tooltip({ tooltip }: { tooltip: TooltipState }) {
  return (
    <div className="trace-tooltip" style={{ left: tooltip.x + 12, top: tooltip.y - 8 }}>
      <div className="trace-tooltip-lane">{tooltip.laneLabel}</div>
      {tooltip.values.map((v) => (
        <div key={v.label} className="trace-tooltip-row" title={v.title || undefined}>
          <span style={{ color: v.color }}>
            {v.label}
            {v.qual && <span className="trace-tooltip-qual"> · {v.qual}</span>}
          </span>
          <span className="trace-tooltip-val">{v.v.toFixed(2)}</span>
        </div>
      ))}
      {tooltip.nearEvents.map((ev, i) => (
        <div key={i} className="trace-tooltip-event" style={{ color: eventColor(ev.type) }}>
          {ev.type}
        </div>
      ))}
    </div>
  );
}

// ── ClockBadge ─────────────────────────────────────────────────────────────────

function ClockBadge({ clock }: { clock: DiagnosticBundle["clock"] }) {
  if ("unmeasured" in clock) {
    return (
      <Chip variant="warning" className="trace-clock-badge">
        clock unmeasured
      </Chip>
    );
  }
  return (
    <Chip variant="neutral" className="trace-clock-badge">
      clock ±{clock.uncertainty_ms.toFixed(1)}ms
    </Chip>
  );
}

// ── Verdict ────────────────────────────────────────────────────────────────────

// ST-07 (#324): the old classifier's overloaded "unknown" is split into "nominal"
// (a healthy session) and "indeterminate_client_hidden" (the classifier can't
// assess presentation only because the tab was hidden/backgrounded).
const VERDICT_LABELS: Record<string, string> = {
  likely_encoder_saturation: "Likely: Encoder Saturation",
  likely_network_congestion: "Likely: Network Congestion",
  likely_client_presentation_limit: "Likely: Client Presentation Limit",
  nominal: "Nominal",
  indeterminate_client_hidden: "Indeterminate (client hidden)",
  unknown: "Verdict: Unknown",
};

// Each verdict takes the state token that already means what the verdict means:
// encoder saturation = warning, network congestion = info (the console's
// transport hue), a client-side limit = accent (its client/browser hue),
// nominal = success. #124: these were hardcoded GitHub-Primer hexes that did
// not follow [data-theme="light"].
const VERDICT_VARIANTS: Record<string, ChipVariant> = {
  likely_encoder_saturation: "warning",
  likely_network_congestion: "info",
  likely_client_presentation_limit: "accent",
  nominal: "success",
  indeterminate_client_hidden: "neutral",
  unknown: "neutral",
};

const TIER_LABELS: Record<string, string> = {
  full: "full (host + client, clocks aligned)",
  host_only: "host only",
  client_only: "client only",
  insufficient: "insufficient",
};

function formatFalsifierValue(f: Falsifier): string {
  if (f.value == null) return "—";
  if (f.unit === "bool") return f.value ? "true" : "false";
  const rounded = Math.abs(f.value) >= 100 ? Math.round(f.value) : Math.round(f.value * 100) / 100;
  return f.unit === "count" ? String(rounded) : `${rounded} ${f.unit}`;
}

function formatThreshold(f: Falsifier): string {
  if (f.unit === "bool") return `${f.op} ${f.threshold ? "true" : "false"}`;
  return `${f.op} ${f.threshold}${f.unit === "count" ? "" : ` ${f.unit}`}`;
}

function FalsifierRow({ f }: { f: Falsifier }) {
  // The series name is a taxonomy name; an unknown one yields no tooltip and
  // renders exactly as before (the verdict vocabulary grows server-side).
  const why = metricTooltip(seriesInfo(f.name));
  // ✓ / ✗ answer "does the data satisfy the condition the verdict relies on?".
  // For a likely_* verdict the conditions that FIRED are the ones that hold, so
  // a ✓ is not "good" — it is "this leg of the argument stands".
  return (
    <li title={why || undefined}>
      <span aria-hidden="true">{f.holds ? "✓" : "✗"}</span>{" "}
      <span className="sr-only">{f.holds ? "holds" : "does not hold"}</span>
      {f.name} {f.estimator} = {formatFalsifierValue(f)} (needs {formatThreshold(f)}, n={f.n})
      {f.note ? ` — ${f.note}` : ""}
    </li>
  );
}

// The strip is one row — chip, one-line reason, evidence toggle — over a panel
// holding the prose evidence, the ST-09 falsifiers, and the tier/clock footnote.
function VerdictStrip({ classifier }: { classifier: DiagnosticBundle["classifier"] }) {
  const [open, setOpen] = useState(false);
  const label = VERDICT_LABELS[classifier.verdict] ?? classifier.verdict;
  const variant = VERDICT_VARIANTS[classifier.verdict] ?? "neutral";
  // Pre-ST-09 control planes return only verdict + evidence, and this admin page
  // is routinely pointed at a stack that has not been redeployed yet. Every new
  // field is therefore read defensively: an older bundle renders exactly as it
  // did before rather than throwing.
  const falsifiers = classifier.falsifiers ?? [];
  const clock = classifier.clock;
  const tier = classifier.evidence_tier;
  const hasDetail = classifier.evidence.length > 0 || falsifiers.length > 0 || !!tier || !!clock;

  return (
    <>
      <div className="trace-verdict-strip">
        <Chip variant={variant}>{label}</Chip>
        {classifier.reason && (
          <span className="trace-verdict-line" title={classifier.reason}>
            {classifier.reason}
          </span>
        )}
        {hasDetail && (
          <button
            className="btn btn-sm btn-ghost trace-verdict-toggle"
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
            type="button"
          >
            {open ? "hide" : "evidence"}
          </button>
        )}
      </div>

      {open && (
        <div className="trace-evidence">
          {classifier.evidence.length > 0 && (
            <ul className="trace-evidence-list">
              {classifier.evidence.map((e, i) => (
                <li key={i}>{e}</li>
              ))}
            </ul>
          )}
          {falsifiers.length > 0 && (
            <ul className="trace-evidence-list">
              {falsifiers.map((f) => (
                <FalsifierRow key={`${f.name}:${f.estimator}`} f={f} />
              ))}
            </ul>
          )}
          {(tier || clock) && (
            <div className="num trace-evidence-meta">
              {tier && <>evidence: {TIER_LABELS[tier] ?? tier}</>}
              {tier && clock && " · "}
              {clock &&
                (clock.quality === "measured" ? (
                  <>
                    clock: measured
                    {clock.offset_ms != null && ` (offset ${Math.round(clock.offset_ms * 10) / 10} ms`}
                    {clock.uncertainty_ms != null &&
                      `, ±${Math.round(clock.uncertainty_ms * 10) / 10} ms`}
                    {clock.offset_ms != null && ")"}
                  </>
                ) : (
                  <span className="trace-clock-unmeasured">clock: unmeasured</span>
                ))}
            </div>
          )}
        </div>
      )}
    </>
  );
}

// ── Legend ─────────────────────────────────────────────────────────────────────

function TraceLegend({ events }: { events: TraceEvent[] }) {
  const { types, hasOther } = useMemo(() => {
    const present = [...new Set(events.map((e) => e.type))];
    return {
      types: present.filter((t) => t in EVENT_COLORS),
      hasOther: present.some((t) => !(t in EVENT_COLORS)),
    };
  }, [events]);

  if (types.length === 0 && !hasOther) return null;

  return (
    <div className="trace-legend">
      {types.map((t) => (
        <span key={t} className="trace-legend-item">
          <i className="trace-legend-swatch" style={{ background: eventColor(t) }} aria-hidden="true" />
          <span className="num">{t}</span>
        </span>
      ))}
      {hasOther && (
        <span
          className="trace-legend-item"
          title="lifecycle and negotiation events — hover a lane to name the one under the cursor"
        >
          <i
            className="trace-legend-swatch"
            style={{ background: OTHER_EVENT_COLOR }}
            aria-hidden="true"
          />
          <span className="num">other event</span>
        </span>
      )}
    </div>
  );
}

// ── Window selector ────────────────────────────────────────────────────────────
//
// A view crop over the bundle already fetched, not a second request: the server
// clamps the diagnostic-bundle window to [2, 10] minutes and defaults to 5, so a
// wider position than 5m would silently show the same data under a longer label.
// The reference mockup's third position (30m) is unimplementable against this
// endpoint for that reason.
const WINDOW_OPTIONS = [
  { value: "60", label: "60s" },
  { value: "300", label: "5m" },
] as const;
type WindowValue = (typeof WINDOW_OPTIONS)[number]["value"];
const DEFAULT_WINDOW: WindowValue = "300";

// Session states considered non-terminal for live polling purposes (schema.md /
// api/types.ts SessionState). A session in any other state (stopped/failed, or the
// caller passing nothing) will not poll — matches #139/#150: no background work once
// there's nothing left to observe.
const NON_TERMINAL_STATES = new Set(["pending", "assigned", "starting", "running", "stopping"]);

// Poll interval while live (ST-07 #324: ~2-5s per the issue).
const POLL_INTERVAL_MS = 4000;

// ── Main TraceViewer ───────────────────────────────────────────────────────────

interface TraceViewerProps {
  sessionId: string;
  token: string;
  /** Session lifecycle state, if known. Polling runs only while this is non-terminal;
   * omitted/undefined means "unknown" and polling is skipped (manual Refresh still
   * works). Passing a terminal state (or none) stops any in-flight polling. */
  sessionState?: string;
}

export function TraceViewer({ sessionId, token, sessionState }: TraceViewerProps) {
  const [bundle, setBundle] = useState<DiagnosticBundle | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [tooltip, setTooltip] = useState<TooltipState | null>(null);
  const [viewWindow, setViewWindow] = useState<WindowValue>(DEFAULT_WINDOW);
  // Hovering the plot holds the chart still: a live poll that redraws under the
  // cursor moves the sample the tooltip is describing.
  const [hovering, setHovering] = useState(false);

  const [trackRef, plotWidth] = useContainerWidth(900);
  const gradientBase = useId().replace(/:/g, "_");

  // Refetches without dropping into the full-page loading state — used by the poll
  // timer so a live-updating chart doesn't flash "Loading trace…" every tick (#139/#150
  // re-render isolation: only the data changes, not the loading skeleton).
  const refetch = useCallback(async () => {
    try {
      const b = await getDiagnosticBundle(token, sessionId);
      setBundle(b);
      setLoadError(null);
    } catch (e: unknown) {
      setLoadError(e instanceof ApiError ? e.message : "could not load trace bundle");
    }
  }, [token, sessionId]);

  const load = useCallback(async () => {
    setLoading(true);
    setLoadError(null);
    try {
      const b = await getDiagnosticBundle(token, sessionId);
      setBundle(b);
    } catch (e: unknown) {
      setLoadError(e instanceof ApiError ? e.message : "could not load trace bundle");
    } finally {
      setLoading(false);
    }
  }, [token, sessionId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Live auto-update (#324): poll on an interval while the session is non-terminal.
  // Stops automatically on a terminal state or unmount — no manual toggle needed to
  // avoid a leaked timer.
  const isLive = sessionState != null && NON_TERMINAL_STATES.has(sessionState);
  const paused = isLive && hovering;
  useEffect(() => {
    if (!isLive) return;
    const id = window.setInterval(() => {
      if (hovering) return;
      void refetch();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(id);
  }, [isLive, hovering, refetch]);

  // The sampled domain, before the view crop.
  const { dataMin, dataMax } = useMemo(() => {
    if (!bundle) return { dataMin: 0, dataMax: 1 };
    const allTs = Object.values(bundle.series)
      .flatMap((pts) => pts ?? [])
      .map((p) => p.ts_unix_ms);
    allTs.push(...bundle.events.map((e) => e.ts_unix_ms));
    if (allTs.length === 0) {
      return { dataMin: bundle.window.from_ms, dataMax: bundle.window.to_ms };
    }
    return { dataMin: arrMin(allTs), dataMax: arrMax(allTs) };
  }, [bundle]);

  // The visible domain: the last N seconds of what was sampled, anchored on the
  // newest sample rather than on wall-clock now, so a stopped session still
  // fills its lanes.
  const xMax = dataMax;
  const xMin = Math.max(dataMin, dataMax - Number(viewWindow) * 1000);

  const seriesMap = useMemo(() => bundle?.series ?? {}, [bundle]);

  // The axis is drawn over the VISIBLE range, which can be much shorter than the
  // window the verdict was computed over (a crop, a host that stopped reporting,
  // a client that joined late). Saying so is the difference between "38 s of a
  // 300 s window" and a chart that looks truncated (#124).
  const axisNote = useMemo(() => {
    if (!bundle) return null;
    const shown = xMax - xMin;
    const sampled = dataMax - dataMin;
    const declared = bundle.window.to_ms - bundle.window.from_ms;
    if (shown <= 0) return null;
    if (sampled > 0 && shown < sampled * 0.99) {
      return `showing the last ${fmtSeconds(shown)} of ${fmtSeconds(sampled)} sampled`;
    }
    if (declared <= 0 || shown >= declared * 0.9) return null;
    return `axis shows the sampled range — ${fmtSeconds(shown)} of a ${fmtSeconds(declared)} window`;
  }, [bundle, xMin, xMax, dataMin, dataMax]);

  const hasData = !!bundle && Object.values(bundle.series).some((pts) => (pts?.length ?? 0) > 0);
  const isEmpty = !!bundle && !hasData && bundle.events.length === 0;

  return (
    <div className="card trace-card">
      <div className="panel-head">
        <span className="panel-title">Trace</span>
        <span className="hint">stacked time-series + events</span>
        <div className="acts">
          <SegmentedControl
            options={WINDOW_OPTIONS.map((o) => ({ value: o.value, label: o.label }))}
            value={viewWindow}
            onChange={(v) => setViewWindow(v as WindowValue)}
            aria-label="Trace window"
          />
          {bundle && <ClockBadge clock={bundle.clock} />}
          {isLive && (
            <Chip
              variant={paused ? "warning" : "success"}
              dot
              title={
                paused
                  ? "paused while the pointer is over the chart"
                  : `auto-updating every ${POLL_INTERVAL_MS / 1000}s`
              }
            >
              {paused ? "paused" : "live"}
            </Chip>
          )}
          {/* Rendered in every state so a failed load still offers a retry. */}
          <button className="btn btn-sm btn-ghost" onClick={() => void load()} type="button">
            <IconRefresh />
            Refresh
          </button>
        </div>
      </div>

      {loading ? (
        <p className="trace-state muted">Loading trace…</p>
      ) : loadError ? (
        <p className="trace-state form-error">{loadError}</p>
      ) : !bundle ? null : isEmpty ? (
        <p className="trace-state muted">No trace data for this session yet.</p>
      ) : (
        <>
          <VerdictStrip classifier={bundle.classifier} />

          <div
            className="trace-body"
            onMouseEnter={() => setHovering(true)}
            onMouseLeave={() => setHovering(false)}
          >
            {LANES.map((lane) => (
              <div key={lane.id} className="trace-lane-row">
                <span className="eyebrow trace-lane-label">{lane.label}</span>
                <div className="trace-lane-col">
                  <LaneChart
                    laneDef={lane}
                    series={seriesMap}
                    events={bundle.events}
                    xMin={xMin}
                    xMax={xMax}
                    width={plotWidth}
                    gradientId={`${gradientBase}-${lane.id}`}
                    derivedWindows={bundle.derived_windows}
                    windowFromMs={bundle.window.from_ms}
                    onHover={setTooltip}
                  />
                </div>
              </div>
            ))}

            <div className="trace-lane-row trace-event-row">
              <span className="eyebrow trace-lane-label">Events</span>
              <EventTrack
                events={bundle.events}
                xMin={xMin}
                xMax={xMax}
                width={plotWidth}
                trackRef={trackRef}
              />
            </div>

            <div className="trace-lane-row trace-axis-row">
              <span />
              <XAxis xMin={xMin} xMax={xMax} width={plotWidth} live={isLive} />
            </div>

            <div className="trace-lane-row trace-legend-row">
              <span />
              <div>
                <TraceLegend events={bundle.events} />
                {axisNote && <p className="trace-axis-note">{axisNote}</p>}
              </div>
            </div>
          </div>
        </>
      )}

      {/* Hover tooltip */}
      {tooltip && <Tooltip tooltip={tooltip} />}
    </div>
  );
}
