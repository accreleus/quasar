export type BestEffortLevel = "silent-debug" | "console-warn" | "user-visible";

/**
 * Emit a consistent breadcrumb for a best-effort failure that is intentionally
 * non-fatal (capability probes, non-critical polling, hidden-as-empty fallbacks).
 * Production UX stays quiet; this only varies console verbosity so a debug trail
 * exists. It does NOT render any UI — the caller still owns any user-facing message.
 *
 *  - "silent-debug"  -> console.debug (default-hidden breadcrumb)
 *  - "console-warn"  -> console.warn
 *  - "user-visible"  -> console.error (caller also surfaces UI; this aids correlation)
 */
export function reportBestEffortFailure(
  level: BestEffortLevel,
  context: string,
  err: unknown,
): void {
  const msg = `[best-effort] ${context}`;
  switch (level) {
    case "silent-debug":
      console.debug(msg, err);
      break;
    case "console-warn":
      console.warn(msg, err);
      break;
    case "user-visible":
      console.error(msg, err);
      break;
  }
}
