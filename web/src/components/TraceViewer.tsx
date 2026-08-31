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

// ── helpers ────────────────────────────────────────────────────────────────────

function arrMin(a: number[]): number {
  return a.reduce((m, v) => (v < m ? v : m), Infinity);
}
function arrMax(a: number[]): number {
  return a.reduce((m, v) => (v > m ? v : m), -Infinity);
}

function useContainerWidth(fallback = 600): [React.RefObject<HTMLDivElement>, number] {
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

// ── Event marker color by type ─────────────────────────────────────────────────

const EVENT_COLORS: Record<string, string> = {
  "abr.retarget": "#f0a030",
  // ABR resolution ladder (T6): distinct color so a rung step reads apart from
  // a plain bitrate retarget. No allow-list exists here — any event type not
  // in this map still renders as a marker (eventColor's fallback below), so
  // "abr.ladder.step" was already reachable; this only gives it its own color.
  "abr.ladder.step": "var(--danger-text)",
  "playout.changed": "#a371f7",
  "pipeline.source_swapped": "#58a6ff",
  "webrtc.state_changed": "var(--muted)",
};

function eventColor(type: string): string {
  return EVENT_COLORS[type] ?? "var(--text-4)";
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
      { key: "encoder.encode_ms", color: "#f0a030", label: "encode ms" },
      { key: "encoder.fps",       color: "#5b6bff", label: "fps" },
    ],
    highlightKey: "encoder_saturation",
    highlightColor: "rgba(240,160,48,0.26)",
  },
  {
    label: "Transport",
    series: [
      { key: "transport.rtt_ms",       color: "#58a6ff", label: "rtt ms" },
      { key: "transport.packets_lost", color: "var(--danger-text)", label: "pkt lost", area: true },
    ],
    highlightKey: "likely_network_congestion",
    highlightColor: "rgba(255,90,90,0.22)",
  },
  {
    label: "Client",
    series: [
      { key: "client.present_interval_sd_ms", color: "#a371f7", label: "present σ ms" },
      { key: "client.glass_to_glass_ms",      color: "#5fe6ac", label: "qualified RVFC capture-to-display ms" },
    ],
    highlightKey: "hitches",
    highlightColor: "rgba(255,90,90,0.22)",
  },
  {
    label: "ABR",
    series: [
      { key: "abr.setpoint_kbps", color: "#f0a030", label: "setpoint kbps" },
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

// ── LaneSvg ────────────────────────────────────────────────────────────────────

const LANE_PAD = { top: 4, right: 4, bottom: 4, left: 0 };
const LANE_HEIGHT = 72;

interface LaneSvgProps {
  laneDef: LaneDef;
  series: Record<string, TraceSeriesPoint[] | undefined>;
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

  // Collect all points for this lane to compute yMax
  const allPoints = useMemo(() => {
    return laneDef.series.flatMap((s) => series[s.key] ?? []);
  }, [laneDef.series, series]);

  const xRange = xMax - xMin || 1;
  const yMax = useMemo(() => {
    if (allPoints.length === 0) return 1;
    const max = arrMax(allPoints.map((p) => p.v));
    return max * 1.1 || 1;
  }, [allPoints]);

  const innerW = Math.max(width - LANE_PAD.left - LANE_PAD.right, 1);
  const innerH = LANE_HEIGHT - LANE_PAD.top - LANE_PAD.bottom;

  const toSvgX = useCallback(
    (ts: number) => LANE_PAD.left + ((ts - xMin) / xRange) * innerW,
    [xMin, xRange, innerW],
  );
  const toSvgY = useCallback(
    (v: number) => LANE_PAD.top + innerH - (v / yMax) * innerH,
    [yMax, innerH],
  );

  // Build SVG paths for each series
  const paths = useMemo(() => {
    return laneDef.series
      .filter((s) => (series[s.key]?.length ?? 0) > 0)
      .map((s) => {
        const pts = [...(series[s.key] ?? [])].sort((a, b) => a.ts_unix_ms - b.ts_unix_ms);
        if (s.area) {
          // render as vertical tick bars (cumulative count → bar-like)
          const rects = pts.map((p) => {
            const cx = toSvgX(p.ts_unix_ms);
            const barH = (p.v / yMax) * innerH;
            return `M${cx.toFixed(1)},${(LANE_PAD.top + innerH).toFixed(1)} L${cx.toFixed(1)},${(LANE_PAD.top + innerH - barH).toFixed(1)}`;
          });
          return { key: s.key, color: s.color, label: s.label, d: rects.join(" "), isArea: true };
        }
        const d = pts
          .map(
            (p, i) =>
              `${i === 0 ? "M" : "L"}${toSvgX(p.ts_unix_ms).toFixed(1)},${toSvgY(p.v).toFixed(1)}`,
          )
          .join(" ");
        return { key: s.key, color: s.color, label: s.label, d, isArea: false };
      });
  }, [laneDef.series, series, toSvgX, toSvgY, yMax, innerH]);

  // Build event marker x-positions
  const relevantEvents = useMemo(
    () =>
      events.filter((e) => e.ts_unix_ms >= xMin && e.ts_unix_ms <= xMax),
    [events, xMin, xMax],
  );

  // Build highlight bands from derived_windows
  const bands = useMemo(() => {
    if (!laneDef.highlightKey) return [];
    const key = laneDef.highlightKey;
    if (key === "hitches") {
      return (derivedWindows.hitches ?? []).map((w) => ({
        x1: toSvgX(windowFromMs + w.from_ms),
        x2: toSvgX(windowFromMs + w.to_ms),
      }));
    }
    if (key === "encoder_saturation") {
      return (derivedWindows.encoder_saturation ?? []).map((w) => ({
        x1: toSvgX(windowFromMs + w.from_ms),
        x2: toSvgX(windowFromMs + w.to_ms),
      }));
    }
    if (key === "likely_network_congestion") {
      return (derivedWindows.likely_network_congestion ?? []).map((w) => ({
        x1: toSvgX(windowFromMs + w.from_ms),
        x2: toSvgX(windowFromMs + w.to_ms),
      }));
    }
    return [];
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

      // Find nearby events (within 2% of time range)
      const threshold = xRange * 0.02;
      const nearEvents = events.filter(
        (ev) => Math.abs(ev.ts_unix_ms - tsAtX) < threshold,
      );

      onHover({
        x: e.clientX,
        y: e.clientY,
        laneLabel: laneDef.label,
        values,
        nearEvents,
      });
    },
    [xMin, xRange, innerW, laneDef, series, events, onHover],
  );

  const handleMouseLeave = useCallback(() => onHover(null), [onHover]);

  return (
    <svg
      ref={svgRef}
      viewBox={`0 0 ${width} ${LANE_HEIGHT}`}
      width={width}
      height={LANE_HEIGHT}
      style={{ display: "block", overflow: "visible" }}
      onMouseMove={handleMouseMove}
      onMouseLeave={handleMouseLeave}
      aria-hidden="true"
    >
      {/* baseline */}
      <line
        x1={LANE_PAD.left}
        y1={LANE_PAD.top + innerH}
        x2={width - LANE_PAD.right}
        y2={LANE_PAD.top + innerH}
        stroke="var(--line)"
        strokeWidth={0.5}
      />

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

      {/* event markers */}
      {relevantEvents.map((ev, i) => {
        const x = toSvgX(ev.ts_unix_ms);
        return (
          <line
            key={i}
            x1={x}
            y1={LANE_PAD.top}
            x2={x}
            y2={LANE_PAD.top + innerH}
            stroke={eventColor(ev.type)}
            strokeWidth={1}
            strokeDasharray="3 2"
            opacity={0.85}
          />
        );
      })}

      {/* series paths */}
      {paths.map((p) =>
        p.isArea ? (
          <path
            key={p.key}
            d={p.d}
            fill="none"
            stroke={p.color}
            strokeWidth={2}
            strokeLinecap="round"
            opacity={0.7}
          />
        ) : (
          <path
            key={p.key}
            d={p.d}
            fill="none"
            stroke={p.color}
            strokeWidth={1.5}
            strokeLinejoin="round"
            strokeLinecap="round"
          />
        ),
      )}
    </svg>
  );
});

// ── XAxis ──────────────────────────────────────────────────────────────────────

interface XAxisProps {
  xMin: number;
  xMax: number;
  width: number;
}

const XAxis = memo(function XAxis({ xMin, xMax, width }: XAxisProps) {
  const ticks = useMemo(() => {
    const range = xMax - xMin;
    const innerW = width - LANE_PAD.left - LANE_PAD.right;
    const count = Math.max(2, Math.floor(innerW / 80));
    return Array.from({ length: count + 1 }, (_, i) => {
      const ts = xMin + (range / count) * i;
      const x = LANE_PAD.left + (i / count) * innerW;
      const relSec = Math.round((ts - xMin) / 1000);
      return { x, label: `+${relSec}s` };
    });
  }, [xMin, xMax, width]);

  return (
    <svg
      viewBox={`0 0 ${width} 16`}
      width={width}
      height={16}
      style={{ display: "block", overflow: "visible" }}
      aria-hidden="true"
    >
      {ticks.map((t) => (
        <text key={t.x} x={t.x} y={12} textAnchor="middle" fill="var(--text-4)" fontSize={9}>
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
      <span className="trace-clock-badge trace-clock-unmeasured">
        clock unmeasured
      </span>
    );
  }
  return (
    <span className="trace-clock-badge">
      clock ±{clock.uncertainty_ms.toFixed(1)}ms
    </span>
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
  const types = useMemo(
    () => [...new Set(events.map((e) => e.type))].filter((t) => t in EVENT_COLORS),
    [events],
  );

  if (types.length === 0) return null;

  return (
    <div className="trace-legend">
      {types.map((t) => (
        <span key={t} className="trace-legend-item">
          <svg width={12} height={10}>
            <line
              x1={0} y1={5} x2={12} y2={5}
              stroke={eventColor(t)}
              strokeWidth={1.5}
              strokeDasharray="3 2"
            />
          </svg>
          <span>{t}</span>
        </span>
      ))}
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

  const [containerRef, containerWidth] = useContainerWidth(600);

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

  // Label column width (fixed 80px); lanes take the rest
  const LABEL_W = 80;
  const svgWidth = Math.max(containerWidth - LABEL_W, 100);

  if (loading) {
    return <p className="muted">Loading trace…</p>;
  }

  if (loadError) {
    return <p className="form-error">{loadError}</p>;
  }

  if (!bundle) return null;

  const hasData = Object.values(bundle.series).some((pts) => (pts?.length ?? 0) > 0);

  if (!hasData && bundle.events.length === 0) {
    return <p className="muted">No trace data for this session yet.</p>;
  }

  return (
    <div className="trace-viewer" ref={containerRef}>
      {/* Top row: clock badge + live indicator + refresh */}
      <div className="trace-clock-row">
        <ClockBadge clock={bundle.clock} />
        {isLive && (
          <span className="trace-live-badge" title={`auto-updating every ${POLL_INTERVAL_MS / 1000}s`}>
            <span className="trace-live-dot" />
            live
          </span>
        )}
        <button className="btn btn-sm" onClick={() => void load()}>
          Refresh
        </button>
      </div>

      {/* Classifier verdict */}
      <VerdictPanel classifier={bundle.classifier} />

      {/* Stacked lanes */}
      <div className="trace-lanes">
        {LANES.map((lane) => (
          <div key={lane.label} className="trace-lane">
            <div className="trace-lane-label">{lane.label}</div>
            <div className="trace-lane-svg">
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
          </div>
        ))}

        {/* Shared x-axis */}
        <div className="trace-lane">
          <div className="trace-lane-label" />
          <div className="trace-lane-svg">
            <XAxis xMin={xMin} xMax={xMax} width={svgWidth} />
          </div>
        </div>
      </div>

      <TraceLegend events={bundle.events} />

      {/* Hover tooltip */}
      {tooltip && <Tooltip tooltip={tooltip} />}
    </div>
  );
}
