// Raw → W3C Standard Gamepad for pads the browser leaves at `mapping: ""`
// (#831). The wire format (protocol/input.md) is untouched: this runs in the
// browser before the state is sent, like the Nintendo layout switch.
//
// Layouts are SDL mappings trimmed to the keys we use, one per line of
// mdqinc/SDL_GameControllerDB gamecontrollerdb.txt (zlib, Copyright (C) 1997-2025
// Sam Lantinga; commit 22a98f6, Windows section, keyed by the GUID's vendor and
// product). That section describes the HID order the browsers expose for an
// unmapped pad; other OS orders are not covered, and a pad whose raw arrays are
// too short for its layout stays unmapped and keeps the notice.

/** SDL key → W3C Standard Gamepad button index. */
const BUTTON_KEYS: Record<string, number> = {
  a: 0, b: 1, x: 2, y: 3, leftshoulder: 4, rightshoulder: 5, lefttrigger: 6,
  righttrigger: 7, back: 8, start: 9, leftstick: 10, rightstick: 11,
  dpup: 12, dpdown: 13, dpleft: 14, dpright: 15, guide: 16,
};
const AXIS_KEYS: Record<string, number> = { leftx: 0, lefty: 1, rightx: 2, righty: 3 };

/** The raw axis a HID hat switch arrives on; see `hatDirection`. */
const HAT_AXIS = 9;
const HAT_MASK: Record<string, number> = { dpup: 1, dpright: 2, dpdown: 4, dpleft: 8 };

const L_8BITDO_NIN =
  "a:b1,b:b0,back:b10,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,dpup:h0.1,guide:b12,leftshoulder:b6,leftstick:b13,lefttrigger:b8,leftx:a0,lefty:a1,rightshoulder:b7,rightstick:b14,righttrigger:b9,rightx:a3,righty:a4,start:b11,x:b4,y:b3";
const L_8BITDO_XBOX =
  "a:b0,b:b1,back:b10,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,dpup:h0.1,guide:b12,leftshoulder:b6,leftstick:b13,lefttrigger:b8,leftx:a0,lefty:a1,rightshoulder:b7,rightstick:b14,righttrigger:b9,rightx:a3,righty:a4,start:b11,x:b3,y:b4";
const L_8BITDO_ULTIMATE_BT =
  "a:b0,b:b1,back:b10,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,dpup:h0.1,guide:b12,leftshoulder:b6,leftstick:b13,lefttrigger:a5,leftx:a0,lefty:a1,rightshoulder:b7,rightstick:b14,righttrigger:a4,rightx:a2,righty:a3,start:b11,x:b3,y:b4";
const L_8BITDO_SN30 =
  "a:b1,b:b0,back:b10,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,dpup:h0.1,guide:b2,leftshoulder:b6,leftstick:b13,lefttrigger:b8,leftx:a0,lefty:a1,rightshoulder:b7,rightstick:b14,righttrigger:b9,rightx:a2,righty:a3,start:b11,x:b4,y:b3";
const L_LOGITECH_DINPUT =
  "a:b1,b:b2,back:b8,dpdown:h0.4,dpleft:h0.8,dpright:h0.2,dpup:h0.1,leftshoulder:b4,leftstick:b10,lefttrigger:b6,leftx:a0,lefty:a1,rightshoulder:b5,rightstick:b11,righttrigger:b7,rightx:a2,righty:a3,start:b9,x:b0,y:b3";

/** "vendor:product" (lowercase hex) → layout. */
export const PADS: Record<string, string> = {
  "2dc8:6006": L_8BITDO_NIN, // 8BitDo Pro 2
  "2dc8:6003": L_8BITDO_NIN, // 8BitDo Pro 2
  "2dc8:6103": L_8BITDO_NIN, // 8BitDo Pro 2
  "2dc8:5112": L_8BITDO_NIN, // 8BitDo Lite 2
  "2dc8:5111": L_8BITDO_NIN, // 8BitDo Lite SE
  "2dc8:6009": L_8BITDO_NIN, // 8BitDo Pro 3
  "2dc8:3011": L_8BITDO_XBOX, // 8BitDo Ultimate
  "2dc8:3012": L_8BITDO_XBOX, // 8BitDo Ultimate 2.4G
  "2dc8:6012": L_8BITDO_XBOX, // 8BitDo Ultimate 2
  "2dc8:2101": L_8BITDO_XBOX, // 8BitDo Xbox One SN30 Pro
  "2dc8:3013": L_8BITDO_ULTIMATE_BT, // 8BitDo Ultimate Bluetooth
  "2dc8:6001": L_8BITDO_SN30, // 8BitDo SN30 Pro
  "2dc8:6101": L_8BITDO_SN30, // 8BitDo SN30 Pro
  "046d:c216": L_LOGITECH_DINPUT, // Logitech Dual Action / F310 (D mode)
  "046d:c218": L_LOGITECH_DINPUT, // Logitech F510
  "046d:c219": L_LOGITECH_DINPUT, // Logitech F710 (D mode)
};

