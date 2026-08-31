// Pure, DOM-free logic for the library browse surface, testable without a browser.
//
// Row detection compares each tile's measured `offsetTop` rather than assuming a
// column count, since the grid's `repeat(auto-fill, minmax(228px,1fr))` column
// count is responsive — tiles sharing a row share one offsetTop regardless of count.

import type { AppKind, SessionState } from "../../api/types";

/** Whether a session state counts as "active / live". Lives here (not a .tsx page)
 *  so homeData's DOM-free rail logic can import it too. */
export const LIVE_STATES: readonly SessionState[] = [
  "pending",
  "assigned",
  "starting",
  "running",
];

/** The library's single-select filter. Favourites is a fourth segment value
 *  rather than an orthogonal toggle (spec §3.4) since the control is single-select
 *  and a separate toggle has no mockup behind it. */
export type KindFilter = "all" | "favourite" | AppKind;

export interface FilterableApp {
  name: string;
  description?: string | null;
  kind: AppKind;
  /** Per-user favourite flag (AppListItem.favourite). Optional so non-App
   * fixtures and the styleguide can still use this helper. */
  favourite?: boolean;
}

/** All / Favourites / Games / Desktops filter + free-text search over name/description. */
export function filterLibraryApps<T extends FilterableApp>(
  apps: readonly T[],
  kind: KindFilter,
  search: string,
): T[] {
  const q = search.trim().toLowerCase();
  return apps.filter((a) => {
    if (kind === "favourite") {
      if (!a.favourite) return false;
    } else if (kind !== "all" && a.kind !== kind) {
      return false;
    }
    if (!q) return true;
    return (
      a.name.toLowerCase().includes(q) ||
      (a.description ?? "").toLowerCase().includes(q)
    );
  });
}

/** Sortable shape for the library's display order. */
export interface SortableApp {
  name: string;
  favourite?: boolean;
}

/** Display order: favourites first, then name within each group. Returns a new
 *  array — never mutates, since the caller also derives cover colour from a
 *  separate name-only ordering and the two must not share an array. */
export function sortLibraryApps<T extends SortableApp>(apps: readonly T[]): T[] {
  return [...apps].sort((a, b) => {
    const fa = a.favourite ? 0 : 1;
    const fb = b.favourite ? 0 : 1;
    if (fa !== fb) return fa - fb;
    return a.name.localeCompare(b.name);
  });
}

/**
 * Index (within `offsetTops`) of the LAST tile sharing `selectedIndex`'s row.
 * The detail band is inserted immediately after this tile so it reads as
 * that row opening, matching the reference's `last.after(detail)`.
 */
export function lastIndexInRow(offsetTops: readonly number[], selectedIndex: number): number {
  if (selectedIndex < 0 || selectedIndex >= offsetTops.length) return selectedIndex;
  const top = offsetTops[selectedIndex];
  let last = selectedIndex;
  for (let i = 0; i < offsetTops.length; i++) {
    if (offsetTops[i] === top) last = i;
  }
  return last;
}

/** Number of tiles sharing the first tile's row — the up/down jump stride. */
export function rowSize(offsetTops: readonly number[]): number {
  if (offsetTops.length === 0) return 0;
  const top = offsetTops[0];
  return offsetTops.filter((t) => t === top).length;
}

/** Cover gradient palette (primitives.css `.cv-*`), reused verbatim by
 *  SessionQuickSwitch's rail tiles so both derive colour from the same logic. */
export const COVER_CLASSES = [
  "cv-violet",
  "cv-cyan",
  "cv-horizon",
  "cv-nebula",
  "cv-plasma",
  "cv-rose",
] as const;

/** Deterministic index into `COVER_CLASSES`. Callers key it off name-sorted (not
 *  display-sorted) position so colour tracks the app, not its slot, across reorders. */
export function coverClassAt(index: number): string {
  return COVER_CLASSES[index % COVER_CLASSES.length];
}

export type NavKey = "ArrowLeft" | "ArrowRight" | "ArrowUp" | "ArrowDown";

/**
 * Grid-aware roving-focus target: left/right walk the flat tile list,
 * up/down jump by the first row's tile count. Returns null when the move
 * would go out of bounds (caller leaves focus where it is).
 */
export function nextTileIndex(
  offsetTops: readonly number[],
  currentIndex: number,
  key: NavKey,
): number | null {
  const perRow = rowSize(offsetTops);
  let to: number | null = null;
  if (key === "ArrowRight") to = currentIndex + 1;
  else if (key === "ArrowLeft") to = currentIndex - 1;
  else if (key === "ArrowDown") to = perRow > 0 ? currentIndex + perRow : null;
  else if (key === "ArrowUp") to = perRow > 0 ? currentIndex - perRow : null;
  if (to === null || to < 0 || to >= offsetTops.length) return null;
  return to;
}

