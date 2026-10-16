// The inline detail band's state: which tile is open, which tile the band is
// anchored after, whether the launch options are expanded, and the launch
// profiles being evaluated for it. DetailBand renders it; this owns when it
// opens, where it opens, and what it is fed.

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
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
  /** Identifies the filter, search and view the grids were built from. The
   *  band closes when this changes after it opened, not when it changes in the
   *  same commit that opens it (a rail card clears the filter and opens). */
  viewKey: string;
  /** Whether to animate the scroll that brings the band into view. */
  smoothScroll: boolean;
}

export function useDetailBand({
  token,
  visibleLists,
  onClearFilter,
  viewKey,
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
  const openedUnderRef = useRef(viewKey);

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

  // A filter or view change drops the band without moving focus: its anchor
  // row may no longer exist and nothing was dismissed by hand. `open` records
  // the key of the render that made it, so a band the reveal effect opens in
  // the same commit as the key change is not closed again.
  useEffect(() => {
    if (openedUnderRef.current === viewKey) return;
    openedUnderRef.current = viewKey;
    setOpenId(null);
    setInsertAfterId(null);
  }, [viewKey]);

  /** The last tile in `id`'s row, by measured `offsetTop` (libraryGrid.lastIndexInRow:
   *  column-count agnostic), or null while `id` has no box: a tile the windowed
   *  grid has not mounted yet. Unmounted tiles must not be measured as a row. */
  const rowEndOf = useCallback((list: readonly App[], id: string): string | null => {
    const ids = list.map((a) => a.id);
    const index = ids.indexOf(id);
    if (index < 0) return id;
    if (!tileBoxOf(tileRefs.current.get(id))) return null;
    const offsetTops = ids.map((i) => tileBoxOf(tileRefs.current.get(i))?.offsetTop ?? -1);
    return ids[lastIndexInRow(offsetTops, index)] ?? id;
  }, []);

  /**
   * Shows `app`'s band anchored after the last tile in its row, even if it is
   * already open. `list` must be the grid the tile actually lives in (the source group in Library view). A tile outside
   * the windowed grid's mounted rows (rail card, gated launch) is pinned by
   * `openId` first; the anchor is found once its row exists (below).
   */
  const show = useCallback(
    (app: App, list: readonly App[]) => {
      openedUnderRef.current = viewKey;
      setOpenId(app.id);
      setInsertAfterId(rowEndOf(list, app.id));
      setOptionsOpen(false);
      ensureProfiles(app);
    },
    [viewKey, ensureProfiles, rowEndOf],
  );

  /** A tile click: opens the band, or closes it when it is already open. */
  const open = useCallback(
    (app: App, list: readonly App[]) => {
      if (openId === app.id) close();
      else show(app, list);
    },
    [openId, close, show],
  );

  // A band opened for an unmounted tile has no anchor yet: its row mounted with
  // the render that set `openId`, so measure it before paint.
  useLayoutEffect(() => {
    if (!openId || insertAfterId) return;
    const list = visibleLists.find((l) => l.some((a) => a.id === openId));
    setInsertAfterId((list && rowEndOf(list, openId)) ?? openId);
  }, [openId, insertAfterId, visibleLists, rowEndOf]);

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
        show(app, list);
        if (expandOptions) setOptionsOpen(true);
        return;
      }
      onClearFilter();
      setPendingOptions({ id: app.id, expand: expandOptions });
    },
    [listContaining, show, onClearFilter],
  );

  useEffect(() => {
    if (!pendingOptions) return;
    const list = listContaining(pendingOptions.id);
    const app = list?.find((a) => a.id === pendingOptions.id);
    const expand = pendingOptions.expand;
    setPendingOptions(null);
    if (!list || !app) return;
    show(app, list);
    if (expand) setOptionsOpen(true);
  }, [pendingOptions, listContaining, show]);

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
        setInsertAfterId(rowEndOf(list, openId) ?? openId);
      }, 150);
    };
    window.addEventListener("resize", onResize);
    return () => {
      if (timer !== null) clearTimeout(timer);
      window.removeEventListener("resize", onResize);
    };
  }, [openId, visibleLists, rowEndOf]);

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
      reveal,
    ],
  );
}
