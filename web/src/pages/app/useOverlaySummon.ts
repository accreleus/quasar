// capture.ts gates keydown on pointer lock, so an uncaptured user has no way
// to reach the drawer (the only route to capture/exit) without this hook —
// a plain document listener, since nothing on the page holds focus yet.
//
// #139: no state, no telemetry, no per-tick work — one listener, calls
// `onSummon` only on an actual chord press.

import { useEffect } from "react";
import { isSummonCombo } from "../../input/summonCombo";

/**
 * Open the session overlay on Ctrl+Alt+Shift+Q while input is NOT captured.
 *
 * While locked, capture.ts owns the chord instead (it must release the lock
 * and swallow 'Q' before the game sees it). Mutually exclusive on
 * `document.pointerLockElement`, so one press never opens the drawer twice.
 *
 * @param onSummon Stable callback (useCallback) — listener re-attaches when it changes.
 */
export function useOverlaySummon(onSummon: () => void): void {
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (!isSummonCombo(e)) return;
      // This listener attaches on mount, before capture.ts's (which waits for
      // the DataChannel), so the lock is still held here when it's about to
      // be released.
      if (document.pointerLockElement) return;
      e.preventDefault();
      onSummon();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [onSummon]);
}
