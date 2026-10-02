/**
 * An instant as local 24-hour wall-clock time: "13:48:02", or "13:48" without
 * seconds. Local, not UTC: an operator correlating it with what they just did
 * is reading their own clock. An unreadable instant renders as "—".
 */
export function clockTime(at: string, opts: { seconds?: boolean } = {}): string {
  const ms = Date.parse(at);
  if (!Number.isFinite(ms)) return "—";
  return new Date(ms).toLocaleTimeString(undefined, {
    hour12: false,
    hour: "2-digit",
    minute: "2-digit",
    ...(opts.seconds === false ? {} : { second: "2-digit" }),
  });
}