type Src = { kind: "b" | "a" | "h"; idx: number; hat?: number };
interface Layout {
  buttons: Array<Src | undefined>; // by W3C button index
  axes: Array<number | undefined>; // W3C axis → raw axis
  need: { buttons: number; axes: number };
}

/** Chrome: "… (Vendor: 2dc8 Product: 6006)"; Firefox: "2dc8-6006-…". */
export function parsePadId(id: string): string | null {
  const m = /Vendor:\s*([0-9a-f]{4})\s+Product:\s*([0-9a-f]{4})/i.exec(id) ?? /^([0-9a-f]{4})-([0-9a-f]{4})-/i.exec(id);
  return m ? `${m[1]}:${m[2]}`.toLowerCase() : null;
}

function parseLayout(sdl: string): Layout {
  const buttons: Layout["buttons"] = [];
  const axes: Layout["axes"] = [];
  const need = { buttons: 0, axes: 0 };
  for (const part of sdl.split(",")) {
    const [key, spec] = part.split(":");
    const m = /^([bah])(\d+)(?:\.(\d+))?$/.exec(spec);
    if (!m) throw new Error(`gamepadRemap: bad SDL entry ${part}`);
    const kind = m[1] as Src["kind"];
    const idx = Number(m[2]);
    if (key in AXIS_KEYS) {
      axes[AXIS_KEYS[key]] = idx;
      need.axes = Math.max(need.axes, idx + 1);
    } else if (key in BUTTON_KEYS) {
      buttons[BUTTON_KEYS[key]] = kind === "h" ? { kind, idx: HAT_AXIS, hat: HAT_MASK[key] } : { kind, idx };
      if (kind === "h") need.axes = Math.max(need.axes, HAT_AXIS + 1);
      else if (kind === "a") need.axes = Math.max(need.axes, idx + 1);
      else need.buttons = Math.max(need.buttons, idx + 1);
    }
  }
  return { buttons, axes, need };
}

const layouts = new Map<string, Layout | null>();

/** The layout for a pad id, or undefined when the table does not know it. */
function layoutFor(id: string): Layout | undefined {
  let l = layouts.get(id);
  if (l === undefined) {
    const sdl = PADS[parsePadId(id) ?? ""];
    l = sdl ? parseLayout(sdl) : null;
    layouts.set(id, l);
  }
  return l ?? undefined;
}

/** Hat switch → bitmask (1 up, 2 right, 4 down, 8 left). The axis steps in
 *  2/7 from -1 (up) clockwise through 1 (up-left); neutral is off the grid
 *  (1.286 or 3.286 depending on the browser/OS). */
export function hatDirection(v: number): number {
  const k = Math.round((v + 1) * 3.5);
  if (k < 0 || k > 7 || Math.abs(v + 1 - (k * 2) / 7) > 0.1) return 0;
  return [1, 3, 2, 6, 4, 12, 8, 9][k];
}

export interface Remapped {
  buttons: number[];
  axes: number[];
}

/** Translate a raw pad into the 17-button / 4-axis W3C layout, or null when the
 *  table does not know the id or the raw arrays are too short for the layout
 *  (a different OS or mode than the table was written for). */
export function remapGamepad(id: string, rawButtons: number[], rawAxes: number[]): Remapped | null {
  const l = layoutFor(id);
  if (!l || rawButtons.length < l.need.buttons || rawAxes.length < l.need.axes) return null;
  const buttons = Array.from({ length: 17 }, (_, i) => {
    const s = l.buttons[i];
    if (!s) return 0;
    if (s.kind === "b") return rawButtons[s.idx];
    if (s.kind === "a") return Math.min(1, Math.max(0, (rawAxes[s.idx] + 1) / 2)); // trigger axis rests at -1
    return hatDirection(rawAxes[s.idx]) & (s.hat ?? 0) ? 1 : 0;
  });
  const axes = [0, 1, 2, 3].map((i) => (l.axes[i] === undefined ? 0 : rawAxes[l.axes[i]!]));
  return { buttons, axes };
}
