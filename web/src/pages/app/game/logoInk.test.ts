import { describe, expect, it } from "vitest";
import { logoReadable } from "./logoInk";

const fill = (px: number, rgba: [number, number, number, number]) => Array.from({ length: px }, () => rgba).flat();

describe("logoReadable", () => {
  it("accepts a white logo", () => expect(logoReadable(fill(100, [255, 255, 255, 255]))).toBe(true));
  it("rejects a black logo", () => expect(logoReadable(fill(100, [10, 10, 10, 255]))).toBe(false));
  it("ignores transparent pixels", () =>
    expect(logoReadable([...fill(90, [0, 0, 0, 0]), ...fill(10, [240, 220, 40, 255])])).toBe(true));
  it("rejects an empty image", () => expect(logoReadable(fill(100, [255, 255, 255, 0]))).toBe(false));
});
