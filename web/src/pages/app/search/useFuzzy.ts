// The fuzzy search's ranked filter for the grid.

import { useCallback, useMemo } from "react";
import type { App } from "../../../api/types";
import { useLibraryExtras, type ExtrasMap } from "../library/libraryExtras";
import { rank, type Recency } from "./match";

export function recencyOf(extras: ExtrasMap | null): Recency {
  return (id) => {
    const p = extras?.[id]?.play;
    return p ? Date.parse(p.last_played_at) : 0;
  };
}

/** The grid's filter: with a query, every match ranked best first. */
export function useFuzzyFilter() {
  const extras = useLibraryExtras();
  const recency = useMemo(() => recencyOf(extras), [extras]);
  return useCallback(
    <T extends App>(apps: readonly T[], q: string): T[] => (q.trim() ? rank(apps, q, recency) : [...apps]),
    [recency],
  );
}
