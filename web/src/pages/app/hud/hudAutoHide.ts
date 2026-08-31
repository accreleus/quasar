// Whether the HUD is on screen right now (handoff §E "Auto-hide").
//
// Five inputs, one answer, no timers: the component owns the idle clock and
// asks this function; the rule itself is a pure decision so "it hid while I
// was reading the shelf" is a unit test, not a bug report.
//
// Ordering is the whole rule. An open shelf or an in-progress swap pins the
// HUD under every preference — including "never show", because the user just
// asked for it and a swap has nowhere else to report itself.

import type { StripAutoHide } from "../../../settings/overlayPreferences";

/** Idle grace before the HUD hides while input is captured. */
export const HUD_IDLE_HIDE_MS = 4000;

export interface HudVisibilityInput {
  mode: StripAutoHide;
  /** Capture engaged — not Pointer Lock (a browser without it still captures). */
  captured: boolean;
  /** Shelf expanded. */
  open: boolean;
  /** A quick-switch swap is in flight. */
  swapping: boolean;
  /** Milliseconds since the last pointer / keyboard / gamepad activity. */
  idleMs: number;
}

export function hudVisible({
  mode,
  captured,
  open,
  swapping,
  idleMs,
}: HudVisibilityInput): boolean {
  // A summoned shelf and a swap in flight both pin it, under every preference.
  if (open || swapping) return true;
  if (mode === "never_visible") return false;
  if (mode === "always_visible") return true;
  // on_capture: only play hides it, and only after the grace period.
  return !(captured && idleMs > HUD_IDLE_HIDE_MS);
}
