import { describe, expect, it } from "vitest";
import { isSummonCombo } from "./summonCombo";

const chord = { ctrlKey: true, altKey: true, shiftKey: true, code: "KeyQ" };

describe("isSummonCombo", () => {
  it("matches Ctrl+Alt+Shift+Q", () => {
    expect(isSummonCombo(chord)).toBe(true);
  });

  it("requires every modifier", () => {
    expect(isSummonCombo({ ...chord, ctrlKey: false })).toBe(false);
    expect(isSummonCombo({ ...chord, altKey: false })).toBe(false);
    expect(isSummonCombo({ ...chord, shiftKey: false })).toBe(false);
  });

  // The release chord is deliberately a different letter and must not summon:
  // releasing by Esc or Ctrl+Alt+Shift+Z does not open the overlay.
  it("does not match the release chord or a bare Q", () => {
    expect(isSummonCombo({ ...chord, code: "KeyZ" })).toBe(false);
    expect(isSummonCombo({ ctrlKey: false, altKey: false, shiftKey: false, code: "KeyQ" })).toBe(false);
  });

  it("ignores auto-repeat, so holding the chord summons once", () => {
    expect(isSummonCombo({ ...chord, repeat: true })).toBe(false);
  });

  // `code`, not `key`: with Ctrl+Alt held some layouts report a different
  // `key`, and the chord must survive that.
  it("keys off the physical code, not the produced character", () => {
    expect(isSummonCombo({ ...chord, code: "KeyA" })).toBe(false);
  });
});
