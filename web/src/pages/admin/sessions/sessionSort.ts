// UI audit #10 — pure client-side sort helper for the Sessions table.
// Extracted so it's independently testable; the page wires it to click-to-sort
// headers on Started / State / User (the highest-value columns per the audit).

import type { AdminSession } from "../../../api/types";

export type SessionSortKey = "started" | "state" | "user";
export type SortDir = "asc" | "desc";

function startedTime(s: AdminSession): number {
  const ts = s.started_at ?? s.created_at;
  return ts ? new Date(ts).getTime() : 0;
}

/**
 * #385 item 7: sort follows what is displayed — the key is always on-screen
 * text (username, or the id exactly when the cell itself falls back to it).
 * Mixed name/uuid ordering on deleted users is accepted over bucketing.
 */
function userSortValue(s: AdminSession): string {
  return s.username ?? s.user_id;
}

function compareBy(key: SessionSortKey, a: AdminSession, b: AdminSession): number {
  switch (key) {
    case "started":
      return startedTime(a) - startedTime(b);
    case "state":
      return a.state.localeCompare(b.state);
    case "user":
      return userSortValue(a).localeCompare(userSortValue(b));
    default:
      return 0;
  }
}

/**
 * Returns a new array sorted by `key`/`dir`. Does not mutate `rows`.
 * Pass `key: null` to get back the original order (default order preserved
 * when no sort is selected).
 */
export function sortSessions(
  rows: AdminSession[],
  key: SessionSortKey | null,
  dir: SortDir,
): AdminSession[] {
  if (!key) return rows;
  const sorted = [...rows].sort((a, b) => compareBy(key, a, b));
  return dir === "asc" ? sorted : sorted.reverse();
}
