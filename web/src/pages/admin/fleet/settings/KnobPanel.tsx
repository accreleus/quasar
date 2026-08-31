// One knob-group `.card` (title + hint + optional panel action, then its
// rows) — the repeated shape of Runtime defaults / Adaptation / Encoder and
// GPU / Advanced streaming tuning. Pulled out so HostSettings.tsx states each
// panel's copy once instead of four near-identical `.panel-head` blocks.

import type { ReactNode } from "react";

export function KnobPanel({
  title,
  hint,
  actions,
  children,
}: {
  title: string;
  hint: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="card">
      <div className="panel-head">
        <div>
          <span className="panel-title">{title}</span>
          <p className="hint" style={{ marginTop: 3 }}>{hint}</p>
        </div>
        {actions && <div className="acts">{actions}</div>}
      </div>
      {children}
    </div>
  );
}
