// When a WebTransport view falls back to WebRTC, and the reason it reports
// (control-api.md §Transport fallback).

/** `transport.fallback` reasons the contract lists. */
const FALLBACK_REASONS = new Set([
  "connect_timeout",
  "connect_failed",
  "auth_failed",
  "unsupported_version",
  "takeover_rate_limited",
  "decoder_unsupported",
  "decode_error",
  "stream_lost",
  "internal",
]);

/** Ends the view with no fallback: the session is over, or another device displaced this one. */
const NO_FALLBACK = new Set(["session_ended", "taken_over", "closed"]);

/** The `transport.fallback` reason for a WebTransport failure `code`, or null when the view stops instead. */
export function fallbackReason(code: string): string | null {
  if (NO_FALLBACK.has(code)) return null;
  // The presenter cannot draw, which is a decoder that cannot present.
  if (code === "no_webgpu") return "decoder_unsupported";
  return FALLBACK_REASONS.has(code) ? code : "internal";
}

/** The cause a phone user can read when the HUD is all there is: the code, then the error's own
 *  name and message (the code alone, `internal`, hid why Safari fell back), capped for the pane. */
export function fallbackCause(code: string, err?: unknown): string {
  const parts = [code];
  if (err instanceof Error) {
    if (err.name !== "TransportError") parts.push(err.name);
    if (err.message && err.message !== code) parts.push(err.message);
  } else if (err !== undefined) {
    parts.push(String(err));
  }
  return parts.join(": ").slice(0, 200);
}

const REMEMBER_PREFIX = "quasar.wt-fallback.";

/** Keeps a session on WebRTC across a reload of this tab, so each reload does not retry WebTransport. */
export function rememberFallback(sessionId: string, reason: string): void {
  try {
    sessionStorage.setItem(REMEMBER_PREFIX + sessionId, reason);
  } catch {
    /* Storage blocked: a reload retries WebTransport, which falls back again. */
  }
}

export function rememberedFallback(sessionId: string | undefined): string | null {
  try {
    return sessionId ? sessionStorage.getItem(REMEMBER_PREFIX + sessionId) : null;
  } catch {
    return null;
  }
}
