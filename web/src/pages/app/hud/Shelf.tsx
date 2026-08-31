// The shelf: the HUD's inner face, one section at a time.
//
// It animates its depth only (height on a horizontal dock, width on a vertical
// one), which is what makes switching tabs never resize the bar — the single
// most important property of the morph (handoff §E "Shelf").
//
// Only the active pane is rendered. The mock keeps all three in the DOM and
// toggles `.active`; rendering one keeps the accessibility tree honest (a
// hidden pane's controls stayed Tab-reachable in the v1 drawer, which is the
// bug this shape prevents) and lets each pane's telemetry subscription end
// when it leaves the screen.

import type { ReactNode } from "react";
import type { HudTab } from "./hudKeys";

export interface ShelfProps {
  tab: HudTab;
  open: boolean;
  children: ReactNode;
  shelfRef: React.Ref<HTMLDivElement>;
}

export function Shelf({ tab, open, children, shelfRef }: ShelfProps) {
  return (
    <div className="shelf" ref={shelfRef} aria-hidden={!open}>
      <div className="shelf-in">
        <div className="pane active" data-pane={tab} role="group" aria-label="Session menu">
          {children}
        </div>
      </div>
    </div>
  );
}
