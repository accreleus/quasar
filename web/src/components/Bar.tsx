/**
 * Bar + Gauge primitives (UI-04).
 * Bar: horizontal capacity bar.
 * Gauge: conic-gradient circular gauge.
 */
import type { ReactNode } from "react";

// ── Bar ────────────────────────────────────────────────────

type BarVariant = "default" | "success" | "warning" | "danger" | "info" | "grad";

interface BarProps {
  /** 0–100. Ignored when `unknown` is set. */
  percent: number;
  label?: ReactNode;
  /** The figure beside the bar. `null` renders the console's no-value glyph;
   *  omit the prop entirely for a bar that carries no figure at all. Callers
   *  must not spell the glyph themselves — "n/a" and "—" both appeared. */
  value?: ReactNode | null;
  variant?: BarVariant;
  /**
   * No-data state (#383): the metric has never been sampled, or the sample is
   * stale/implausible. Renders no fill at all — never a 0%-filled bar, which
   * would read as "confirmed empty" rather than "unknown". Pair with a `hint`
   * (native title tooltip, matching the design handoff's tooltip convention)
   * explaining why, and pass `value="—"` (the handoff's convention for an
   * unavailable metric, e.g. admin-hosts.html's offline "Sessions" cell) —
   * `value={null}` renders it.
   */
  unknown?: boolean;
  /** Native title/tooltip text shown on hover over the row. */
  hint?: string;
}

export function Bar({ percent, label, value, variant = "default", unknown = false, hint }: BarProps) {
  const pct = Math.max(0, Math.min(100, percent));
  const cls = ["bar", variant !== "default" ? variant : "", unknown ? "unknown" : ""]
    .filter(Boolean)
    .join(" ");
  const rowCls = unknown ? "bar-row unknown" : "bar-row";
  return (
    <div className={rowCls} title={hint}>
      {label !== undefined && <span className="lbl">{label}</span>}
      <span className={cls}>{!unknown && <span style={{ width: `${pct}%` }} />}</span>
      {value !== undefined && <span className="val">{value === null ? "—" : value}</span>}
    </div>
  );
}

// ── Gauge ──────────────────────────────────────────────────

interface GaugeProps {
  /** 0–100 */
  percent: number;
  label?: string;
  /** Optional CSS color override for the arc; defaults to var(--accent) */
  color?: string;
}

export function Gauge({ percent, label, color }: GaugeProps) {
  const pct = Math.max(0, Math.min(100, percent));
  const arcColor = color ?? "var(--accent)";
  return (
    <div
      className="gauge"
      style={{
        background: `conic-gradient(from -90deg, ${arcColor} ${pct * 3.6}deg, var(--ink-5) 0)`,
      }}
      role="meter"
      aria-valuenow={pct}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div className="g-val">
        <b>{pct}%</b>
        {label && <span>{label}</span>}
      </div>
    </div>
  );
}
