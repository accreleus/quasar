// Related titles for a game page, from the game extras (genres, developer, rank).

import type { App } from "../../../api/types";
import type { ExtrasMap } from "./gameData";

// Genres that say little about what a game is like.
const BROAD = new Set(["Indie", "Free To Play", "Early Access", "Casual", "Massively Multiplayer"]);

const rank = (x: ExtrasMap, id: string) => x[id]?.rank ?? Number.MAX_SAFE_INTEGER;

/** Games sharing the most genres with `app`, ties by popularity. */
export function moreLikeThis(apps: readonly App[], x: ExtrasMap, app: App, n = 6): App[] {
  const mine = new Set(x[app.id]?.genres ?? []);
  if (!mine.size) return [];
  return apps
    .filter((a) => a.kind === "game" && a.id !== app.id)
    .map((a) => ({ a, shared: (x[a.id]?.genres ?? []).filter((g) => mine.has(g) && !BROAD.has(g)).length }))
    .filter((r) => r.shared > 0)
    .sort((p, q) => q.shared - p.shared || rank(x, p.a.id) - rank(x, q.a.id))
    .slice(0, n)
    .map((r) => r.a);
}
