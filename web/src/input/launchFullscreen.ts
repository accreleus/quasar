// #647: Play goes straight in. Full screen needs the Play click's transient
// activation, which is long gone by the time a launch reveals, so the click
// spends it here, synchronously, before the caller's first await. The whole
// document goes full screen, so the loader is full screen too and the SPA's
// route change to the session page keeps it. Esc goes to the game through
// capture.ts's own Keyboard Lock once input is captured, not from here: a lock
// taken at the click would outlive a launch that never captured.

import { launchImmersive } from "../settings/displayPreferences";

/** The full screen this module asked for; only that one is ever left again. */
let entered: Promise<void> | null = null;

/** Call from the click handler before any await. Returns whether the setting
 *  is on. A refusal is quiet: the session page offers "Click to play" instead. */
export function enterLaunchFullscreen(): boolean {
  if (!launchImmersive()) return false;
  if (!document.fullscreenElement) {
    const req = document.documentElement.requestFullscreen?.();
    if (req) {
      entered = req;
      req.catch(() => {
        if (entered === req) entered = null;
      });
    }
  }
  return true;
}

/** Leave the full screen a launch entered: a launch that did not happen, or the
 *  end of the session. Waits for a pending request, so a fast failure cannot
 *  leave the grant landing afterwards. A full screen the user made is untouched. */
export function leaveLaunchFullscreen(): void {
  const req = entered;
  entered = null;
  void req
    ?.then(() => {
      if (document.fullscreenElement === document.documentElement) return document.exitFullscreen();
    })
    .catch(() => {});
}
