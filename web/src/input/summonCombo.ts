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
