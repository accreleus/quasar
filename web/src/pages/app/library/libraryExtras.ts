// Per-title facts the organising controls, the search results, the Home shelves
// and the game page read: genres, when a title was added, this user's play
// history, a small icon, and the game page's logo, credits and last session. The
// control-api carries none of them yet, so the source below yields null and every
// consumer degrades to what `App` itself has (A–Z only, no Genre menu, no
// captions, no history). Wiring a source here is the whole change once an
// amendment adds them.

export interface GameSessionStats {
  started_at: string;
  duration_seconds: number;
  requested: { width: number; height: number; fps: number; bitrate_kbps: number; codec: string };
  delivered: {
    width: number;
    height: number;
    fps_avg: number;
    bitrate_kbps_avg: number;
    codec: string;
    rtt_ms_p50: number;
    held_full_res_pct: number;
    host: string;
  };
}

export interface LibraryExtras {
  genres: string[];
  added_at: string | null;
  play: {
    last_played_at: string;
    total_seconds: number;
    sessions?: number;
    last_session?: GameSessionStats | null;
  } | null;
  icon_url?: string | null;
  // Game page only.
  rank?: number;
  logo_url?: string | null;
  developer?: string | null;
  publisher?: string | null;
  release_date?: string | null;
  controller_support?: string | null;
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
