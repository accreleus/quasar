// Relative mouse speed: "Match cursor to my window" and sensitivity
// (control-api.md §Region and Input settings, "What the client applies").
//
// The wire is untouched (protocol/input.md `mm`): this only changes the numbers
// put into dx/dy. The gain is worked out when the window, stream or setting
// changes; the per-event path is one multiply-add per axis and allocates nothing.

import type { RegionInputWire } from "../api/me";
import type { AppKind } from "../api/types";

export interface MouseSettings {
  /** `match_cursor` resolved for this app: scale by stream size ÷ shown size. */
  match: boolean;
  /** `mouse_sensitivity`, 0.25 to 4. */
  sensitivity: number;
  /** `raw_mouse`: ask for pointer lock with `unadjustedMovement`. */
  raw: boolean;
}

export const MOUSE_SENSITIVITY_MIN = 0.25;
export const MOUSE_SENSITIVITY_MAX = 4;

export const NEUTRAL_MOUSE: MouseSettings = { match: false, sensitivity: 1, raw: false };

/** The user's value, then the install default, then the built-in (`auto`, 1×, raw off);
 *  `auto` is on for a desktop app and off for every other kind. */
export function resolveMouseSettings(
  s: RegionInputWire | null | undefined,
  kind: AppKind | undefined,
): MouseSettings {
  const mode = s?.input?.match_cursor ?? s?.defaults?.input?.match_cursor ?? "auto";
  const sens = s?.input?.mouse_sensitivity ?? s?.defaults?.input?.mouse_sensitivity ?? 1;
  return {
    match: mode === "on" || (mode === "auto" && kind === "desktop"),
    sensitivity: Math.min(MOUSE_SENSITIVITY_MAX, Math.max(MOUSE_SENSITIVITY_MIN, sens)),
    raw: s?.input?.raw_mouse ?? s?.defaults?.input?.raw_mouse ?? false,
  };
}

export interface ShownGeometry {
  /** Stream size in pixels (`video.videoWidth/Height`). */
  streamW: number;
  streamH: number;
  /** The video element's box in CSS pixels. */
  boxW: number;
  boxH: number;
  /** The session view's scaling mode (`data-scaling-mode`): contain, cover or stretch; `integer` draws as contain. */
  fit?: string;
}

/** Stream pixels per shown pixel on each axis; `[1, 1]` when the geometry is not known yet. */
export function streamPerShown(g: ShownGeometry): [number, number] {
  const { streamW, streamH, boxW, boxH } = g;
  if (!(streamW > 0 && streamH > 0 && boxW > 0 && boxH > 0)) return [1, 1];
  const sx = boxW / streamW;
  const sy = boxH / streamH;
  if (g.fit === "stretch") return [1 / sx, 1 / sy];
  const s = g.fit === "cover" ? Math.max(sx, sy) : Math.min(sx, sy);
  return [1 / s, 1 / s];
}

/** The per-axis multiplier for one mouse event. Exactly `[1, 1]` for a window equal to the stream at 1×. */
export function mouseGain(settings: MouseSettings, g: ShownGeometry): [number, number] {
  const [px, py] = settings.match ? streamPerShown(g) : [1, 1];
  return [px * settings.sensitivity, py * settings.sensitivity];
}

/**
 * Integer deltas from scaled motion, carrying the fractional remainder so slow
 * movement is never lost to rounding. At gain 1 the delta passes through
 * unchanged, so a default setup sends what it always sent.
 */
export function createMouseScaler() {
  let gx = 1;
  let gy = 1;
  let rx = 0;
  let ry = 0;
  return {
    setGain(x: number, y: number) {
      gx = x;
      gy = y;
      rx = 0;
      ry = 0;
    },
    /** Writes the scaled delta into `out`; false when it is zero (nothing to send). */
    apply(out: { dx?: unknown; dy?: unknown }, dx: number, dy: number): boolean {
      if (gx === 1 && gy === 1) {
        out.dx = dx;
        out.dy = dy;
        return dx !== 0 || dy !== 0;
      }
      const ax = dx * gx + rx;
      const ay = dy * gy + ry;
      const ox = Math.round(ax);
      const oy = Math.round(ay);
      rx = ax - ox;
      ry = ay - oy;
      out.dx = ox;
      out.dy = oy;
      return ox !== 0 || oy !== 0;
    },
  };
}
