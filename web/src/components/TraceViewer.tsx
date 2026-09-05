/**
 * TraceViewer — stacked time-series lane chart for the admin session-detail page.
 *
 * Renders 4 stacked lanes (Encoder / Transport / Client / ABR) sharing a common
 * time axis, with event markers as vertical dashed lines and derived-window
 * highlight bands. Reads from GET /v1/admin/sessions/{id}/diagnostic-bundle.
 *
 * Design goal: "make a hitch obvious." No external chart deps — hand-rolled SVG
 * following the Charts.tsx / TelemetryChart.tsx patterns.
 *
 * ST-07 — lazy-loaded into the session detail's Diagnostics disclosure.
 */

import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { getDiagnosticBundle } from "../api/admin";
import { ApiError } from "../api/client";
import type { DiagnosticBundle, Falsifier, TraceSeriesPoint, TraceEvent } from "../api/types";
import { estimatorLabel, metricTooltip, seriesInfo } from "../lib/metricsManifest";
import { useContainerWidth } from "../lib/useContainerWidth";
import { Chip, LiveDot } from "./Chip";

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

// ── Event marker color by type ─────────────────────────────────────────────────

// The loud events — the ones an operator is looking for when a session misbehaves.
// Any other type still renders as a marker in OTHER_EVENT_COLOR and is accounted
// for by a single "other event" legend entry, because the type vocabulary is
// open-ended and a legend that silently omits half the markers on the chart is
// worse than one that buckets them.
const EVENT_COLORS: Record<string, string> = {
  "abr.retarget": "var(--warning)",
  "encoder.stall": "var(--danger)",
  "client.freeze_detected": "var(--danger-text)",
  // ABR resolution ladder (T6): distinct color so a rung step reads apart from
  // a plain bitrate retarget. No allow-list exists here — any event type not
  // in this map still renders as a marker (eventColor's fallback below), so
  // "abr.ladder.step" was already reachable; this only gives it its own color.
  "abr.ladder.step": "var(--danger-text)",
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
  /** True = render as area fill (bars approximation) instead of a line */
  area?: boolean;
}

interface LaneDef {
  label: string;
  series: LaneSeries[];
  /** Keys for derived_windows highlight bands: "hitches" / "encoder_saturation" */
  highlightKey?: "hitches" | "encoder_saturation" | "likely_network_congestion";
  highlightColor?: string;
}

const LANES: LaneDef[] = [
  {
    label: "Encoder",
    series: [
      { key: "encoder.fps",       color: "var(--accent)",  label: "fps" },
      // Amber, not the console's usual lavender for encode time: inside ONE lane
      // the two series have to be told apart at a glance, and amber is also the
      // hue of this lane's encoder-saturation band — which encode_ms is what
      // detects.
      { key: "encoder.encode_ms", color: "var(--warning)", label: "encode ms" },
    ],
    highlightKey: "encoder_saturation",
    highlightColor: "var(--warning-bg)",
  },
  {
    label: "Transport",
    series: [
      { key: "transport.rtt_ms",       color: "var(--info)",   label: "rtt ms" },
      { key: "transport.packets_lost", color: "var(--danger)", label: "pkt lost", area: true },
    ],
    highlightKey: "likely_network_congestion",
    highlightColor: "var(--danger-bg)",
  },
  {
    label: "Client",
    series: [
      { key: "client.present_interval_sd_ms", color: "var(--lavender)", label: "present σ ms" },
      { key: "client.glass_to_glass_ms",      color: "var(--success)",  label: "qualified RVFC capture-to-display ms" },
    ],
    highlightKey: "hitches",
    highlightColor: "var(--danger-bg)",
  },
  {
    label: "ABR",
    series: [
      { key: "abr.setpoint_kbps", color: "var(--accent)", label: "setpoint kbps" },
    ],
  },
];

// ── Tooltip state ──────────────────────────────────────────────────────────────

interface TooltipState {
  x: number;
  y: number;
  laneLabel: string;
  values: Array<{ label: string; v: number; color: string; qual?: string; title?: string }>;
  nearEvents: TraceEvent[];
}

