// Raw → W3C Standard Gamepad for pads the browser leaves at `mapping: ""` (#831).
// The wire format (protocol/input.md) is untouched: this runs in the browser
// before the state is sent, like the Nintendo layout switch.
//
// The order a browser exposes for an unmapped pad depends on the pad, the OS and
// the browser, so an entry is keyed by vendor:product AND platform AND browser
// family, and carries the exact button and axis counts that capture reported.
// Anything that does not match exactly is left alone and keeps the "not
// recognised" notice. Add an entry only from a real capture (docs/troubleshooting
// "Controller buttons are in the wrong places"); a guess is worse than the notice.

/** SDL notation key → W3C Standard Gamepad button index. */
const BUTTON_KEYS: Record<string, number> = {
  a: 0, b: 1, x: 2, y: 3, leftshoulder: 4, rightshoulder: 5, lefttrigger: 6,
  righttrigger: 7, back: 8, start: 9, leftstick: 10, rightstick: 11,
  dpup: 12, dpdown: 13, dpleft: 14, dpright: 15, guide: 16,
};
const AXIS_KEYS: Record<string, number> = { leftx: 0, lefty: 1, rightx: 2, righty: 3 };

export interface PadEnv {
  platform: "win" | "mac" | "linux" | "other";
  browser: "chrome" | "firefox" | "other";
}

export interface PadEntry extends PadEnv {
  /** Lowercase hex, as in `pad.id`. */
  vid: string;
  pid: string;
  /** Where the capture came from (pad, mode, OS, browser, date). */
  source: string;
  /** Exact lengths of `pad.buttons` and `pad.axes` in that capture. */
  buttons: number;
  axes: number;
  /** Raw index per W3C control, in SDL's mapping notation: `a:b1,leftx:a0,dpup:h0.1,lefttrigger:a5`.
   *  `bN` button, `aN` axis (a trigger axis, see `triggerRest`), `h0.M` hat direction M
   *  (1 up, 2 right, 4 down, 8 left) on `hatAxis`. A d-pad that arrives as buttons is `dpup:bN`. */
  map: string;
  /** Raw axis that carries a HID hat switch; required when `map` uses `h0.M`. */
  hatAxis?: number;
  /** Resting value of a trigger axis (default -1: rest -1, full +1). */
  triggerRest?: number;
}

/** Entries backed by a real capture. */
export const PADS: PadEntry[] = [];

type Src = { kind: "b" | "a" | "h"; idx: number; hat?: number };
interface Layout {
  buttons: Array<Src | undefined>; // by W3C button index
  axes: Array<number | undefined>; // W3C axis → raw axis
}

/** Chrome: "… (Vendor: 2dc8 Product: 6006)"; Firefox: "2dc8-6006-…". */
export function parsePadId(id: string): string | null {
  const m = /Vendor:\s*([0-9a-f]{4})\s+Product:\s*([0-9a-f]{4})/i.exec(id) ?? /^([0-9a-f]{4})-([0-9a-f]{4})-/i.exec(id);
  return m ? `${m[1]}:${m[2]}`.toLowerCase() : null;
}

/** From `navigator.userAgentData?.platform ?? navigator.platform` and the user agent. */
export function detectPadEnv(
  nav: { platform?: string; userAgent?: string; userAgentData?: { platform?: string } } = navigator,
): PadEnv {
  const p = (nav.userAgentData?.platform ?? nav.platform ?? "").toLowerCase();
  const ua = nav.userAgent ?? "";
  return {
    platform: p.startsWith("win") ? "win" : p.startsWith("mac") ? "mac" : p.startsWith("linux") ? "linux" : "other",
    browser: /Firefox\//.test(ua) ? "firefox" : /Chrome\/|Chromium\//.test(ua) ? "chrome" : "other",
  };
}

export function parseEntry(e: PadEntry): Layout {
  const buttons: Layout["buttons"] = [];
  const axes: Layout["axes"] = [];
  for (const part of e.map.split(",")) {
    const [key, spec] = part.split(":");
    const m = /^([bah])(\d+)(?:\.(\d+))?$/.exec(spec ?? "");
    if (!m) throw new Error(`gamepadRemap: bad entry ${e.vid}:${e.pid} ${part}`);
    const kind = m[1] as Src["kind"];
    const idx = Number(m[2]);
    const limit = kind === "b" ? e.buttons : e.axes;
    if (kind === "h" && e.hatAxis === undefined) throw new Error(`gamepadRemap: ${e.vid}:${e.pid} uses a hat but has no hatAxis`);
    if (kind !== "h" && idx >= limit) throw new Error(`gamepadRemap: ${e.vid}:${e.pid} ${part} is outside its capture`);
    if (key in AXIS_KEYS) axes[AXIS_KEYS[key]] = idx;
    else if (key in BUTTON_KEYS) buttons[BUTTON_KEYS[key]] = kind === "h" ? { kind, idx: e.hatAxis!, hat: Number(m[3]) } : { kind, idx };
  }
  if (e.hatAxis !== undefined && e.hatAxis >= e.axes) throw new Error(`gamepadRemap: ${e.vid}:${e.pid} hatAxis is outside its capture`);
  return { buttons, axes };
}

const layouts = new WeakMap<PadEntry, Layout>();

/** Hat switch → bitmask (1 up, 2 right, 4 down, 8 left). The axis steps in 2/7
 *  from -1 (up) clockwise through 1 (up-left); neutral (1.286 or 3.286) is off
 *  the grid. Anything not within HAT_TOLERANCE of a step is neutral, never a press. */
const HAT_TOLERANCE = 0.02;
export function hatDirection(v: number): number {
  const k = Math.round((v + 1) * 3.5);
  if (k < 0 || k > 7 || Math.abs(v + 1 - (k * 2) / 7) > HAT_TOLERANCE) return 0;
  return [1, 3, 2, 6, 4, 12, 8, 9][k];
}

export interface Remapped {
  buttons: number[];
  axes: number[];
}

/** Translate a raw pad into the 17-button / 4-axis W3C layout, or null when no
 *  entry matches this id on this platform and browser with exactly these array
 *  lengths. */
export function remapGamepad(
  id: string,
  rawButtons: number[],
  rawAxes: number[],
  env: PadEnv,
  table: PadEntry[] = PADS,
): Remapped | null {
  const key = parsePadId(id);
  const e = key && table.find(
    (t) => `${t.vid}:${t.pid}` === key && t.platform === env.platform && t.browser === env.browser,
  );
  if (!e || rawButtons.length !== e.buttons || rawAxes.length !== e.axes) return null;
  let l = layouts.get(e);
  if (!l) layouts.set(e, (l = parseEntry(e)));
  const rest = e.triggerRest ?? -1;
  const buttons = Array.from({ length: 17 }, (_, i) => {
    const s = l!.buttons[i];
    if (!s) return 0;
    if (s.kind === "b") return rawButtons[s.idx];
    if (s.kind === "a") return Math.min(1, Math.max(0, (rawAxes[s.idx] - rest) / (1 - rest)));
    return hatDirection(rawAxes[s.idx]) & (s.hat ?? 0) ? 1 : 0;
  });
  const axes = [0, 1, 2, 3].map((i) => (l!.axes[i] === undefined ? 0 : rawAxes[l!.axes[i]!]));
  return { buttons, axes };
}
