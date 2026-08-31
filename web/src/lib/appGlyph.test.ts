import { describe, expect, it } from "vitest";
import { appGlyph } from "./appGlyph";

describe("appGlyph", () => {
  it("returns the first letter uppercased for a single word", () => {
    expect(appGlyph("quasar")).toBe("Q");
    expect(appGlyph("Minecraft")).toBe("M");
  });

  it("returns first letters of the first two words for multi-word names", () => {
    expect(appGlyph("Half Life")).toBe("HL");
    expect(appGlyph("call of duty")).toBe("CO");
  });

  it("returns ? for an empty string", () => {
    expect(appGlyph("")).toBe("?");
  });

  it("collapses extra whitespace between words", () => {
    expect(appGlyph("Red  Dead")).toBe("RD");
  });
});
