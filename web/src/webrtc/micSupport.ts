// The microphone feature detect, apart from the capture code (mic.ts): the codec
// probe the library page runs needs only this, and mic.ts is session-view code.

/**
 * Cheap sync feature detect. getUserMedia is secure-context-only (same
 * constraint gates Keyboard Lock on plain-HTTP LAN deploys, #376), so an
 * insecure origin reports false regardless of browser capability.
 */
export function microphoneSupported(): boolean {
  return (
    typeof navigator !== "undefined" &&
    typeof navigator.mediaDevices?.getUserMedia === "function" &&
    typeof window !== "undefined" &&
    window.isSecureContext === true
  );
}
