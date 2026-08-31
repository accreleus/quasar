/**
 * The pre-v3 date helpers, still used by the pages that have not been ported to
 * the v3 language yet. Bytes, durations, bitrates and relative times all live in
 * `./format/` now; this file goes when its last caller is ported.
 */

export function fmtDate(s: string | null | undefined): string {
  if (!s) return "—";
  return new Date(s).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

/** Explicit HH:MM:SS clock time (padded, with seconds). */
export function fmtTime(s: string | null | undefined): string {
  if (!s) return "—";
  return new Date(s).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

/** Bare locale clock time. A different function from fmtTime, not a shared
 *  implementation — the two render different strings; folding them would
 *  change output. */
export function fmtClockTime(s: string | null | undefined): string {
  if (!s) return "—";
  return new Date(s).toLocaleTimeString();
}
