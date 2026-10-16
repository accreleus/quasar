// The Ctrl+K palette's game search in the new design: the same matcher and the
// same result line as the topbar popover. Games lead when the query names one;
// a non-admin's chosen game opens its page.

import type { App } from "../../api/types";
import type { PaletteGroup, PaletteItem, PaletteSources } from "./paletteSearch";
import { searchPalette } from "./paletteSearch";
import type { ExtrasMap } from "../../pages/app/library/libraryExtras";
import { rank } from "../../pages/app/search/match";
import { metaFor } from "../../pages/app/search/FuzzySearch";
import { recencyOf } from "../../pages/app/search/useFuzzy";

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
    to: sources.isAdmin ? `/admin/library/apps/${a.id}` : `/app/game/${a.id}`,
  }));
  const groups = items.length ? [{ title: sources.isAdmin ? "Apps" : "Games", items }, ...rest] : rest;
  return groups.length ? groups : [{ title: "No matches", items: [] }];
}
