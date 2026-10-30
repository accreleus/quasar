import { describe, expect, it } from "vitest";
import { detectPadEnv, hatDirection, PADS, parseEntry, parsePadId, remapGamepad, type PadEntry } from "./gamepadRemap";

// Synthetic entries: they pin the mechanism, not any real pad. A real entry comes from a capture.
const HAT: PadEntry = {
  vid: "2dc8", pid: "6006", platform: "win", browser: "chrome", source: "synthetic",
  buttons: 13, axes: 6, hatAxis: 5,
  map: "a:b1,b:b0,x:b4,y:b3,leftshoulder:b6,rightshoulder:b7,righttrigger:b9,back:b10,start:b11,guide:b12,lefttrigger:a2,leftx:a0,lefty:a1,rightx:a3,righty:a4,dpup:h0.1,dpright:h0.2,dpdown:h0.4,dpleft:h0.8",
};
const FIREFOX: PadEntry = {
  ...HAT, browser: "firefox", buttons: 17, axes: 8, hatAxis: 7, triggerRest: 0,
  map: "a:b0,b:b1,x:b2,y:b3,lefttrigger:a5,leftx:a0,lefty:a1,rightx:a2,righty:a3,dpup:h0.1,dpright:h0.2,dpdown:h0.4,dpleft:h0.8",
};
const DPAD_BUTTONS: PadEntry = {
  vid: "1234", pid: "0001", platform: "linux", browser: "chrome", source: "synthetic",
  buttons: 16, axes: 4,
  map: "a:b0,b:b1,dpup:b12,dpdown:b13,dpleft:b14,dpright:b15,leftx:a0,lefty:a1,rightx:a2,righty:a3",
};
const TABLE = [HAT, FIREFOX, DPAD_BUTTONS];
const WIN_CHROME = { platform: "win", browser: "chrome" } as const;
const NEUTRAL = 3.286;

const chrome = (vid: string, pid: string) => `Some Pad (Vendor: ${vid} Product: ${pid})`;
const ID = chrome("2dc8", "6006");

function raw(e: PadEntry, pressed: number[] = [], axes: Record<number, number> = {}) {
  const buttons = Array.from({ length: e.buttons }, (_, i) => (pressed.includes(i) ? 1 : 0));
  const trig = [...e.map.matchAll(/trigger:a(\d+)/g)].map((m) => Number(m[1]));
  const a = Array.from({ length: e.axes }, (_, i) => (trig.includes(i) ? (e.triggerRest ?? -1) : 0));
  if (e.hatAxis !== undefined) a[e.hatAxis] = NEUTRAL;
  for (const [i, v] of Object.entries(axes)) a[Number(i)] = v;
  return { buttons, axes: a };
}
const run = (e: PadEntry, pressed: number[], axes = {}, env = { platform: e.platform, browser: e.browser }, id = chrome(e.vid, e.pid)) => {
  const r = raw(e, pressed, axes);
  return remapGamepad(id, r.buttons, r.axes, env, TABLE);
};
const down = (r: { buttons: number[] } | null) => r!.buttons.flatMap((v, i) => (v ? [i] : []));

describe("parsePadId", () => {
  it("reads the Chrome form", () => expect(parsePadId("8BitDo Pro 2 (Vendor: 2DC8 Product: 6006)")).toBe("2dc8:6006"));
  it("reads the Firefox form", () => expect(parsePadId("2dc8-6006-8BitDo Pro 2")).toBe("2dc8:6006"));
  it("is null for an id without vendor/product", () => expect(parsePadId("Generic USB Gamepad")).toBeNull());
});

describe("detectPadEnv", () => {
  const chromeUa = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36";
  const firefoxUa = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:130.0) Gecko/20100101 Firefox/130.0";
  it("prefers userAgentData.platform over navigator.platform", () => {
    expect(detectPadEnv({ platform: "Linux x86_64", userAgent: chromeUa, userAgentData: { platform: "Windows" } }))
      .toEqual({ platform: "win", browser: "chrome" });
  });
  it("reads each platform and browser family", () => {
    expect(detectPadEnv({ platform: "MacIntel", userAgent: chromeUa })).toEqual({ platform: "mac", browser: "chrome" });
    expect(detectPadEnv({ platform: "Linux x86_64", userAgent: chromeUa })).toEqual({ platform: "linux", browser: "chrome" });
    expect(detectPadEnv({ platform: "Win32", userAgent: firefoxUa })).toEqual({ platform: "win", browser: "firefox" });
  });
  it("is other for anything it does not know (Safari, Android)", () => {
    expect(detectPadEnv({ platform: "iPhone", userAgent: "Mozilla/5.0 AppleWebKit Version/17 Safari/605" }))
      .toEqual({ platform: "other", browser: "other" });
    expect(detectPadEnv({ userAgentData: { platform: "Android" }, userAgent: chromeUa }).platform).toBe("other");
  });
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
  it("never reads ordinary analog values as a press", () => {
    for (const v of [0, 0.05, -0.05, 0.5, -0.5, 0.25, 0.9, 0.1]) expect(hatDirection(v), String(v)).toBe(0);
  });
  it("accepts a step with float and rounding noise", () => {
    expect(hatDirection(-1.01)).toBe(1);
    expect(hatDirection(0.1428571)).toBe(4);
    expect(hatDirection(0.7)).toBe(8);
  });
});

