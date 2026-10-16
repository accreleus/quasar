// The Ctrl+K palette's game search in the new design: the same matcher and the
// same result line as the topbar popover. Games lead when the query names one;
// a non-admin's chosen game opens its launcher (runGamePick) on the library route
// it already used.

import { startTransition } from "react";
import type { App } from "../../api/types";
import type { PaletteGroup, PaletteItem, PaletteSources } from "./paletteSearch";
import { searchPalette } from "./paletteSearch";
import type { ExtrasMap } from "../../pages/app/library/libraryExtras";
import { rank } from "../../pages/app/search/match";
import { metaFor } from "../../pages/app/search/FuzzySearch";
import { pokeOpenRequests, queueOpen, recencyOf } from "../../pages/app/search/useFuzzy";

const CAP = 6;

export function searchPaletteFuzzy(query: string, sources: PaletteSources, extras: ExtrasMap | null): PaletteGroup[] {
  const q = query.trim();
  // Everything but apps stays the shipped search; apps are swapped for ranked hits.
  const rest = searchPalette(query, { ...sources, apps: [] }).filter((g) => g.title !== "No matches");
  if (!q) return rest;
  const hits = rank(sources.apps as readonly App[], q, recencyOf(extras)).slice(0, CAP);
  const items = hits.map<PaletteItem>((a) => ({
    id: `app-${a.id}`,
    label: a.name,
    meta: sources.isAdmin ? a.kind : metaFor(a, extras),
    icon: "library",
    to: sources.isAdmin ? `/admin/library/apps/${a.id}` : `/app/library?q=${encodeURIComponent(a.name)}`,
  }));
  const groups = items.length ? [{ title: sources.isAdmin ? "Apps" : "Games", items }, ...rest] : rest;
  return groups.length ? groups : [{ title: "No matches", items: [] }];
}

export const isGamePick = (item: PaletteItem, isAdmin: boolean) => !isAdmin && item.id.startsWith("app-");

/** A player's chosen game. The box is set to its name here, not only through
 *  `?q=` (which may not change); the open request names the box text and the
 *  app id, and the box, the navigation and the wake-up commit in one transition,
 *  so the request is never served from the list being left. */
export function runGamePick(item: PaletteItem, setQuery: (q: string) => void, go: () => void): void {
  queueOpen(item.id.slice(4), item.label);
  startTransition(() => {
    setQuery(item.label);
    go();
    pokeOpenRequests();
  });
}
