// Sort and genre state for the library section. Called unconditionally by the
// page (hooks cannot branch on the design); only the new design reads it.

import { useCallback, useMemo, useState } from "react";
import type { App } from "../../../api/types";
import { useLibraryExtras } from "./libraryExtras";
import { SORTS, arrangeApps, metaLine, topGenres, type SortMode } from "./organise";

const SORT_KEY = "quasar.library.sort";

function readSort(): SortMode {
  try {
    const v = localStorage.getItem(SORT_KEY);
    return SORTS.some((s) => s.value === v) ? (v as SortMode) : "az";
  } catch {
    return "az";
  }
}

export function useOrganised() {
  const extras = useLibraryExtras();
  const [stored, setStored] = useState<SortMode>(readSort);
  const [genre, setGenre] = useState<string | null>(null);
  // Without play/added data only A–Z means anything: a remembered "Most played"
  // must not silently reorder nothing.
  const sort: SortMode = extras ? stored : "az";
  const sorts = useMemo(() => (extras ? SORTS : SORTS.slice(0, 1)), [extras]);
  const setSort = useCallback((s: SortMode) => {
    setStored(s);
    try {
      localStorage.setItem(SORT_KEY, s);
    } catch {
      /* private mode: the choice just does not persist */
    }
  }, []);
  const arrange = useCallback((apps: readonly App[]) => arrangeApps(apps, sort, genre, extras), [sort, genre, extras]);
  const metaFor = useCallback((app: App) => metaLine(app, sort, extras), [sort, extras]);
  const genres = useMemo(() => topGenres(extras), [extras]);
  return { sort, sorts, setSort, genre, setGenre, genres, arrange, metaFor, extras };
}

export type Organised = ReturnType<typeof useOrganised>;
