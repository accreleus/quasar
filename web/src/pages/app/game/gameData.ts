// Game-page data the control-api contract does not carry yet: logo art, genres
// and credits, per-user play history, the last session's delivery, and per-game
// stream settings. Until it does (issue #720) every source below yields nothing
// and sends no request, and the page shows what the catalogue already holds.

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

export interface GamePlay {
  last_played_at: string;
  total_seconds: number;
  sessions: number;
  last_session: GameSessionStats | null;
}

export interface GameExtras {
  rank: number;
  logo_url: string | null;
  genres: string[];
  developer: string | null;
  publisher: string | null;
  release_date: string | null;
  controller_support: string | null;
  play: GamePlay | null;
}

export type ExtrasMap = Record<string, GameExtras>;

/** Null until the control-api carries these facts (issue #720). */
export function useGameExtras(): ExtrasMap | null {
  return null;
}

// ── Per-game stream settings ────────────────────────────────────────────────

export interface GameSettings {
  /** The profile chosen for this game (a launch profile id). */
  profile_id: string;
  /** Wire codec or omitted for Auto. */
  codec?: "h264" | "h265" | "av1";
  /** "explicit" = the user set it; "remembered" = carried over from the last launch. */
  source: "explicit" | "remembered";
  updated_at?: string;
}

// Per-game stream settings have no control-api home yet (issue #720): nothing is
// read or written, so the page always opens on the device recommendation.
export function getGameSettings(_token: string, _appId: string): Promise<GameSettings | null> {
  return Promise.resolve(null);
}

export function putGameSettings(_token: string, _appId: string, s: GameSettings): Promise<GameSettings> {
  return Promise.resolve(s);
}

export function clearGameSettings(_token: string, _appId: string): Promise<void> {
  return Promise.resolve();
}

// ── Formatting shared by the experiments ────────────────────────────────────

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