// ── Lane geometry ──────────────────────────────────────────────────────────────
//
// A lane is a scale caption row above a plot box. The caption is what makes the
// plot readable at all: series inside one lane are scaled INDEPENDENTLY (#124 —
// fps ~60 and encode_ms ~20 on one shared scale flattened encode_ms onto the
// baseline), so the only way to know what the top of a curve is worth is to say
// so. Charts.tsx puts that in a y-axis gutter; a 4-lane stack has no room for
// four gutters, and a gutter can only ever label one of the two series anyway.

const LANE_PAD = { top: 5, right: 6, bottom: 5, left: 6 };
const LANE_SCALE_H = 15;
const LANE_PLOT_H = 62;
const LANE_HEIGHT = LANE_SCALE_H + LANE_PLOT_H;

/** Height of the whole lane stack — the event overlay is sized from it. */
const LANE_STACK_H = LANES.length * LANE_HEIGHT;

interface SeriesRender {
  key: string;
  color: string;
  label: string;
  /** Highest sample in the window; the caption's "max". */
  max: number;
  /** The stroked outline. Empty when the series is flat at zero. */
  d: string;
  /** Area fill under `d`, for count-like series only. Never stroked: stroking a
   *  closed area paints its baseline edge too, which draws a hard rule across
   *  the whole lane. */
  fillD: string;
  isArea: boolean;
  /** Set when the series' last sample falls short of the shared x domain. */
  end: { x: number; y: number } | null;
}

interface LaneSvgProps {
  laneDef: LaneDef;
  series: Record<string, TraceSeriesPoint[] | undefined>;
  /** Hover only — the markers themselves are drawn once by MarkerOverlay. */
  events: TraceEvent[];
  xMin: number;
  xMax: number;
  width: number;
  derivedWindows: DiagnosticBundle["derived_windows"];
  windowFromMs: number;
  onHover: (tooltip: TooltipState | null) => void;
}

