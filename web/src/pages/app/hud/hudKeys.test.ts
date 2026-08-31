import { describe, expect, it } from "vitest";
import { hudKeyAction, stepTab } from "./hudKeys";

const k = (key: string, mods: Partial<{ ctrl: boolean; alt: boolean; shift: boolean }> = {}) => ({
  key,
  ctrlKey: !!mods.ctrl,
  altKey: !!mods.alt,
  shiftKey: !!mods.shift,
});
const open = (isOpen: boolean) => ({ open: isOpen, captured: false });

describe("hudKeyAction", () => {
  it("maps the shortcuts", () => {
    expect(hudKeyAction(k("Escape"), open(true))).toBe("close");
    expect(hudKeyAction(k("q", { ctrl: true, alt: true, shift: true }), open(false))).toBe(
      "release-and-open",
    );
    expect(hudKeyAction(k("z", { ctrl: true, alt: true, shift: true }), open(false))).toBe(
      "release",
    );
    expect(hudKeyAction(k("S", { shift: true }), open(true))).toBe("stats");
    expect(hudKeyAction(k("ArrowRight"), open(true))).toBe("next-tab");
    expect(hudKeyAction(k("ArrowLeft"), open(false))).toBeNull();
  });

  // Shift+S is a plain letter with a plain modifier: a game may bind it, and
  // while input is captured every such press belongs to the game.
  it("leaves Shift+S to the game while input is captured", () => {
    expect(hudKeyAction(k("S", { shift: true }), { open: false, captured: true })).toBeNull();
    expect(hudKeyAction(k("S", { shift: true }), { open: true, captured: true })).toBeNull();
  });

  // The two chords are the way out of capture, so they must survive it.
  it("still answers the release chords while captured", () => {
    expect(
      hudKeyAction(k("q", { ctrl: true, alt: true, shift: true }), { open: false, captured: true }),
    ).toBe("release-and-open");
    expect(
      hudKeyAction(k("z", { ctrl: true, alt: true, shift: true }), { open: false, captured: true }),
    ).toBe("release");
  });

  it("leaves Escape to the session while the shelf is closed", () => {
    expect(hudKeyAction(k("Escape"), open(false))).toBeNull();
  });

  it("cycles backwards on ArrowLeft while open", () => {
    expect(hudKeyAction(k("ArrowLeft"), open(true))).toBe("prev-tab");
  });

  it("reads the physical key, so the chords survive a non-QWERTY layout", () => {
    expect(
      hudKeyAction(
        { key: "'", code: "KeyQ", ctrlKey: true, altKey: true, shiftKey: true },
        open(false),
      ),
    ).toBe("release-and-open");
  });

  it("ignores auto-repeat on the chords, so holding them fires once", () => {
    expect(
      hudKeyAction(
        { key: "q", code: "KeyQ", ctrlKey: true, altKey: true, shiftKey: true, repeat: true },
        open(false),
      ),
    ).toBeNull();
  });

  it("leaves a modified S to the browser", () => {
    expect(hudKeyAction(k("S", { shift: true, ctrl: true }), open(true))).toBeNull();
  });

  it("leaves a modified S to the browser", () => {
    expect(hudKeyAction(k("S", { shift: true, ctrl: true }), open(true))).toBeNull();
  });
});

describe("stepTab", () => {
  it("walks games → input → stats → display and wraps", () => {
    expect(stepTab("games", 1)).toBe("input");
    expect(stepTab("input", 1)).toBe("stats");
    expect(stepTab("stats", 1)).toBe("display");
    expect(stepTab("display", 1)).toBe("games");
  });

  it("walks backwards the same way", () => {
    expect(stepTab("games", -1)).toBe("display");
    expect(stepTab("stats", -1)).toBe("input");
  });
});
