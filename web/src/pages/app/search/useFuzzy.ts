// Fuzzy search hooks: the ranked filter for the grid, and the "open this game's
// launcher" request the topbar popover and the palette send to the page below.

import { useCallback, useEffect, useMemo, useState } from "react";
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

// The shell sits above the routed page and cannot reach its detail band, so a
// chosen result is a request the page picks up — even one sent before a palette
// navigation mounted it.
export const OPEN_EVENT = "quasar:open-game";
const EVENT = OPEN_EVENT;
let pending: { id: string; at: number; q?: string } | null = null;

/** Wakes the page to look at `pending`. */
export function pokeOpenRequests(): void {
  window.dispatchEvent(new Event(EVENT));
}

/** Topbar: the page is already showing the list the game is in; open it now. */
export function requestOpen(appId: string): void {
  queueOpen(appId);
  pokeOpenRequests();
}

/** Palette: the navigation it is about to make changes the list; open the game
 *  when that new list lands, not in the list being left (whose view change
 *  would close the band straight away). `q` is the search the navigation
 *  seeds: the request waits until the box holds it. */
export function queueOpen(appId: string, q?: string): void {
  pending = { id: appId, at: Date.now(), q };
}

/** Opens a requested app's band once it is in the list on screen. */
export function useOpenRequests(apps: readonly App[], reveal: (app: App, expand: boolean) => void, search: string): void {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const on = () => setTick((t) => t + 1);
    window.addEventListener(EVENT, on);
    return () => window.removeEventListener(EVENT, on);
  }, []);
  useEffect(() => {
    if (!pending) return;
    if (Date.now() - pending.at > 3000) {
      pending = null; // stale: never land on a launcher nobody asked for just now
      return;
    }
    // A search the box has not taken yet means this list is about to be replaced
    // (and the band closed with it): wait for the list that query produces.
    if (pending.q !== undefined && pending.q !== search) return;
    const id = pending.id;
    const app = apps.find((a) => a.id === id);
    if (!app) return; // not filtered in yet: the next list change retries
    pending = null;
    reveal(app, false);
  }, [tick, apps, reveal, search]);
}
