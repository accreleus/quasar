// Where the HUD sits, and what its shell looks like there (handoff §E). Every
// per-dock geometry decision is one pure function, so the component applies an
// answer rather than carrying a dozen `body[data-pos=…]` rules that can drift.

import type { StripPosition } from "../../../settings/overlayPreferences";

/** The four docks. Alias of the preference type — one vocabulary, not two. */
export type Dock = StripPosition;

/** Layout axis: horizontal docks (top/bottom) lay out as a row, vertical ones
 *  (left/right) as a column. Drives the `data-axis` attribute the CSS keys off. */
export type DockAxis = "h" | "v";

export interface DockLayout {
  pos: Dock;
  axis: DockAxis;
  open: boolean;
  /** `.hud` flex direction — bar first, then shelf on the inward face. */
  direction: "column" | "row" | "row-reverse";
  /** Inline width/height. Only the docked axis is driven; the other stays
   *  `auto`, so the pill is content-sized and the morph is single-axis. */
  width: string;
  height: string;
  radius: string;
  /** Chevron rotation (deg): at rest it points at the shelf that would open;
   *  open, it points back at the edge it would collapse to. */
  chevron: number;
}

const AXIS: Record<Dock, DockAxis> = { bottom: "h", top: "h", left: "v", right: "v" };

const DIRECTION: Record<Dock, DockLayout["direction"]> = {
  bottom: "column",
  top: "column",
  left: "row",
  right: "row-reverse",
};

/** Rest radius per dock: the two inward corners only, at the icon buttons' own
 *  radius, so shell and contents share one corner language. Open, the HUD meets
 *  every edge and has none. */
const REST_RADIUS: Record<Dock, string> = {
  bottom: "12px 12px 0 0",
  top: "0 0 12px 12px",
  left: "0 12px 12px 0",
  right: "12px 0 0 12px",
};

const CHEVRON_REST: Record<Dock, number> = { bottom: 0, top: 180, left: 90, right: -90 };
const CHEVRON_OPEN: Record<Dock, number> = { bottom: 180, top: 0, left: 270, right: 90 };

/** The shell geometry for one dock in one state. `--hud-w`/`--hud-h` are written
 *  by the component's `measure()` from the collapsed bar; the `auto` fallback
 *  keeps the pill correct before the first measurement lands. */
export function dockLayout(pos: Dock, open: boolean): DockLayout {
  const axis = AXIS[pos];
  const horizontal = axis === "h";
  return {
    pos,
    axis,
    open,
    direction: DIRECTION[pos],
    width: horizontal ? (open ? "100vw" : "var(--hud-w, auto)") : "auto",
    height: horizontal ? "auto" : open ? "100vh" : "var(--hud-h, auto)",
    radius: open ? "0" : REST_RADIUS[pos],
    chevron: open ? CHEVRON_OPEN[pos] : CHEVRON_REST[pos],
  };
}

/** The pill size to pin from one measurement of the collapsed bar: its content
 *  box plus the shell's two 1px borders, clamped so the pill can never exceed
 *  the viewport. A degenerate box (bar hidden, or read before layout) pins
 *  nothing — `var(--hud-w, auto)` keeps the pill intrinsic until a real
 *  measurement lands. */
export function hudShellPin(box: {
  width: number;
  height: number;
}): { w: string; h: string } | null {
  if (!(box.width > 0) || !(box.height > 0)) return null;
  return {
    w: `min(${Math.ceil(box.width) + 2}px, calc(100vw - 48px))`,
    h: `min(${Math.ceil(box.height) + 2}px, calc(100vh - 48px))`,
  };
}
