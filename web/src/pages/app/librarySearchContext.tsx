// Bridges the home topbar's search field to AppHome's grid filter. The shell
// must not know what a library is, so AppLayout owns the query state and this
// context carries the value down to AppHome, its routed (not prop-tree) child.

import { createContext, useContext } from "react";

export interface LibrarySearchValue {
  query: string;
  setQuery: (query: string) => void;
}

export const LibrarySearchContext = createContext<LibrarySearchValue | null>(null);

/** Returns the shared query state, or a harmless local no-op pair when no
 * provider is mounted (e.g. a test rendering AppHome in isolation). */
export function useLibrarySearch(): LibrarySearchValue {
  const ctx = useContext(LibrarySearchContext);
  if (ctx) return ctx;
  return { query: "", setQuery: () => {} };
}