const LaneSvg = memo(function LaneSvg({
  laneDef,
  series,
  events,
  xMin,
  xMax,
  width,
  derivedWindows,
  windowFromMs,
  onHover,
}: LaneSvgProps) {
  const svgRef = useRef<SVGSVGElement>(null);

  const xRange = xMax - xMin || 1;
  const innerW = Math.max(width - LANE_PAD.left - LANE_PAD.right, 1);
  const innerH = LANE_PLOT_H - LANE_PAD.top - LANE_PAD.bottom;

  const toSvgX = useCallback(
    (ts: number) => LANE_PAD.left + ((ts - xMin) / xRange) * innerW,
    [xMin, xRange, innerW],
  );

  const rendered = useMemo<SeriesRender[]>(() => {
    return laneDef.series.flatMap((s) => {
      const pts = [...(series[s.key] ?? [])].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms);
      if (pts.length === 0) return [];

      const max = arrMax(pts.map((p) => p.v));
      // Own scale, always anchored at zero (the v3 mock's lineChart does the
      // same) so a curve's height is proportional to its value.
      const scaleMax = max > 0 ? max * 1.1 : 1;
      const toY = (v: number) => LANE_PAD.top + innerH - (v / scaleMax) * innerH;

      // Count-like series (packets lost) get an area fill under the same line.
      // Drawing one isolated tick per sample — what this did before #124 —
      // spaces them ~17px apart at any real width, so a low-but-nonzero count
      // rendered as a row of dots that read as a rendering artifact.
      const d =
        s.area && max <= 0
          ? "" // flat at zero: draw nothing rather than a hairline of dots
          : pts
              .map((p, i) => `${i === 0 ? "M" : "L"}${toSvgX(p.ts_unix_ms).toFixed(1)},${toY(p.v).toFixed(1)}`)
              .join(" ");

      const base = (LANE_PAD.top + innerH).toFixed(1);
      const fillD =
        s.area && d
          ? `M${toSvgX(pts[0].ts_unix_ms).toFixed(1)},${base} ${d.slice(1)} L${toSvgX(
              pts[pts.length - 1].ts_unix_ms,
            ).toFixed(1)},${base} Z`
          : "";

      // "This series stopped" has to be judged against the series' OWN cadence:
      // a client sampling once a second is always ~1 s behind the domain end,
      // and a flat 2%-of-range threshold would cap every live series on the
      // chart. Three missed samples is a stop; one is the cadence.
      const gaps = pts.slice(1).map((p, i) => p.ts_unix_ms - pts[i].ts_unix_ms).sort((a, b) => a - b);
      const medianGap = gaps.length > 0 ? gaps[Math.floor(gaps.length / 2)] : 0;
      const endThreshold = xMax - Math.max(xRange * 0.02, medianGap * 3);

      const last = pts[pts.length - 1];
      const end =
        last.ts_unix_ms < endThreshold
          ? { x: toSvgX(last.ts_unix_ms), y: toY(last.v) }
          : null;

      return [{ key: s.key, color: s.color, label: s.label, max, d, fillD, isArea: !!s.area, end }];
    });
  }, [laneDef.series, series, toSvgX, innerH, xMax, xRange]);

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
    return windows.map((w) => ({
      x1: toSvgX(windowFromMs + w.from_ms),
      x2: toSvgX(windowFromMs + w.to_ms),
    }));
  }, [laneDef.highlightKey, derivedWindows, toSvgX, windowFromMs]);

  const handleMouseMove = useCallback(
    (e: React.MouseEvent<SVGSVGElement>) => {
      const rect = svgRef.current?.getBoundingClientRect();
      if (!rect) return;
      const svgX = e.clientX - rect.left;
      const tsAtX = xMin + ((svgX - LANE_PAD.left) / innerW) * xRange;

      // Find nearest data values for each series
      const values: TooltipState["values"] = [];
      for (const s of laneDef.series) {
        const pts = series[s.key];
        if (!pts || pts.length === 0) continue;
        // Find closest point by ts
        let closest = pts[0];
        let minDist = Math.abs(pts[0].ts_unix_ms - tsAtX);
        for (const p of pts) {
          const d = Math.abs(p.ts_unix_ms - tsAtX);
          if (d < minDist) { minDist = d; closest = p; }
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
    [xMin, xRange, innerW, laneDef, series, events, onHover],
  );

  const handleMouseLeave = useCallback(() => onHover(null), [onHover]);

  return (
    <>
      <div className="trace-lane-scale" style={{ height: LANE_SCALE_H }}>
        {rendered.map((r) => (
          <span key={r.key} data-scale-for={r.key} className="trace-lane-scale-item">
            <span className="trace-lane-swatch" style={{ background: r.color }} aria-hidden="true" />
            {r.label} · max {fmtScale(r.max)}
          </span>
        ))}
      </div>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${width} ${LANE_PLOT_H}`}
        width={width}
        height={LANE_PLOT_H}
        style={{ display: "block", overflow: "visible" }}
        onMouseMove={handleMouseMove}
        onMouseLeave={handleMouseLeave}
        aria-hidden="true"
      >
        {/* highlight bands */}
        {laneDef.highlightColor &&
          bands.map((b, i) => (
            <rect
              key={i}
              x={Math.max(LANE_PAD.left, b.x1)}
              y={LANE_PAD.top}
              width={Math.max(0, Math.min(b.x2, width - LANE_PAD.right) - Math.max(LANE_PAD.left, b.x1))}
              height={innerH}
              fill={laneDef.highlightColor}
            />
          ))}

        {/* lane frame — the top rule is what stops two lanes reading as one */}
        <line
          x1={LANE_PAD.left}
          y1={LANE_PAD.top}
          x2={width - LANE_PAD.right}
          y2={LANE_PAD.top}
          stroke="var(--line)"
          strokeWidth={0.5}
        />
        <line
          x1={LANE_PAD.left}
          y1={LANE_PAD.top + innerH}
          x2={width - LANE_PAD.right}
          y2={LANE_PAD.top + innerH}
          stroke="var(--line-2)"
          strokeWidth={0.5}
        />

        {/* series — fill first, then the line over it */}
        {rendered.map((r) =>
          r.fillD ? (
            <path key={`${r.key}-fill`} d={r.fillD} fill={r.color} fillOpacity={0.26} stroke="none" />
          ) : null,
        )}
        {rendered.map((r) =>
          r.d ? (
            <path
              key={r.key}
              data-series={r.key}
              d={r.d}
              fill="none"
              stroke={r.color}
              strokeWidth={r.isArea ? 1.2 : 1.5}
              strokeLinejoin="round"
              strokeLinecap="round"
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

// ── MarkerOverlay ──────────────────────────────────────────────────────────────
//
// Event markers are drawn ONCE across the whole lane stack rather than once per
// lane: a per-lane marker is chopped into four segments by the lane boundaries,
// and a dense burst of them turned the chart into a barcode (#124). Markers
// closer together than MARKER_MIN_GAP_PX collapse into one, so a burst reads as
// a single annotated moment.

const MARKER_MIN_GAP_PX = 3;

interface MarkerOverlayProps {
  events: TraceEvent[];
  xMin: number;
  xMax: number;
  width: number;
}

const MarkerOverlay = memo(function MarkerOverlay({ events, xMin, xMax, width }: MarkerOverlayProps) {
  const marks = useMemo(() => {
    const xRange = xMax - xMin || 1;
    const innerW = Math.max(width - LANE_PAD.left - LANE_PAD.right, 1);
    const out: Array<{ x: number; type: string; n: number }> = [];

    for (const ev of [...events].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms)) {
      if (ev.ts_unix_ms < xMin || ev.ts_unix_ms > xMax) continue;
      const x = LANE_PAD.left + ((ev.ts_unix_ms - xMin) / xRange) * innerW;
      const prev = out[out.length - 1];
      if (prev && prev.type === ev.type && x - prev.x < MARKER_MIN_GAP_PX) {
        prev.n += 1;
        continue;
      }
      out.push({ x, type: ev.type, n: 1 });
    }
    return out;
  }, [events, xMin, xMax, width]);

  return (
    <div className="trace-markers" style={{ height: LANE_STACK_H }}>
      <svg
        viewBox={`0 0 ${width} ${LANE_STACK_H}`}
        width={width}
        height={LANE_STACK_H}
        style={{ display: "block", overflow: "visible" }}
        aria-hidden="true"
      >
        {marks.map((m, i) => (
          <line
            key={i}
            x1={m.x}
            y1={0}
            x2={m.x}
            y2={LANE_STACK_H}
            stroke={eventColor(m.type)}
            strokeWidth={1}
            strokeDasharray="3 3"
            opacity={0.55}
          />
        ))}
      </svg>
    </div>
  );
});

// ── XAxis ──────────────────────────────────────────────────────────────────────
//
// Ticks land on a round number of seconds. Dividing the domain into
// `floor(width / 80)` equal parts — what this did before #124 — produces labels
// like +0/+6/+13/+19/+26s, which no operator can read a duration off.

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
}

const XAxis = memo(function XAxis({ xMin, xMax, width }: XAxisProps) {
  const ticks = useMemo(() => {
    const rangeMs = xMax - xMin || 1;
    const innerW = Math.max(width - LANE_PAD.left - LANE_PAD.right, 1);
    const step = niceTickStepSeconds(rangeMs / 1000, innerW);

    const out: Array<{ x: number; label: string }> = [];
    for (let t = 0; t * 1000 <= rangeMs + 1; t += step) {
      out.push({
        x: LANE_PAD.left + ((t * 1000) / rangeMs) * innerW,
        label: `+${t}s`,
      });
    }
    return out;
  }, [xMin, xMax, width]);

  return (
    <svg
      className="trace-axis"
      viewBox={`0 0 ${width} 16`}
      width={width}
      height={16}
      style={{ display: "block", overflow: "visible" }}
      aria-hidden="true"
    >
      {ticks.map((t) => (
        <text key={t.x} x={t.x} y={11} textAnchor="middle" fill="var(--text-4)" fontSize={9}>
          {t.label}
        </text>
      ))}
    </svg>
  );
});

// ── Tooltip ────────────────────────────────────────────────────────────────────

function Tooltip({ tooltip }: { tooltip: TooltipState }) {
  return (
    <div
      className="trace-tooltip"
      style={{ left: tooltip.x + 12, top: tooltip.y - 8 }}
    >
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

// ── VerdictPanel ───────────────────────────────────────────────────────────────

// ST-07 (#324): the old classifier's overloaded "unknown" is split into "nominal"
// (a healthy session — green) and "indeterminate_client_hidden" (grey — the classifier
// can't assess presentation only because the tab was hidden/backgrounded). The three
// likely_* verdicts stay amber/network/client-colored; genuine "unknown" stays plain.
const VERDICT_LABELS: Record<string, string> = {
  likely_encoder_saturation:        "Likely: Encoder Saturation",
  likely_network_congestion:        "Likely: Network Congestion",
  likely_client_presentation_limit: "Likely: Client Presentation Limit",
  nominal:                          "Nominal",
  indeterminate_client_hidden:      "Indeterminate (client hidden)",
  unknown:                          "Verdict: Unknown",
};

const VERDICT_CLASSES: Record<string, string> = {
  likely_encoder_saturation:        "trace-verdict-encoder",
  likely_network_congestion:        "trace-verdict-network",
  likely_client_presentation_limit: "trace-verdict-client",
  nominal:                          "trace-verdict-nominal",
  indeterminate_client_hidden:      "trace-verdict-indeterminate",
  unknown:                          "trace-verdict-unknown",
};

// ST-09: the verdict is now a VALUE, not a string plus prose. What it gained is
// what an operator could not previously check — the window and the per-source
// sample counts behind it, whether the two clocks can be aligned at all, and the
// falsifiers: the specific numbers the verdict rests on, each with its estimator,
// threshold and sample count, and whether it holds.
//
// Deliberately styled with the EXISTING verdict-block classes only. The design
// handoff has no falsifier mockup (admin-session-detail.html predates it), so
// this adds no new tokens, colours or rules — the falsifiers ride the same
// `.trace-verdict-evidence` list the prose evidence uses, and the clock reuses
// the `.trace-clock-unmeasured` chip already on this page.

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

function VerdictPanel({ classifier }: { classifier: DiagnosticBundle["classifier"] }) {
  const [open, setOpen] = useState(false);
  const label = VERDICT_LABELS[classifier.verdict] ?? classifier.verdict;
  const cls = VERDICT_CLASSES[classifier.verdict] ?? "trace-verdict-unknown";
  // Pre-ST-09 control planes return only verdict + evidence, and this admin page
  // is routinely pointed at a stack that has not been redeployed yet. Every new
  // field is therefore read defensively: an older bundle renders exactly as it
  // did before rather than throwing.
  const falsifiers = classifier.falsifiers ?? [];
  const clock = classifier.clock;
  const tier = classifier.evidence_tier;
  const hasDetail = classifier.evidence.length > 0 || falsifiers.length > 0;

  return (
    <div className={`trace-verdict ${cls}`}>
      <div className="trace-verdict-head">
        <span className="trace-verdict-label">{label}</span>
        {hasDetail && (
          <button
            className="trace-verdict-toggle"
            onClick={() => setOpen((o) => !o)}
            aria-expanded={open}
          >
            {open ? "hide" : "evidence"}
          </button>
        )}
      </div>

      {classifier.reason && <div className="trace-verdict-evidence">{classifier.reason}</div>}

      {(tier || clock) && (
        <div className="trace-verdict-evidence">
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

      {open && (
        <>
          {classifier.evidence.length > 0 && (
            <ul className="trace-verdict-evidence">
              {classifier.evidence.map((e, i) => (
                <li key={i}>{e}</li>
              ))}
            </ul>
          )}
          {falsifiers.length > 0 && (
            <ul className="trace-verdict-evidence">
              {falsifiers.map((f) => (
                <FalsifierRow key={`${f.name}:${f.estimator}`} f={f} />
              ))}
            </ul>
          )}
        </>
      )}
    </div>
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
          {/* 16px to match the series legend in Charts.tsx — at 12px two event
              colours a shade apart were indistinguishable. */}
          <svg width={16} height={10} aria-hidden="true">
            <line
              x1={0} y1={5} x2={16} y2={5}
              stroke={eventColor(t)}
              strokeWidth={1.5}
              strokeDasharray="3 3"
            />
          </svg>
          <span>{t}</span>
        </span>
      ))}
      {hasOther && (
        <span className="trace-legend-item" title="lifecycle and negotiation events — hover a lane to name the one under the cursor">
          <svg width={16} height={10} aria-hidden="true">
            <line
              x1={0} y1={5} x2={16} y2={5}
              stroke={OTHER_EVENT_COLOR}
              strokeWidth={1.5}
              strokeDasharray="3 3"
            />
          </svg>
          <span>other event</span>
        </span>
      )}
    </div>
  );
}

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

  const [plotRef, plotWidth] = useContainerWidth(600);

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
  useEffect(() => {
    if (!isLive) return;
    const id = window.setInterval(() => {
      void refetch();
    }, POLL_INTERVAL_MS);
    return () => window.clearInterval(id);
  }, [isLive, refetch]);

  // Compute shared x domain across all series
  const { xMin, xMax } = useMemo(() => {
    if (!bundle) return { xMin: 0, xMax: 1 };
    const allTs = Object.values(bundle.series)
      .flatMap((pts) => pts ?? [])
      .map((p) => p.ts_unix_ms);
    allTs.push(...bundle.events.map((e) => e.ts_unix_ms));
    if (allTs.length === 0) {
      return { xMin: bundle.window.from_ms, xMax: bundle.window.to_ms };
    }
    return { xMin: arrMin(allTs), xMax: arrMax(allTs) };
  }, [bundle]);

  // Stable series map
  const seriesMap = useMemo(() => bundle?.series ?? {}, [bundle]);

  // The lane label column; the plot itself takes the rest of the measured row.
  const LABEL_W = 80;
  const svgWidth = Math.max(plotWidth - LABEL_W, 100);

  // The axis is drawn over the SAMPLED range, which can be much shorter than the
  // window the verdict was computed over (a host that stopped reporting, a
  // client that joined late). Saying so is the difference between "38 s of a
  // 300 s window" and a chart that looks truncated (#124).
  const axisNote = useMemo(() => {
    if (!bundle) return null;
    const sampled = xMax - xMin;
    const declared = bundle.window.to_ms - bundle.window.from_ms;
    if (sampled <= 0 || declared <= 0 || sampled >= declared * 0.9) return null;
    return `axis shows the sampled range — ${fmtSeconds(sampled)} of a ${fmtSeconds(declared)} window`;
  }, [bundle, xMin, xMax]);

  const hasData =
    !!bundle && Object.values(bundle.series).some((pts) => (pts?.length ?? 0) > 0);
  const isEmpty = !!bundle && !hasData && bundle.events.length === 0;

  return (
    <div className="trace-viewer">
      {/* Top row: clock badge + live indicator + refresh. Rendered in every
          state so a failed load still offers a retry. */}
      <div className="trace-clock-row">
        {bundle && <ClockBadge clock={bundle.clock} />}
        {isLive && (
          <span title={`auto-updating every ${POLL_INTERVAL_MS / 1000}s`}>
            <LiveDot label="live" />
          </span>
        )}
        <button className="btn btn-sm" onClick={() => void load()}>
          Refresh
        </button>
      </div>

      {loading ? (
        <p className="muted">Loading trace…</p>
      ) : loadError ? (
        <p className="form-error">{loadError}</p>
      ) : !bundle ? null : isEmpty ? (
        <p className="muted">No trace data for this session yet.</p>
      ) : (
        <>
          <VerdictPanel classifier={bundle.classifier} />

          {/* Stacked lanes. The measured element is this row — the observer is
              attached by a callback ref, so it binds whenever the plot appears
              rather than only on the component's first (loading) render. */}
          <div className="trace-plot" ref={plotRef}>
            <div className="trace-plot-labels" style={{ width: LABEL_W }}>
              {LANES.map((lane) => (
                <div key={lane.label} className="trace-lane-label" style={{ height: LANE_HEIGHT }}>
                  {lane.label}
                </div>
              ))}
            </div>

            <div className="trace-plot-lanes">
              <MarkerOverlay
                events={bundle.events}
                xMin={xMin}
                xMax={xMax}
                width={svgWidth}
              />
              {LANES.map((lane) => (
                <div key={lane.label} className="trace-lane-svg" style={{ height: LANE_HEIGHT }}>
                  <LaneSvg
                    laneDef={lane}
                    series={seriesMap}
                    events={bundle.events}
                    xMin={xMin}
                    xMax={xMax}
                    width={svgWidth}
                    derivedWindows={bundle.derived_windows}
                    windowFromMs={bundle.window.from_ms}
                    onHover={setTooltip}
                  />
                </div>
              ))}
              <XAxis xMin={xMin} xMax={xMax} width={svgWidth} />
            </div>
          </div>

          {axisNote && <p className="trace-axis-note">{axisNote}</p>}

          <TraceLegend events={bundle.events} />
        </>
      )}

      {/* Hover tooltip */}
      {tooltip && <Tooltip tooltip={tooltip} />}
    </div>
  );
}
