// The overlay summon combo (Ctrl+Alt+Shift+Q), defined once. Two listeners
// answer it and must never both answer the same press: capture.ts only while
// the pointer is locked (releases the lock, swallows the 'Q'), SessionPage only
// while it is not. The split on `document.pointerLockElement` keeps the cases
// disjoint, so only the chord definition itself has to stay in sync — hence
// this module.

export interface SummonComboEvent {
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
  /** Physical key (`KeyboardEvent.code`), so the chord survives keyboard layouts. */
  code: string;
  repeat?: boolean;
}

/** Auto-repeat excluded: holding the chord must summon once. */
export function isSummonCombo(e: SummonComboEvent): boolean {
  return (
    e.ctrlKey && e.altKey && e.shiftKey && e.code === "KeyQ" && e.repeat !== true
  );
}

/** #647: Ctrl+Alt+Shift+E sends Esc to the game. Firefox only: it keeps Esc
 *  with any modifier for itself (it leaves full screen and the pointer lock) and
 *  has no Keyboard Lock, so a captured Firefox player has no other way to press
 *  Esc in the game. Chromium forwards Esc itself through Keyboard Lock. */
export function isSendEscCombo(e: SummonComboEvent): boolean {
  return e.ctrlKey && e.altKey && e.shiftKey && e.code === "KeyE" && e.repeat !== true;
}

/** Whether this browser gets the send-Esc combo (Firefox, by its user agent). */
export function sendEscComboEnabled(ua: string = navigator.userAgent): boolean {
  return /\bFirefox\//.test(ua);
}
