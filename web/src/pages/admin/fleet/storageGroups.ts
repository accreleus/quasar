// Pure grouping/aggregation for the Fleet › Storage tab — no JSX, so the math
// is unit-testable without rendering (same split as sessions/sessionSort.ts).

import type { AdminHome } from "../../../api/types";
import type { ChipVariant } from "../../../components/Chip";

/** Synthetic grouping key for homes with no linked user (`user_id === null`).
 *  Real user ids are UUIDs and can never collide with this string. */
export const NO_USER_KEY = "__no_user__";

export interface StorageUserGroup {
  /** Grouping key: the user's uuid, or NO_USER_KEY. */
  key: string;
  userId: string | null;
  /** Null only for the NO_USER_KEY bucket: user_id and username go NULL
   *  together on user deletion (AdminHome doc, api/types.ts), so "user_id
   *  present but username missing" cannot occur. */
  username: string | null;
  /** This user's homes, sorted by bytes_used descending (biggest first). */
  homes: AdminHome[];
  totalBytes: number;
}

/** True when a home's app row is gone (ON DELETE SET NULL). The bytes are
 *  real and reclaimable, but nothing server-side still names the app. */
export function isAppOrphaned(home: AdminHome): boolean {
  return home.app_id == null;
}

/** True when a home's host row is gone. Same reasoning as isAppOrphaned. */
export function isHostOrphaned(home: AdminHome): boolean {
  return home.host_id == null;
}

/**
 * Groups homes by user, biggest consumer first (within groups too). The
 * NO_USER_KEY bucket is pinned last regardless of size — it must never
 * outrank a real person — but still counts toward the fleet summary tiles.
 */
export function groupHomesByUser(homes: AdminHome[]): StorageUserGroup[] {
  const byKey = new Map<string, StorageUserGroup>();
  for (const home of homes) {
    const key = home.user_id ?? NO_USER_KEY;
    let group = byKey.get(key);
    if (!group) {
      group = { key, userId: home.user_id, username: home.username, homes: [], totalBytes: 0 };
      byKey.set(key, group);
    }
    group.homes.push(home);
    group.totalBytes += home.bytes_used;
  }
  for (const group of byKey.values()) {
    group.homes.sort((a, b) => b.bytes_used - a.bytes_used);
  }
  const all = Array.from(byKey.values());
  const real = all.filter((g) => g.key !== NO_USER_KEY).sort((a, b) => b.totalBytes - a.totalBytes);
  const noUser = all.find((g) => g.key === NO_USER_KEY);
  return noUser ? [...real, noUser] : real;
}

export interface HomeState {
  label: string;
  variant: ChipVariant;
}

/**
 * One home's State chip (handoff-v3-spec §A.8): Active / Pending cleanup /
 * App deleted / Host deleted / No linked user. These can co-occur (an
 * orphaned home can also be pending cleanup) so precedence picks the single
 * most actionable fact — the one already-scheduled deletion outranks the
 * orphan facts, and "no linked user" (the group itself is the orphan) is
 * a stronger signal than a single missing app or host row.
 */
export function homeState(home: AdminHome): HomeState {
  if (home.gc_after) return { label: "Pending cleanup", variant: "warning" };
  if (home.user_id == null) return { label: "No linked user", variant: "neutral" };
  if (isAppOrphaned(home)) return { label: "App deleted", variant: "neutral" };
  if (isHostOrphaned(home)) return { label: "Host deleted", variant: "neutral" };
  return { label: "Active", variant: "success" };
}

/** Distinct `provider` values across a home list, sorted — feeds the
 *  toolbar's "All providers" filter select. */
export function distinctProviders(homes: AdminHome[]): string[] {
  return Array.from(new Set(homes.map((h) => h.provider))).sort();
}

/** Distinct non-null `host_id`s across a home list — "across {n} hosts" in
 *  the Managed homes KPI tile. */
export function distinctHostCount(homes: AdminHome[]): number {
  return new Set(homes.filter((h): h is AdminHome & { host_id: string } => h.host_id != null).map((h) => h.host_id))
    .size;
}
