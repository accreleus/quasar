// Per-title facts the organising controls and the search results read: genres,
// when a title was added, this user's play history, a small icon. The control-api
// carries none of them yet, so the source below yields null and every consumer
// degrades to what `App` itself has (A–Z only, no Genre menu, no captions).
// Wiring a source here is the whole change once an amendment adds them.

export interface LibraryExtras {
  genres: string[];
  added_at: string | null;
  play: { last_played_at: string; total_seconds: number } | null;
  icon_url?: string | null;
}

export type ExtrasMap = Record<string, LibraryExtras>;

/** Null until the control-api carries these facts: issue #720 (needs an
 *  amendment and owner sign-off). Everything above degrades without them. */
export function useLibraryExtras(): ExtrasMap | null {
  return null;
}

const rtf = typeof Intl !== "undefined" ? new Intl.RelativeTimeFormat("en", { numeric: "auto" }) : null;

/** "today", "yesterday", "3 days ago", "last week", "2 months ago". */
export function relativeDay(iso: string, now = Date.now()): string {
  const days = Math.floor((now - Date.parse(iso)) / 86400e3);
  if (days <= 0) return "today";
  if (days < 7) return rtf ? rtf.format(-days, "day") : `${days} days ago`;
  if (days < 14) return "last week";
  if (days < 60) return `${Math.round(days / 7)} weeks ago`;
  if (days < 365) return `${Math.round(days / 30)} months ago`;
  return `${Math.round(days / 365)} years ago`;
}

/** "42 h", "35 min". */
export function hoursLabel(seconds: number): string {
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))} min`;
  const h = seconds / 3600;
  return `${h < 10 ? h.toFixed(1).replace(/\.0$/, "") : Math.round(h)} h`;
}
