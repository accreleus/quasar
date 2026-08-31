// Pure toolbar filtering for the v3 Library > Apps tab (handoff §A.9). Kept
// free of React/fetching so the segment/search/source/preset interaction can
// be tested without mounting the page.

import type { AdminApp, LibraryUnpublishedItem } from "../../../api/types";

export type AppSegment = "all" | "games" | "desktops" | "disabled" | "pending";
export type AppSourceFilter = "all" | "steam" | "manual";

const APP_SEGMENTS: readonly AppSegment[] = ["all", "games", "desktops", "disabled", "pending"];
const APP_SOURCE_FILTERS: readonly AppSourceFilter[] = ["all", "steam", "manual"];

/** SourcesTab links to `?segment=` / `?source=` (handoff §A.9 <-> §A.15); an
 *  unrecognised or missing value falls back to "all" rather than erroring. */
export function parseAppSegment(value: string | null): AppSegment {
  return (APP_SEGMENTS as readonly string[]).includes(value ?? "") ? (value as AppSegment) : "all";
}

export function parseAppSourceFilter(value: string | null): AppSourceFilter {
  return (APP_SOURCE_FILTERS as readonly string[]).includes(value ?? "")
    ? (value as AppSourceFilter)
    : "all";
}

/** One discovered-but-unpublished title, paired with the provider app that
 *  owns it. GET .../apps/{id}/library/unpublished is scoped per provider app
 *  (there is no fleet-wide endpoint), so the tab fans that call out across
 *  every provider app and flattens the result into this shape. */
export interface PendingImportItem {
  providerAppId: string;
  item: LibraryUnpublishedItem;
}

/** Source column + filter (handoff §A.9): a reconciler-created tile
 *  (`parent_app_id` set) or the provider app itself (`external_source` set)
 *  is Steam; everything else is Manual. */
export function isSteamSourced(app: AdminApp): boolean {
  return Boolean(app.parent_app_id) || app.external_source === "steam";
}

export interface AppsFilterOptions {
  segment: AppSegment;
  q: string;
  source: AppSourceFilter;
  /** A runtime preset id from `?preset=`. */
  presetId?: string;
}

export interface FilteredApps {
  apps: AdminApp[];
  pending: PendingImportItem[];
}

/** A pending item carries no `kind`/`runtime_preset_id`/manual source, so it
 *  drops under Desktops, Disabled, a preset filter, or Manual source; it stays
 *  visible under All, Games (it renders with a Game chip) and Pending. */
export function filterApps(
  apps: readonly AdminApp[],
  pending: readonly PendingImportItem[],
  options: AppsFilterOptions,
): FilteredApps {
  const q = options.q.trim().toLowerCase();

  const filteredApps = apps.filter((a) => {
    if (options.presetId && a.runtime_preset_id !== options.presetId) return false;
    if (options.segment === "pending") return false;
    if (options.segment === "games" && a.kind !== "game") return false;
    if (options.segment === "desktops" && a.kind !== "desktop") return false;
    if (options.segment === "disabled" && a.enabled) return false;
    if (options.source === "steam" && !isSteamSourced(a)) return false;
    if (options.source === "manual" && isSteamSourced(a)) return false;
    if (q && !a.name.toLowerCase().includes(q)) return false;
    return true;
  });

  const showPending =
    !options.presetId &&
    options.source !== "manual" &&
    (options.segment === "all" || options.segment === "pending" || options.segment === "games");
  const filteredPending = showPending
    ? pending.filter((p) => !q || (p.item.name || p.item.external_id).toLowerCase().includes(q))
    : [];

  return { apps: filteredApps, pending: filteredPending };
}

export interface AppSegmentCounts {
  all: number;
  games: number;
  desktops: number;
  disabled: number;
  pending: number;
}

/** Unfiltered counts for the segmented control's badges — always against the
 *  full lists, never the current filter's result. */
export function segmentCounts(
  apps: readonly AdminApp[],
  pending: readonly PendingImportItem[],
): AppSegmentCounts {
  return {
    all: apps.length,
    games: apps.filter((a) => a.kind === "game").length,
    desktops: apps.filter((a) => a.kind === "desktop").length,
    disabled: apps.filter((a) => !a.enabled).length,
    pending: pending.length,
  };
}
