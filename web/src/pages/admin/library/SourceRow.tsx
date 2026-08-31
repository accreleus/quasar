// One row of the Sources "Content sources" / "Artwork providers" card
// (handoff-v3-spec.md §A.15's `src()` template): name, description, an
// optional meta line and extra content, trailing actions and a switch. A
// second provider is one more call site of this component, not new markup.

import type { ReactNode } from "react";
import { Switch } from "../../../components/TextField";

export interface SourceRowProps {
  name: string;
  /** Trailing badge beside the name (the SteamGridDB configured/not-configured chip). */
  badge?: ReactNode;
  description: string;
  /** "{n} titles discovered · {n} imported · last scan {relative}" and similar. */
  meta?: ReactNode;
  /** Ghost/secondary buttons before the trailing switch. */
  actions?: ReactNode;
  switchChecked: boolean;
  onSwitchChange?: (checked: boolean) => void;
  switchDisabled?: boolean;
  /** Accessible name for the trailing switch, which carries no visible label. */
  switchLabel: string;
  /** Explains a disabled switch that has no enable action of its own. */
  switchTitle?: string;
  /** Anything below the description/meta: a `.note.warn`, a field, a collapsible block. */
  children?: ReactNode;
  /** Omit the bottom rule on the last row of a card. */
  last?: boolean;
}

export function SourceRow({
  name,
  badge,
  description,
  meta,
  actions,
  switchChecked,
  onSwitchChange,
  switchDisabled,
  switchLabel,
  switchTitle,
  children,
  last,
}: SourceRowProps) {
  return (
    <div
      className="row"
      style={{
        display: "flex",
        gap: "var(--s5)",
        alignItems: "flex-start",
        padding: "var(--card-pad)",
        borderBottom: last ? undefined : "1px solid var(--line)",
      }}
    >
      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="rowflex">
          <span className="panel-title">{name}</span>
          {badge}
        </div>
        <div className="hint" style={{ marginTop: 5, maxWidth: "60ch" }}>
          {description}
        </div>
        {meta && (
          <div className="muted" style={{ marginTop: 10, fontSize: "var(--t-sm)" }}>
            {meta}
          </div>
        )}
        {children}
      </div>
      <div style={{ display: "flex", alignItems: "center", gap: "var(--s2)", flex: "none" }}>
        {actions}
        <span title={switchTitle} style={{ marginLeft: "var(--s3)" }}>
          <Switch
            aria-label={switchLabel}
            checked={switchChecked}
            onChange={(v) => onSwitchChange?.(v)}
            disabled={switchDisabled || !onSwitchChange}
          />
        </span>
      </div>
    </div>
  );
}
