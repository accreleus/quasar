// #647: Play goes straight in. Full screen and Keyboard Lock both need the Play
// click's transient activation, which is long gone by the time a launch reveals,
// so the click spends it here, synchronously, before the caller's first await.
// The whole document goes full screen, so the loader is full screen too and the
// SPA's route change to the session page keeps it.

import { launchImmersive } from "../settings/displayPreferences";

/** Call from the click handler before any await. Returns whether the setting
 *  is on (the caller may want to undo it if the click ends up not launching).
 *  A refusal is quiet: the session page offers "Click to play" instead. */
export function enterLaunchFullscreen(): boolean {
  if (!launchImmersive()) return false;
  if (!document.fullscreenElement) {
    document.documentElement.requestFullscreen?.().catch(() => {});
  }
  // Chromium only; takes effect while full screen, so Esc reaches the game.
  const kb = (navigator as unknown as { keyboard?: { lock?: (keys?: string[]) => Promise<void> } })
    .keyboard;
  kb?.lock?.(["Escape"]).catch(() => {});
  return true;
}

/** Undo `enterLaunchFullscreen` when the click did not launch after all. */
export function leaveLaunchFullscreen(): void {
  if (document.fullscreenElement) document.exitFullscreen?.().catch(() => {});
}
