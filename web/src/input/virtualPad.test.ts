import { describe, expect, it } from "vitest";
import {
  BTN,
  STICK_DEAD_ZONE,
  STICK_RIGHT,
  clampToRadius,
  dpadFromOffset,
  neutralPad,
  setButton,
  setStick,
  stickFromOffset,
} from "./virtualPad";

describe("stickFromOffset", () => {
  it("reads zero in the dead zone and at the centre", () => {
    expect(stickFromOffset(0, 0, 50)).toEqual({ x: 0, y: 0 });
    expect(stickFromOffset(50 * STICK_DEAD_ZONE * 0.9, 0, 50)).toEqual({ x: 0, y: 0 });
  });

  it("starts from 0 at the dead-zone edge and reaches 1 at the rim", () => {
    const edge = stickFromOffset(50 * STICK_DEAD_ZONE + 0.001, 0, 50);
    expect(edge.x).toBeGreaterThan(0);
    expect(edge.x).toBeLessThan(0.01);
    expect(stickFromOffset(50, 0, 50)).toEqual({ x: 1, y: 0 });
  });

  it("clamps a finger dragged past the rim, keeping the direction", () => {
    const { x, y } = stickFromOffset(300, 400, 50);
    expect(Math.hypot(x, y)).toBeCloseTo(1, 6);
    expect(x / y).toBeCloseTo(0.75, 6);
  });

  it("is down-positive on y, as the standard layout wants", () => {
    expect(stickFromOffset(0, 50, 50).y).toBe(1);
    expect(stickFromOffset(0, -50, 50).y).toBe(-1);
  });

  it("is zero for a zero-size stick", () => {
    expect(stickFromOffset(10, 10, 0)).toEqual({ x: 0, y: 0 });
  });
});

describe("clampToRadius", () => {
  it("leaves a point inside alone and pulls one outside onto the circle", () => {
    expect(clampToRadius(3, 4, 10)).toEqual({ x: 3, y: 4 });
    const c = clampToRadius(30, 40, 10);
    expect(c.x).toBeCloseTo(6);
    expect(c.y).toBeCloseTo(8);
  });
});

describe("dpadFromOffset", () => {
  it("gives one direction on an axis and two on a diagonal", () => {
    expect(dpadFromOffset(0, -40, 48)).toEqual({ up: true, down: false, left: false, right: false });
    expect(dpadFromOffset(40, 40, 48)).toEqual({ up: false, down: true, left: false, right: true });
    expect(dpadFromOffset(2, 2, 48)).toEqual({ up: false, down: false, left: false, right: false });
  });
});

describe("pad state", () => {
  it("is 17 buttons and 4 axes, all zero", () => {
    const p = neutralPad();
    expect(p.buttons).toHaveLength(17);
    expect(p.axes).toEqual([0, 0, 0, 0]);
  });

  it("sets and clears a button without touching the rest, and without mutating", () => {
    const p = neutralPad();
    const a = setButton(p, BTN.A, true);
    expect(a.buttons[BTN.A]).toBe(1);
    expect(p.buttons[BTN.A]).toBe(0);
    expect(setButton(a, BTN.A, true)).toBe(a);
    expect(setButton(a, BTN.A, false).buttons.every((v) => v === 0)).toBe(true);
  });

  it("writes a stick to its own axis pair", () => {
    const p = setStick(neutralPad(), STICK_RIGHT, 0.5, -1);
    expect(p.axes).toEqual([0, 0, 0.5, -1]);
    expect(setStick(p, STICK_RIGHT, 0.5, -1)).toBe(p);
  });
});
