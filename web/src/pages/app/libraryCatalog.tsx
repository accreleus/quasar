/**
 * The signed-in user's app catalogue (`GET /v1/apps`), loaded once for a whole
 * user area rather than once per consumer.
 *
 * Two things read it: the page that renders the library grid, and the shell's
 * command palette, which searches apps for everyone and routes a non-admin's
 * hit into /app/library?q= (spec §3.2). Mounted by the layout so the palette —
 * which lives above the routed page — can be handed the same list the page is
 * rendering, instead of opening a second read of the same endpoint.
 *
 * `setApps` is here because the favourite toggle is optimistic: it rewrites one
 * row before the PATCH lands. `useResource.setData` also discards any GET
 * already in flight, so a list read that predates the toggle cannot stomp it.
 */

import { createContext, useContext, useMemo, type ReactNode } from "react";
import * as libraryApi from "../../api/library";
import type { App } from "../../api/types";
import { useResource } from "../../lib/resource/react";

export interface LibraryCatalogValue {
  apps: App[];
  /** True only before the first load settles — a refresh never re-flashes it. */
  loading: boolean;
  error: string | null;
  reload: () => void;
  setApps: (updater: (apps: App[]) => App[]) => void;
}

const LibraryCatalogCtx = createContext<LibraryCatalogValue | null>(null);

export function LibraryCatalogProvider({ children }: { children: ReactNode }) {
  // The label is what a load failure reads as: "could not load library".
  const res = useResource({
    label: "library",
    initialData: [] as App[],
    fetch: async (ctx) => (await libraryApi.listApps(ctx.token)).items,
  });

  const value = useMemo<LibraryCatalogValue>(
    () => ({
      apps: res.data ?? [],
      loading: res.loading,
      error: res.errorMessage,
      reload: () => void res.refresh(),
      setApps: res.setData,
    }),
    [res.data, res.loading, res.errorMessage, res.refresh, res.setData],
  );

  return <LibraryCatalogCtx.Provider value={value}>{children}</LibraryCatalogCtx.Provider>;
}

export function useLibraryCatalog(): LibraryCatalogValue {
  const value = useContext(LibraryCatalogCtx);
  if (!value) throw new Error("useLibraryCatalog must be used inside <LibraryCatalogProvider>");
  return value;
}
