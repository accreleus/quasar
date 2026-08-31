// Where a library tile came from — the Library view's grouping (v3 handoff
// §B "Library view"). Pure and DOM-free, like libraryGrid.ts; replaces
// homeData.ts's `groupAppsBySource`, which grouped by `external_source` alone
// and could not name a provider after the tile the titles were discovered
// through.
//
// The user read shape is the limit. `AppListItem` carries `kind`,
// `parent_app_id` and `external_source`; `origin` and `library_provider` are
// admin-only (control-api.md §Derived tiles), and no user-visible shape names
// a host — `GET /v1/hosts` is admin-only. So the mock's sub-line "via Steam
// launcher on host-01" ships as "via Steam": the half we can prove.
//
// Three buckets, in this order:
//   1. providers  — a tile discovered from a provider (parent_app_id set) or
//                   tagged with one (external_source set), named after the
//                   parent tile when the caller holds it, else after the
//                   provider id, else (neither) filed as Manual rather than
//                   heading a group with no name. Alphabetical.
//   2. Manual     — everything else, including the provider's own launcher
//                   tile: that app was added by hand, and only the titles
//                   found through it are the provider's.
//   3. Desktops   — kind === "desktop", whatever else it carries.

import type { AppKind } from "../../../api/types";

/** The minimum a tile has to expose to be filed. */
export interface GroupableApp {
  id: string;
  name: string;
  kind: AppKind;
  /** control-api.md: the app this tile was derived from, null for a normal app. */
  parent_app_id: string | null;
  /** Wire vocabulary is `"" | "steam"` (openapi `ExternalSource`), typed wide
   *  on purpose: an unknown provider must degrade, not fail to compile. */
  external_source?: string;
}

export interface SourceGroup<T> {
  /** Stable, DOM-safe — also the id of the group's heading element. */
  id: string;
  /** Which of the three buckets this is, independent of its title. */
  bucket: "provider" | "manual" | "desktops";
  /** The `.src-head` h3. */
  title: string;
  /** The `.s-sub` line beside it. */
  sub: string;
  apps: T[];
}

const MANUAL_TITLE = "Manual";
const DESKTOPS_TITLE = "Desktops";

/** Human label for a provider id. Wire `ExternalSource` is exactly
 *  `"" | "steam"`; an unknown id title-cases so a new provider renders
 *  without a code change. */
function providerTitle(source: string): string {
  if (source === "steam") return "Steam";
  return source.charAt(0).toUpperCase() + source.slice(1);
}

/** what a group is, as opposed to what it is called. Everything downstream —
 *  the order, the DOM id, the merge key — reads this and never the title, so a
 *  provider that happens to be named "Manual" is still a provider. */
type Bucket = "provider" | "manual" | "desktops";

const RANK: Record<Bucket, number> = { provider: 1, manual: 2, desktops: 3 };

/** `("provider", "Steam")` → `"src-provider-steam"`. Providers are namespaced
 *  so a provider id can never collide with a fixed bucket's. */
function groupId(bucket: Bucket, title: string): string {
  if (bucket !== "provider") return `src-${bucket}`;
  const slug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
  return `src-provider-${slug || "other"}`;
}

/**
 * Buckets `apps` for the Library view. Order inside a group is the caller's
 * (the page has already sorted favourites-first by name); empty groups are
 * never emitted.
 */
export function groupBySource<T extends GroupableApp>(apps: readonly T[]): SourceGroup<T>[] {
  const nameById = new Map(apps.map((a) => [a.id, a.name]));
  const groups = new Map<string, SourceGroup<T>>();

  for (const app of apps) {
    const source = (app.external_source ?? "").trim();
    // Named after the parent tile where possible: that is the name the user sees
    // on the launcher they already own ("Steam"), not a wire enum. With neither
    // the parent nor a provider id — a derived tile whose parent the caller
    // cannot see, on a pre-provider catalogue — the tile is Manual rather than
    // the head of a group with no name.
    const providerName =
      (app.parent_app_id ? nameById.get(app.parent_app_id) : undefined) ||
      (source !== "" ? providerTitle(source) : "");

    let bucket: Bucket;
    let title: string;
    let sub: string;
    if (app.kind === "desktop") {
      bucket = "desktops";
      title = DESKTOPS_TITLE;
      sub = "Full desktop environments";
    } else if (providerName !== "") {
      bucket = "provider";
      title = providerName;
      sub = `via ${title}`;
    } else {
      bucket = "manual";
      title = MANUAL_TITLE;
      sub = "Added to the catalogue directly";
    }

    // Keyed by bucket and title, so the two paths to one provider (a parent tile
    // and a bare `external_source`) merge, while a provider named "Manual" stays
    // its own group.
    const key = `${bucket}:${title}`;
    let group = groups.get(key);
    if (!group) {
      group = { id: groupId(bucket, title), title, sub, apps: [], bucket };
      groups.set(key, group);
    }
    group.apps.push(app);
  }

  return [...groups.values()].sort((a, b) => {
    if (a.bucket !== b.bucket) return RANK[a.bucket] - RANK[b.bucket];
    return a.title.localeCompare(b.title);
  });
}
