// Game-page data the control-api contract does not carry yet: logo art, genres
// and credits, per-user play history, the last session's delivery, and per-game
// stream settings. The paths are proposals, not contract: until the control
// plane serves them every read below degrades to "nothing known" and the page
// shows what the catalogue already holds.

import { useEffect, useState } from "react";
import { apiFetch } from "../../../api/client";
import { useAuth } from "../../../auth/context";

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

let cache: Promise<ExtrasMap> | null = null;

/** Fetched once per page; `null` until it lands, `{}` when the server has none. */
export function useGameExtras(): ExtrasMap | null {
  const { token } = useAuth();
  const [data, setData] = useState<ExtrasMap | null>(null);
  useEffect(() => {
    if (!token) return;
    cache ??= apiFetch<ExtrasMap>("/lab/extras", { token }).catch(() => ({}));
    let live = true;
    void cache.then((d) => live && setData(d));
    return () => {
      live = false;
    };
  }, [token]);
  return data;
}

/** Drop the cached extras after a launch moves "last played". */
export function invalidateGameExtras(): void {
  cache = null;
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

export function getGameSettings(token: string, appId: string): Promise<GameSettings | null> {
  return apiFetch<GameSettings>(`/lab/game-settings/${encodeURIComponent(appId)}`, { token }).catch(() => null);
}

export function putGameSettings(token: string, appId: string, s: GameSettings): Promise<GameSettings> {
  return apiFetch<GameSettings>(`/lab/game-settings/${encodeURIComponent(appId)}`, { method: "PUT", body: s, token });
}

export function clearGameSettings(token: string, appId: string): Promise<void> {
  return apiFetch<void>(`/lab/game-settings/${encodeURIComponent(appId)}`, { method: "DELETE", token });
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
