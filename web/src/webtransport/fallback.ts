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
