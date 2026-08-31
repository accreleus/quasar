// The home page's keyboard model, on one document listener.
//
// Exactly one Escape handler, resolving most-nested-first (options panel, then
// detail band) — do not add another; competing handlers previously collapsed
// the band under the modal and stole focus back. Arrows rove over the rail and
// the grid as one model (rovingFocus.ts); `P` launches through the same
// eligibility gate as the play control; `/` focuses the topbar search.

import { useEffect } from "react";
import type { RefObject } from "react";
import type { App } from "../../../api/types";
import type { RailCard } from "../homeData";
import { dirForKey } from "../roving";
import { moveRovingFocus } from "./rovingFocus";

export interface UseHomeKeysOptions {
  apps: readonly App[];
  featured: readonly RailCard[];
  blockedIds: ReadonlySet<string>;
  railTrackRef: RefObject<HTMLElement>;
  gridsRef: RefObject<HTMLElement>;
  /** Whether the band's launch-options panel is open (Escape's first target). */
  optionsOpen: boolean;
  /** Whether a detail band is open at all (Escape's second target). */
  bandOpen: boolean;
  smoothScroll: boolean;
  onCloseOptions: () => void;
  onCloseBand: () => void;
  onLaunch: (app: App) => void;
  onResume: (sessionId: string) => void;
}

export function useHomeKeys({
  apps,
  featured,
  blockedIds,
  railTrackRef,
  gridsRef,
  optionsOpen,
  bandOpen,
  smoothScroll,
  onCloseOptions,
  onCloseBand,
  onLaunch,
  onResume,
}: UseHomeKeysOptions): void {
  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") {
        if (optionsOpen) {
          onCloseOptions();
          return;
        }
        if (bandOpen) onCloseBand();
        return;
      }
      const activeEl = document.activeElement as HTMLElement | null;
      const activeTag = activeEl?.tagName;
      if (e.key === "/" && activeTag !== "INPUT" && activeTag !== "TEXTAREA") {
        e.preventDefault();
        document.querySelector<HTMLInputElement>(".topbar .search input")?.focus();
        return;
      }
      const onTile = activeEl?.classList.contains("lib-tile-surface") ?? false;
      const onRailCard = activeEl?.classList.contains("home-feat-surface") ?? false;
      if (!activeEl || (!onTile && !onRailCard)) return;

      // `P` fires on the rail too: its play control is `tabIndex={-1}` like the
      // tile's (surface + play button both as tab stops doubles the cost of
      // walking the page), making `P` the only keyboard route to it.
      //
      // Dispatches on the card, not the app: a live card resumes, every other
      // card launches — resolving only the app would turn resume into launch
      // (the 409 the single-writer lock exists to prevent).
      if (e.key.toLowerCase() === "p") {
        e.preventDefault();
        const appId = activeEl.getAttribute("data-app-id");
        const app = appId ? apps.find((a) => a.id === appId) : undefined;
        if (!app) return;
        if (onRailCard) {
          const card = featured.find((c) => c.appId === app.id);
          if (card?.sessionId) {
            onResume(card.sessionId);
            return;
          }
          // A card for an app blocked by the live session's home has a disabled
          // play button; the shortcut must not be a way around it.
          if (blockedIds.has(app.id)) return;
        }
        onLaunch(app);
        return;
      }

      // Arrows: nothing but the key→direction map onto the roving model.
      // `preventDefault` only when focus actually moved, so an arrow at the
      // model's edge still scrolls the page.
      const dir = dirForKey(e.key);
      if (!dir) return;
      const moved = moveRovingFocus(dir, {
        railTrack: railTrackRef.current,
        grids: gridsRef.current,
        smooth: smoothScroll,
      });
      if (moved) e.preventDefault();
    }
    document.addEventListener("keydown", handleKeyDown);
    return () => document.removeEventListener("keydown", handleKeyDown);
  }, [
    apps,
    featured,
    blockedIds,
    railTrackRef,
    gridsRef,
    optionsOpen,
    bandOpen,
    smoothScroll,
    onCloseOptions,
    onCloseBand,
    onLaunch,
    onResume,
  ]);
}
