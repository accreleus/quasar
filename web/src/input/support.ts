// Device probes the account page and the codec probe need without loading the
// capture engine (capture.ts), which only the session view runs.

/** Raw mouse input (`unadjustedMovement`) is Chromium-only and cannot be probed without a lock
 *  request, so the account page reads the user agent (Chrome, Edge, Opera and Brave all say
 *  `Chrome/`; Firefox and Safari do not). Not `userAgentData`: it is absent on plain HTTP, where
 *  many installs run. A wrong guess costs nothing: engage() still falls back to a normal lock and says so. */
export function rawMouseSupported(): boolean {
  return pointerLockSupported() && /\bChrome\//.test(navigator.userAgent);
}

/** Probed on `Element.prototype`, not `"pointerLockElement" in document` —
 *  WebKit ships the two independently, and the element method is the one the
 *  grab gesture calls (iPadOS Safari has never shipped it). */
export function pointerLockSupported(): boolean {
  return typeof Element !== "undefined" && "requestPointerLock" in Element.prototype;
}

/**
 * Touchscreen AND no Pointer Lock — exactly the devices where the touch layer
 * runs. Answers "are gestures live", not "does this screen accept a finger"
 * (a touch-capable laptop still has Pointer Lock and its mouse-look is the
 * mouse); the UI words the capture hint from this.
 */
export function touchLookSupported(): boolean {
  if (pointerLockSupported()) return false;
  // Two probes: `maxTouchPoints` reads 0 under WebKit's iPad emulation, which
  // does set `ontouchstart`; a real iPad answers yes to both.
  return (
    (typeof navigator !== "undefined" && (navigator.maxTouchPoints ?? 0) > 0) ||
    (typeof window !== "undefined" && "ontouchstart" in window)
  );
}
