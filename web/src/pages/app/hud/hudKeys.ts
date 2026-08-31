// The HUD's keyboard map (handoff §E "Interaction rules"): one pure function
// from a key press to an intent. The summon chord is not redefined here —
// `input/summonCombo.ts` is its single definition, shared with capture.ts.

import { isSummonCombo } from "../../../input/summonCombo";

/** The shelf's sections, in the order ←/→ walk them. */
export const HUD_TABS = ["games", "input", "stats", "display"] as const;
export type HudTab = (typeof HUD_TABS)[number];

/** The next section in the given direction, wrapping. */
export function stepTab(tab: HudTab, direction: 1 | -1): HudTab {
  const i = HUD_TABS.indexOf(tab);
  const at = i === -1 ? 0 : i;
  return HUD_TABS[(at + direction + HUD_TABS.length) % HUD_TABS.length];
}

export type HudKeyAction =
  | "close"
  | "release-and-open"
  | "release"
  | "stats"
  | "next-tab"
  | "prev-tab";

/** The parts of a KeyboardEvent this map reads. `code` is optional so a test
 *  (and a synthetic event) can supply only `key`. */
export interface HudKeyEvent {
  key: string;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  metaKey?: boolean;
  /** Physical key. Preferred for the chords — it survives keyboard layouts. */
  code?: string;
  repeat?: boolean;
}

export interface HudKeyState {
  open: boolean;
  /** Capture engaged. While it is, an unmodified key belongs to the game. */
  captured: boolean;
}

/** `KeyboardEvent.code` for a letter key, derived when the event carries none.
 *  Layout-correct events always have `code`; this is only the fallback. */
function codeOf(e: HudKeyEvent): string {
  if (e.code) return e.code;
  return /^[a-z]$/i.test(e.key) ? `Key${e.key.toUpperCase()}` : "";
}

export function hudKeyAction(e: HudKeyEvent, state: HudKeyState): HudKeyAction | null {
  // Escape closes an open shelf. Closed, Escape belongs to the session (it is
  // capture.ts's own release gesture where Keyboard Lock is unavailable).
  if (e.key === "Escape") return state.open ? "close" : null;

  const code = codeOf(e);
  // Built field by field, never spread: a real KeyboardEvent's properties are
  // prototype getters, so `{...e}` copies none of them.
  const chord = {
    ctrlKey: e.ctrlKey,
    altKey: e.altKey,
    shiftKey: e.shiftKey,
    code,
    repeat: e.repeat,
  };
  if (isSummonCombo(chord)) return "release-and-open";
  if (chord.ctrlKey && chord.altKey && chord.shiftKey && code === "KeyZ" && e.repeat !== true) {
    return "release";
  }
  // Shift+S is an ordinary in-game binding, so it is ours only while input is
  // not captured. With a further modifier it belongs to the browser.
  if (
    !state.captured &&
    e.shiftKey &&
    !e.ctrlKey &&
    !e.altKey &&
    !e.metaKey &&
    code === "KeyS"
  ) {
    return "stats";
  }

  if (state.open && e.key === "ArrowRight") return "next-tab";
  if (state.open && e.key === "ArrowLeft") return "prev-tab";
  return null;
}
