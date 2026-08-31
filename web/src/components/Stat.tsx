/**
 * Stat / StatGrid — telemetry stat tiles.
 * Matches the `.stat` / `.stat-grid` pattern from the design system.
 *
 * CSS structure:
 *   .stat > .k (label eyebrow) + .v (large value) [+ .meta (sub-label)]
 */
import type { ReactNode } from "react";

interface StatProps {
  /** Eyebrow label rendered in `.k` */
  label: string;
  /** Large value rendered in `.v` */
  value: ReactNode;
  /** Optional small unit or sub-label rendered after value as <small> */
  unit?: string;
  /** Optional meta line below the value */
  meta?: string;
}

export function Stat({ label, value, unit, meta }: StatProps) {
  return (
    <div className="stat">
      <div className="k">{label}</div>
      <div className="v">
        {value}
        {unit && <small>{unit}</small>}
      </div>
      {meta && <div className="meta">{meta}</div>}
    </div>
  );
}

interface StatGridProps {
  children: ReactNode;
  /** Override number of columns (default: auto-fit minmax(200px,1fr)) */
  columns?: number;
}

export function StatGrid({ children, columns }: StatGridProps) {
  return (
    <div
      className="stat-grid"
      style={columns ? { gridTemplateColumns: `repeat(${columns}, 1fr)` } : undefined}
    >
      {children}
    </div>
  );
}
