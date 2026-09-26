// SettingRow — one Settings-page row: label + hint on the left, a control on
// the right, separated by a rule (handoff-v3-spec.md §A.21's `row()` helper).

import type { ReactNode } from "react";

interface SettingRowProps {
  label: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
}

export function SettingRow({ label, hint, children }: SettingRowProps) {
  return (
    <div className="rowflex between gap6 setting-row">
      <div>
        <div className="label">{label}</div>
        {hint && <div className="hint">{hint}</div>}
      </div>
      <div className="setting-row-control">{children}</div>
    </div>
  );
}
