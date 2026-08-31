// The inline detail band's state: which tile is open, which tile the band is
// anchored after, whether the launch options are expanded, and the launch
// profiles being evaluated for it. DetailBand renders it; this owns when it
// opens, where it opens, and what it is fed.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App, ProfilesResponse } from "../../../api/types";
import { lastIndexInRow } from "../libraryGrid";
import { tileBoxOf } from "./LibraryTile";

interface BandProfiles {
  appId: string;
  data: ProfilesResponse | string | null;
}

export interface UseDetailBandOptions {
  token: string | null;
  /** The grids currently on screen, in DOM order: one flat list in Home view,
   *  one per source group in Library view. A band opens inside the list its
   *  tile actually lives in. */
  visibleLists: readonly (readonly App[])[];
  /** Clears the kind filter so a rail card for a filtered-out app can still be
   *  revealed. */
  onClearFilter: () => void;
  /** Whether to animate the scroll that brings the band into view. */
  smoothScroll: boolean;
}

export function useDetailBand({
  token,
  visibleLists,
  onClearFilter,
  smoothScroll,
}: UseDetailBandOptions) {
  const [openId, setOpenId] = useState<string | null>(null);
  const [insertAfterId, setInsertAfterId] = useState<string | null>(null);
  const [optionsOpen, setOptionsOpen] = useState(false);
  const [bandProfiles, setBandProfiles] = useState<BandProfiles | null>(null);
  const [pendingOptions, setPendingOptions] = useState<{ id: string; expand: boolean } | null>(null);

  const tileRefs = useRef<Map<string, HTMLButtonElement>>(new Map());
  const detailRef = useRef<HTMLDivElement>(null);
  const optionsToggleRef = useRef<HTMLButtonElement>(null);
  const profilesForRef = useRef<string | null>(null);

  const fetchProfiles = useCallback(
    async (app: App): Promise<ProfilesResponse | null> => {
      if (!token) return null;
      profilesForRef.current = app.id;
      setBandProfiles({ appId: app.id, data: null });
      try {
        const data = await libraryApi.getProfiles(token, app.id);
        setBandProfiles((prev) => (prev?.appId === app.id ? { appId: app.id, data } : prev));
        return data;
      } catch (err: unknown) {
        const msg = err instanceof ApiError ? err.message : "could not load stream profiles";
        setBandProfiles((prev) => (prev?.appId === app.id ? { appId: app.id, data: msg } : prev));
        return null;
      }
    },
    [token],
  );

  const ensureProfiles = useCallback(
    (app: App) => {
      if (profilesForRef.current === app.id) return;
      void fetchProfiles(app);
    },
    [fetchProfiles],
  );

  const close = useCallback(() => {
    if (openId) tileRefs.current.get(openId)?.focus();
    setOpenId(null);
    setInsertAfterId(null);
    setOptionsOpen(false);
    setBandProfiles(null);
    profilesForRef.current = null;
  }, [openId]);

  /** Drops the band without moving focus — for a filter or view change, where
   *  the anchor row may no longer exist and nothing was dismissed by hand. */
  const reset = useCallback(() => {
    setOpenId(null);
    setInsertAfterId(null);
  }, []);

  /**
   * Opens `app`'s band anchored after the last tile in its row. `list` must be
   * the grid the tile actually lives in (the source group in Library view).
   * Row detection is by measured `offsetTop` (libraryGrid.lastIndexInRow) —
   * column-count agnostic.
   */
  const open = useCallback(
    (app: App, list: readonly App[]) => {
      if (openId === app.id) {
        close();
        return;
      }
      const ids = list.map((a) => a.id);
      const index = ids.indexOf(app.id);
      const offsetTops = ids.map((id) => tileBoxOf(tileRefs.current.get(id))?.offsetTop ?? 0);
      const lastIdx = index >= 0 ? lastIndexInRow(offsetTops, index) : -1;
      setOpenId(app.id);
      setInsertAfterId(ids[lastIdx] ?? app.id);
      setOptionsOpen(false);
      ensureProfiles(app);
    },
    [openId, close, ensureProfiles],
  );

  /** Find the currently-rendered list that contains `id`, or null. */
  const listContaining = useCallback(
    (id: string): readonly App[] | null =>
      visibleLists.find((l) => l.some((a) => a.id === id)) ?? null,
    [visibleLists],
  );

  /**
   * Opens a band for an app that may not be in the current view (rail card, or
   * a gated-launch warning). Resets the kind filter and defers if the tile is
   * not rendered yet. Deliberately does not clear the search query.
   */
  const reveal = useCallback(
    (app: App, expandOptions: boolean) => {
      const list = listContaining(app.id);
      if (list) {
        if (openId !== app.id) open(app, list);
        if (expandOptions) setOptionsOpen(true);
        return;
      }
      onClearFilter();
      setPendingOptions({ id: app.id, expand: expandOptions });
    },
    [listContaining, openId, open, onClearFilter],
  );

  useEffect(() => {
    if (!pendingOptions) return;
    const list = listContaining(pendingOptions.id);
    const app = list?.find((a) => a.id === pendingOptions.id);
    const expand = pendingOptions.expand;
    setPendingOptions(null);
    if (!list || !app) return;
    if (openId !== app.id) open(app, list);
    if (expand) setOptionsOpen(true);
  }, [pendingOptions, listContaining, openId, open]);

  // The band is anchored after the last tile of its row, and a resize
  // recomposes the rows: without this the band sits mid-row until the next
  // click. Debounced 150ms, and only while one is open.
  useEffect(() => {
    if (!openId) return;
    if (typeof window === "undefined") return;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const onResize = () => {
      if (timer !== null) clearTimeout(timer);
      timer = setTimeout(() => {
        timer = null;
        const list = visibleLists.find((l) => l.some((a) => a.id === openId));
        if (!list) return;
        const ids = list.map((a) => a.id);
        const index = ids.indexOf(openId);
        const offsetTops = ids.map((id) => tileBoxOf(tileRefs.current.get(id))?.offsetTop ?? 0);
        setInsertAfterId(ids[lastIndexInRow(offsetTops, index)] ?? openId);
      }, 150);
    };
    window.addEventListener("resize", onResize);
    return () => {
      if (timer !== null) clearTimeout(timer);
      window.removeEventListener("resize", onResize);
    };
  }, [openId, visibleLists]);

  useEffect(() => {
    if (!openId) return;
    detailRef.current?.scrollIntoView?.({
      block: "nearest",
      behavior: smoothScroll ? "smooth" : "auto",
    });
  }, [openId, insertAfterId, smoothScroll]);

  const setTileRef = useCallback(
    (id: string) => (el: HTMLButtonElement | null) => {
      if (el) tileRefs.current.set(id, el);
      else tileRefs.current.delete(id);
    },
    [],
  );

  /** null = evaluation in flight, string = error message, object = loaded. */
  const profilesFor = useCallback(
    (appId: string) => (bandProfiles?.appId === appId ? bandProfiles.data : null),
    [bandProfiles],
  );
  const focusTile = useCallback((id: string) => tileRefs.current.get(id)?.focus(), []);

  // Memoised: the page hands this object to `useCallback` dependency lists, and
  // a fresh object every render would make every one of them inert.
  return useMemo(
    () => ({
      openId,
      insertAfterId,
      optionsOpen,
      setOptionsOpen,
      profilesFor,
      detailRef,
      optionsToggleRef,
      setTileRef,
      focusTile,
      fetchProfiles,
      open,
      close,
      reset,
      reveal,
    }),
    [
      openId,
      insertAfterId,
      optionsOpen,
      profilesFor,
      setTileRef,
      focusTile,
      fetchProfiles,
      open,
      close,
      reset,
      reveal,
    ],
  );
}
