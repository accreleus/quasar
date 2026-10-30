import { describe, expect, it } from "vitest";
import { hatDirection, PADS, parsePadId, remapGamepad } from "./gamepadRemap";

const NEUTRAL_HAT = 3.286;
const chrome = (vid: string, pid: string) => `Some Pad (Vendor: ${vid} Product: ${pid})`;

/** Raw state with `pressed` buttons down, `axes` overrides, and the hat axis at `hat`. */
function raw(pressed: number[] = [], axes: Record<number, number> = {}, hat = NEUTRAL_HAT) {
  const buttons = Array.from({ length: 15 }, (_, i) => (pressed.includes(i) ? 1 : 0));
  const a = Array.from({ length: 10 }, () => 0);
  a[9] = hat;
  for (const [i, v] of Object.entries(axes)) a[Number(i)] = v;
  return { buttons, axes: a };
}
const down = (id: string, pressed: number[], axes = {}, hat = NEUTRAL_HAT) => {
  const r = raw(pressed, axes, hat);
  const out = remapGamepad(id, r.buttons, r.axes)!;
  return out.buttons.flatMap((v, i) => (v ? [i] : []));
};

describe("parsePadId", () => {
  it("reads the Chrome form", () => expect(parsePadId("8BitDo Pro 2 (Vendor: 2DC8 Product: 6006)")).toBe("2dc8:6006"));
  it("reads the Firefox form", () => expect(parsePadId("2dc8-6006-8BitDo Pro 2")).toBe("2dc8:6006"));
  it("is null for an id without vendor/product", () => expect(parsePadId("Generic USB Gamepad")).toBeNull());
});

describe("hatDirection", () => {
  it("decodes the eight directions clockwise from up as a bitmask", () => {
    const steps = [-1, -0.714, -0.429, -0.143, 0.143, 0.429, 0.714, 1];
    expect(steps.map(hatDirection)).toEqual([1, 3, 2, 6, 4, 12, 8, 9]);
  });
  it("treats either browser's neutral value as released", () => {
    expect(hatDirection(3.286)).toBe(0);
    expect(hatDirection(1.286)).toBe(0);
  });
});

describe("remapGamepad", () => {
  const NIN = chrome("2dc8", "6006"); // a:b1 b:b0 x:b4 y:b3
  const XBOX = chrome("2dc8", "6012"); // a:b0 b:b1 x:b3 y:b4
  const SN30 = "2dc8-6001-8BitDo SN30 Pro"; // guide:b2, right stick a2/a3
  const LOGI = chrome("046d", "c219");

  it("is null for a pad that is not in the table", () => {
    expect(remapGamepad(chrome("1234", "5678"), raw().buttons, raw().axes)).toBeNull();
    expect(remapGamepad("Generic USB Gamepad", raw().buttons, raw().axes)).toBeNull();
  });

  it("is null when the raw arrays are too short for the layout", () => {
    expect(remapGamepad(NIN, raw().buttons.slice(0, 10), raw().axes)).toBeNull();
    expect(remapGamepad(NIN, raw().buttons, raw().axes.slice(0, 9))).toBeNull();
  });

  it("sends 17 buttons and 4 axes", () => {
    const out = remapGamepad(NIN, raw().buttons, raw().axes)!;
    expect([out.buttons.length, out.axes.length]).toEqual([17, 4]);
  });

  it("Nintendo-order 8BitDo: face buttons land by position", () => {
    // raw b1 is the bottom button, b0 the right one, b4 the left one, b3 the top one
    expect(down(NIN, [1])).toEqual([0]);
    expect(down(NIN, [0])).toEqual([1]);
    expect(down(NIN, [4])).toEqual([2]);
    expect(down(NIN, [3])).toEqual([3]);
  });

  it("Xbox-order 8BitDo: face buttons land by position", () => {
    expect(down(XBOX, [0])).toEqual([0]);
    expect(down(XBOX, [1])).toEqual([1]);
    expect(down(XBOX, [3])).toEqual([2]);
    expect(down(XBOX, [4])).toEqual([3]);
  });

  it("shoulders, triggers, back/start, sticks and guide", () => {
    // raw 6 7 8 9 10 11 13 14 12 → W3C 4 5 6 7 8 9 10 11 16
    expect(down(NIN, [6, 7, 8, 9, 10, 11, 13, 14, 12])).toEqual([4, 5, 6, 7, 8, 9, 10, 11, 16]);
  });

  it("SN30 Pro: guide on raw 2 and the right stick on raw axes 2 and 3", () => {
    expect(down(SN30, [2])).toEqual([16]);
    const r = raw([], { 2: 0.5, 3: -0.5 });
    expect(remapGamepad(SN30, r.buttons, r.axes)!.axes).toEqual([0, 0, 0.5, -0.5]);
  });

  it("Logitech D-input: its own face and shoulder order", () => {
    expect(down(LOGI, [1, 2, 0, 3])).toEqual([0, 1, 2, 3]);
    expect(down(LOGI, [4, 5, 6, 7, 8, 9, 10, 11])).toEqual([4, 5, 6, 7, 8, 9, 10, 11]);
  });

  it("maps the sticks, right stick on raw axes 3 and 4", () => {
    const r = raw([], { 0: -1, 1: 1, 3: 0.25, 4: -0.75 });
    expect(remapGamepad(NIN, r.buttons, r.axes)!.axes).toEqual([-1, 1, 0.25, -0.75]);
  });

  it("d-pad: hat directions become buttons 12-15, diagonals press two", () => {
    expect(down(NIN, [], {}, -1)).toEqual([12]); // up
    expect(down(NIN, [], {}, -0.429)).toEqual([15]); // right
    expect(down(NIN, [], {}, 0.143)).toEqual([13]); // down
    expect(down(NIN, [], {}, 0.714)).toEqual([14]); // left
    expect(down(NIN, [], {}, -0.714)).toEqual([12, 15]); // up-right
    expect(down(NIN, [], {}, 0.429)).toEqual([13, 14]); // down-left
    expect(down(NIN, [], {}, NEUTRAL_HAT)).toEqual([]);
  });

  it("Ultimate over Bluetooth: triggers on axes 5 and 4 (rest -1), sticks on 0-3", () => {
    const BT = chrome("2dc8", "3013");
    const rest = raw([], { 4: -1, 5: -1 });
    expect(remapGamepad(BT, rest.buttons, rest.axes)!.buttons.slice(6, 8)).toEqual([0, 0]);
    const pulled = raw([], { 5: 1, 4: 0, 2: 0.4, 3: -0.4 });
    const out = remapGamepad(BT, pulled.buttons, pulled.axes)!;
    expect(out.buttons.slice(6, 8)).toEqual([1, 0.5]); // left trigger a5, right a4
    expect(out.axes).toEqual([0, 0, 0.4, -0.4]);
  });

  it("every table entry resolves to a layout that fits a 15-button, 10-axis pad", () => {
    for (const key of Object.keys(PADS)) {
      const [vid, pid] = key.split(":");
      expect(remapGamepad(chrome(vid, pid), raw().buttons, raw().axes), key).not.toBeNull();
    }
  });
});