/** "3 minutes" / "1h 12m" — coarse elapsed time for a running session. */
export function formatElapsedMinutes(startedAtIso: string, nowMs: number = Date.now()): string {
  const startMs = new Date(startedAtIso).getTime();
  const totalMinutes = Math.max(0, Math.floor((nowMs - startMs) / 60000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours === 0) return minutes === 1 ? "1 minute" : `${minutes} minutes`;
  return `${hours}h ${minutes}m`;
}

/** "2 days ago" / "3 hours ago" / "just now" — coarse relative time. */
export function formatRelativeTime(iso: string, nowMs: number = Date.now()): string {
  const thenMs = new Date(iso).getTime();
  const diffSec = Math.max(0, Math.floor((nowMs - thenMs) / 1000));
  if (diffSec < 60) return "just now";
  const diffMin = Math.floor(diffSec / 60);
  if (diffMin < 60) return diffMin === 1 ? "1 minute ago" : `${diffMin} minutes ago`;
  const diffHr = Math.floor(diffMin / 60);
  if (diffHr < 24) return diffHr === 1 ? "1 hour ago" : `${diffHr} hours ago`;
  const diffDay = Math.floor(diffHr / 24);
  return diffDay === 1 ? "1 day ago" : `${diffDay} days ago`;
}

// Derived tiles / single-writer lock UX (spec §2.2). A derived tile borrows its
// parent's home, so the single-writer guard keys on the home-owning app, not the
// tile (spec §2: homeAppID(a) = a.parent_app_id ?? a.id). This mirrors that identity
// only to PRESENT the lock (grey out family members before a 409) — never enforces
// it; the server's 409 home_in_use is (invariant #6).

/** Minimal shape needed to compute a tile's home-owning app. */
export interface HomeApp {
  id: string;
  parent_app_id: string | null;
}

/** Mirrors the control-plane's `homeAppID` exactly (spec §2). */
export function homeAppId(app: HomeApp): string {
  return app.parent_app_id ?? app.id;
}

/** Every other app sharing a home with the live session's app — i.e. every tile
 *  that home would refuse with `409 home_in_use` (spec §2.2). */
export function blockedFamilyIds<T extends HomeApp>(
  apps: readonly T[],
  liveAppId: string | null,
): Set<string> {
  const blocked = new Set<string>();
  if (!liveAppId) return blocked;
  const liveApp = apps.find((a) => a.id === liveAppId);
  if (!liveApp) return blocked;
  const homeId = homeAppId(liveApp);
  for (const a of apps) {
    if (a.id === liveAppId) continue;
    if (homeAppId(a) === homeId) blocked.add(a.id);
  }
  return blocked;
}

/** The family's home-owning app, used to NAME the tile in 409 copy — spec §2.2
 *  requires the user-visible name (e.g. "Steam"), never a bare parent app id. */
export function familyRootApp<T extends HomeApp>(apps: readonly T[], app: T): T | null {
  return apps.find((a) => a.id === homeAppId(app)) ?? null;
}

export interface LaunchErrorPresentation {
  variant: "danger" | "info";
  title: string;
  body: string;
  /** Present only for `home_in_use` with a nameable conflicting session (spec §2.2). */
  sessionId?: string;
}

/** Renders a failed launch for the toast. Covers the two 409s spec §2.2 names
 *  (`home_in_use`, `home_not_provisioned`) plus the generic fallback in one place. */
export function presentLaunchError<T extends HomeApp & { name: string }>(
  apps: readonly T[],
  app: T,
  code: string,
  message: string,
  sessionId?: string,
): LaunchErrorPresentation {
  const rootName = familyRootApp(apps, app)?.name ?? app.name;
  if (code === "home_in_use") {
    return {
      variant: "danger",
      title: `${rootName} is already running`,
      body: "Go to your session to continue playing.",
      sessionId,
    };
  }
  if (code === "home_not_provisioned") {
    return {
      variant: "info",
      title: "Library not set up yet",
      body: `Launch ${rootName} once on a host to set up your library, then try again.`,
    };
  }
  if (code === "capacity_unavailable") {
    return {
      variant: "danger",
      title: "Launch failed",
      body: "No host available right now — try again shortly.",
    };
  }
  // #525: fallback body must never be empty — a "" body renders a titled toast
  // that says nothing. Every other code uses its server-authored message.
  return {
    variant: "danger",
    title: "Launch failed",
    body: message || "The server refused this launch.",
  };
}
