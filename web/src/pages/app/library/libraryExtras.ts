// Per-title facts the organising controls, the search results, the Home shelves
// and the game page read: genres, when a title was added, this user's play
// history, a small icon, and the game page's logo and credits. They come from the
// app's own fields and GET /v1/me/play-history (control-api.md amendment 38). The
// control-api serves no `rank` or `last_session`, so those stay absent.

import { useMemo } from "react";
import type { App, PlayHistoryItem } from "../../../api/types";
import { useOptionalLibraryCatalog } from "../libraryCatalog";

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

export function buildExtras(apps: readonly App[], history: readonly PlayHistoryItem[]): ExtrasMap {
  const played = new Map(history.map((h) => [h.app_id, h]));
  const out: ExtrasMap = {};
  for (const a of apps) {
    const p = played.get(a.id);
    out[a.id] = {
      genres: a.genres ?? [],
      added_at: a.added_at ?? null,
      play: p ? { last_played_at: p.last_played_at, total_seconds: p.total_seconds, sessions: p.sessions } : null,
      icon_url: a.icon_url ?? null,
      logo_url: a.logo_url ?? null,
      developer: a.developer ?? null,
      publisher: a.publisher ?? null,
      release_date: a.release_date ?? null,
      controller_support: a.controller_support ?? null,
    };
  }
  return out;
}

/** Null until the library and the play history have both loaded, and after
 *  either failed: every consumer then degrades to what `App` alone has (A–Z
 *  only, no Genre menu, no captions, no history). */
export function useLibraryExtras(): ExtrasMap | null {
  const catalog = useOptionalLibraryCatalog();
  const apps = catalog?.settled && !catalog.error ? catalog.apps : null;
  const history = catalog?.playHistory ?? null;
  return useMemo(() => (apps && history ? buildExtras(apps, history) : null), [apps, history]);
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
