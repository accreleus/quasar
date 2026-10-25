import { describe, expect, it } from "vitest";
import { isSendEscCombo, isSummonCombo, sendEscComboEnabled } from "./summonCombo";

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

describe("send-Esc combo (#647)", () => {
  it("is Ctrl+Alt+Shift+E, not repeated", () => {
    expect(isSendEscCombo({ ctrlKey: true, altKey: true, shiftKey: true, code: "KeyE" })).toBe(true);
    expect(isSendEscCombo({ ctrlKey: true, altKey: true, shiftKey: true, code: "KeyE", repeat: true })).toBe(false);
    expect(isSendEscCombo({ ctrlKey: true, altKey: false, shiftKey: true, code: "KeyE" })).toBe(false);
  });

  it("is on for Firefox only", () => {
    expect(sendEscComboEnabled("Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:142.0) Gecko/20100101 Firefox/142.0")).toBe(true);
    expect(sendEscComboEnabled("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0")).toBe(false);
    expect(sendEscComboEnabled("Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15")).toBe(false);
  });
});
