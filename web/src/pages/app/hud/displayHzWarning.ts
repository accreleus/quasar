// #85 — when (and whether) the Performance stats pane may claim the local
// display is costing the user frames.
//
// The defect this replaces: the pill was derived from `tierFps > displayHz`
// alone and rendered as the flat assertion "N Hz display · frames dropped",
// on a session whose own `drops / freezes` readout said `0 / 0`. Three things
// were wrong with that — it never consulted the dropped-frame telemetry it
// was making a claim about, it compared two numbers exactly when the
// right-hand one is a rounded estimate, and it fired on a single sample.
//
// So the rule here is: a display-cadence warning requires BOTH a refresh rate
// meaningfully below the stream's frame rate AND frames actually being
// dropped, sustained over more than one telemetry window. Either alone is not
// evidence of anything the user should act on.

/** Streamed fps parsed out of a tier string ("2560×1440@120" → 120). */
export function parseTierFps(tier: string | undefined | null): number | null {
  if (!tier) return null;
  const at = tier.split("@")[1];
  if (at == null) return null;
  const fps = parseInt(at, 10);
  return Number.isFinite(fps) && fps > 0 ? fps : null;
}

/**
 * How far below the tier fps the measured refresh must sit before the gap is
 * treated as real. `estimateDisplayRefreshHz` rounds `1000 / median-interval`,
 * so a true 120 Hz panel routinely measures 119 and a true 60 Hz panel 61 —
 * an exact `<` comparison turns that rounding into a warning about itself.
 */
export const DISPLAY_HZ_TOLERANCE = 5;

/**
 * Consecutive telemetry windows that must report dropped frames. One window is
 * a decoder hiccup, a tab switch, or a single late frame at startup; the pill
 * describes a standing property of the display, so it needs a standing signal.
 */
export const DROPPED_WINDOWS_REQUIRED = 3;

export interface DisplayHzInputs {
  /** Streamed fps (from the tier). Null before the tier is known. */
  streamFps: number | null;
  /** rAF-measured local refresh. Null until the first measurement lands. */
  displayHz: number | null;
  /** Consecutive recent telemetry windows in which framesDropped was non-zero. */
  droppedWindows: number;
}

export interface DisplayHzWarning {
  displayHz: number;
  streamFps: number;
}

/**
 * The warning to show, or null for "nothing worth saying".
 *
 * Null when the display keeps up, when the gap is inside the estimator's own
 * rounding noise, or when nothing is actually being dropped — that last case
 * being the one the operator hit: 119 of 120 fps received, zero drops, and a
 * pill insisting otherwise.
 */
export function displayHzWarning({
  streamFps,
  displayHz,
  droppedWindows,
}: DisplayHzInputs): DisplayHzWarning | null {
  if (streamFps == null || displayHz == null) return null;
  if (displayHz + DISPLAY_HZ_TOLERANCE >= streamFps) return null;
  if (droppedWindows < DROPPED_WINDOWS_REQUIRED) return null;
  return { displayHz, streamFps };
}
