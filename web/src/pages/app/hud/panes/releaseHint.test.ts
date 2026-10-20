// #647: the release hint and the Input pane's Esc row read one decision. One
// case per row of the issue's platform matrix, plus the states a session can
// reach on its way there.
import { describe, expect, it } from "vitest";
import { escReleases, releaseHint, type ReleaseInputs } from "./InputPane";

const CHROME_COMBOS =
  "Ctrl+Alt+Shift+Z releases input · Ctrl+Alt+Shift+Q opens the menu · hold Esc to leave full screen";
const ESC = "Esc releases input";
const TOUCH = "Tap the session-menu button to release input";

const desktop: ReleaseInputs = {
  pointerLockAvailable: true,
  touchLook: false,
  fullscreen: true,
  keyboardLockAvailable: true,
  lockRefused: false,
  certUntrusted: false,
};
const touch: ReleaseInputs = { ...desktop, pointerLockAvailable: false, touchLook: true, keyboardLockAvailable: false };

const rows: [string, ReleaseInputs, boolean, string][] = [
  ["Chrome / Edge desktop, full screen with Keyboard Lock", desktop, false, CHROME_COMBOS],
  ["Firefox desktop (no Keyboard Lock)", { ...desktop, keyboardLockAvailable: false }, true, ESC],
  ["Safari macOS (no Keyboard Lock)", { ...desktop, keyboardLockAvailable: false }, true, ESC],
  ["iPadOS Safari (no Pointer Lock, touch)", { ...touch, fullscreen: false }, true, TOUCH],
  ["iPhone Safari (no element full screen, touch)", { ...touch, fullscreen: false }, true, TOUCH],
  ["Android Chrome (full screen, touch)", { ...touch, keyboardLockAvailable: true }, true, TOUCH],
  ["Esc pressed during the loader (left full screen)", { ...desktop, fullscreen: false }, true, ESC],
  ["takeover from another device (never full screen)", { ...desktop, fullscreen: false }, true, ESC],
  ["Keyboard Lock refused", { ...desktop, lockRefused: true }, true, ESC],
  ["no Pointer Lock, no touchscreen", { ...desktop, pointerLockAvailable: false }, true, ESC],
  [
    "bypassed certificate (Esc capture unreliable)",
    { ...desktop, certUntrusted: true },
    false,
    "Ctrl+Alt+Shift+Z releases input · Ctrl+Alt+Shift+Q opens the menu",
  ],
];

describe("release wording (#647)", () => {
  it.each(rows)("%s", (_name, inputs, esc, hint) => {
    expect(escReleases(inputs)).toBe(esc);
    expect(releaseHint(inputs)).toBe(hint);
  });

  it("never promises Esc where the pane says Esc is sent to the game", () => {
    for (const [, inputs] of rows) {
      if (!escReleases(inputs)) expect(releaseHint(inputs)).not.toBe(ESC);
    }
  });
});