describe("remapGamepad: the table is exact", () => {
  it("is null for an id the table does not know", () => {
    expect(run(HAT, [], {}, WIN_CHROME, chrome("1234", "5678"))).toBeNull();
    expect(run(HAT, [], {}, WIN_CHROME, "Generic USB Gamepad")).toBeNull();
  });

  it("is null on another platform, or another browser, than the entry was captured on", () => {
    expect(run(HAT, [], {}, { platform: "linux", browser: "chrome" })).toBeNull();
    expect(run(HAT, [], {}, { platform: "mac", browser: "chrome" })).toBeNull();
    expect(run(HAT, [], {}, { platform: "other", browser: "other" })).toBeNull();
    expect(run(HAT, [], {}, { platform: "win", browser: "other" })).toBeNull();
    expect(remapGamepad(ID, raw(HAT).buttons, raw(HAT).axes, { platform: "win", browser: "firefox" }, [HAT])).toBeNull();
  });

  it("is null when the button or axis count differs from the capture", () => {
    const r = raw(HAT);
    expect(remapGamepad(ID, r.buttons.slice(1), r.axes, WIN_CHROME, TABLE)).toBeNull();
    expect(remapGamepad(ID, [...r.buttons, 0], r.axes, WIN_CHROME, TABLE)).toBeNull();
    expect(remapGamepad(ID, r.buttons, r.axes.slice(1), WIN_CHROME, TABLE)).toBeNull();
    expect(remapGamepad(ID, r.buttons, [...r.axes, NEUTRAL], WIN_CHROME, TABLE)).toBeNull();
  });

  it("picks the entry for the browser it runs in: same pad, different order", () => {
    expect(down(run(HAT, [1]))).toEqual([0]); // Chrome: raw b1 is the bottom button
    expect(down(run(FIREFOX, [1]))).toEqual([1]); // Firefox: raw b1 is the right one
  });

  it("sends 17 buttons and 4 axes", () => {
    const out = run(HAT, [])!;
    expect([out.buttons.length, out.axes.length]).toEqual([17, 4]);
  });
});

describe("remapGamepad: translation", () => {
  it("face buttons, shoulders, back/start/guide land by position", () => {
    expect(down(run(HAT, [1]))).toEqual([0]);
    expect(down(run(HAT, [0]))).toEqual([1]);
    expect(down(run(HAT, [4]))).toEqual([2]);
    expect(down(run(HAT, [3]))).toEqual([3]);
    expect(down(run(HAT, [6, 7, 9, 10, 11, 12]))).toEqual([4, 5, 7, 8, 9, 16]);
  });

  it("sticks follow the entry's axes", () => {
    expect(run(HAT, [], { 0: -1, 1: 1, 3: 0.25, 4: -0.75 })!.axes).toEqual([-1, 1, 0.25, -0.75]);
  });

  it("d-pad from the hat: directions, diagonals, neutral", () => {
    const hat = (v: number) => down(run(HAT, [], { 5: v }));
    expect(hat(-1)).toEqual([12]);
    expect(hat(-0.429)).toEqual([15]);
    expect(hat(0.143)).toEqual([13]);
    expect(hat(0.714)).toEqual([14]);
    expect(hat(-0.714)).toEqual([12, 15]);
    expect(hat(0.429)).toEqual([13, 14]);
    expect(hat(NEUTRAL)).toEqual([]);
  });

  it("reads the hat at the entry's hatAxis only: another axis moving, or the hat elsewhere, is no press", () => {
    expect(down(run(HAT, [], { 4: -1 }))).toEqual([]); // an axis next to the hat
    expect(down(run(HAT, [], { 2: -1, 3: -1, 4: -1 }))).toEqual([]);
    expect(down(run(HAT, [], { 5: 0.05 }))).toEqual([]); // analog noise on the hat axis
    // a layout whose hat is on axis 7 must not read axis 5
    expect(down(run(FIREFOX, [], { 5: -1 }))).toEqual([]);
    expect(down(run(FIREFOX, [], { 7: -1 }))).toEqual([12]);
  });

  it("d-pad that arrives as buttons needs no hat", () => {
    expect(down(run(DPAD_BUTTONS, [12, 15]))).toEqual([12, 15]);
    expect(down(run(DPAD_BUTTONS, [0, 1]))).toEqual([0, 1]);
  });

  it("trigger axis: default rest -1, or the entry's own rest", () => {
    expect(run(HAT, [], { 2: -1 })!.buttons[6]).toBe(0);
    expect(run(HAT, [], { 2: 0 })!.buttons[6]).toBe(0.5);
    expect(run(HAT, [], { 2: 1 })!.buttons[6]).toBe(1);
    expect(run(FIREFOX, [], { 5: 0 })!.buttons[6]).toBe(0);
    expect(run(FIREFOX, [], { 5: 0.5 })!.buttons[6]).toBe(0.5);
  });
});

describe("parseEntry", () => {
  it("rejects a hat with no hatAxis, and a hatAxis or index outside the capture", () => {
    expect(() => parseEntry({ ...HAT, hatAxis: undefined })).toThrow(/no hatAxis/);
    expect(() => parseEntry({ ...HAT, hatAxis: 6 })).toThrow(/hatAxis is outside/);
    expect(() => parseEntry({ ...HAT, map: "a:b13" })).toThrow(/outside its capture/);
    expect(() => parseEntry({ ...HAT, map: "leftx:a6" })).toThrow(/outside its capture/);
    expect(() => parseEntry({ ...HAT, map: "a:x1" })).toThrow(/bad entry/);
  });
});

describe("the shipped table", () => {
  it("every entry parses, names its capture, and has a unique key", () => {
    const keys = PADS.map((e) => `${e.vid}:${e.pid}:${e.platform}:${e.browser}`);
    expect(new Set(keys).size).toBe(keys.length);
    for (const e of PADS) {
      expect(e.source.length, keys.join()).toBeGreaterThan(10);
      expect(e.platform).not.toBe("other");
      expect(e.browser).not.toBe("other");
      expect(() => parseEntry(e)).not.toThrow();
    }
  });
});
