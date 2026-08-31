// Audit log filters (Task 28, handoff-v3-spec §A.20). Pure — no fetch, no React.

import type { ActivityQuery, AdminActivityItem } from "../../../api/admin";

export type AuditSegment = "all" | "operator" | "system" | "errors";
export type AuditRange = "24h" | "7d" | "30d";

const RANGE_MS: Record<AuditRange, number> = {
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
  "30d": 30 * 24 * 60 * 60 * 1000,
};

export function sinceFor(range: AuditRange, now: Date = new Date()): string {
  return new Date(now.getTime() - RANGE_MS[range]).toISOString();
}

/** `since`/`q` are the only server-side params — the API has no actor-null or
 *  severity filter (api-surface-map.md §1 Audit), so `segment` never reaches
 *  this function; it is applied client-side by `applySegment`/`segmentCounts`
 *  only. `now` is the caller's anchor — pass the same `Date` for the initial
 *  load and any cursor page of the same query, or the range's `since` drifts
 *  between pages and desyncs the cursor from what "Last 24 hours" meant when
 *  the user opened the page. */
export function queryFor(filters: { q: string; range: AuditRange }, now?: Date): ActivityQuery {
  const q = filters.q.trim();
  return { since: sinceFor(filters.range, now), q: q || undefined };
}

/** operator = has an actor; system = no actor (`actor_user_id` null); errors =
 *  `severity === "err"`. No API-side equivalent for any of the three. */
export function segmentPredicate(segment: AuditSegment): (item: AdminActivityItem) => boolean {
  switch (segment) {
    case "operator":
      return (item) => item.actor_user_id != null;
    case "system":
      return (item) => item.actor_user_id == null;
    case "errors":
      return (item) => item.severity === "err";
    default:
      return () => true;
  }
}

export function applySegment(
  items: AdminActivityItem[],
  segment: AuditSegment,
): AdminActivityItem[] {
  return items.filter(segmentPredicate(segment));
}

/** Counts per segment over the given rows — used for the toolbar's "Errors
 *  {n}" badge. Counts the currently loaded/queried set, not the visible
 *  (already segment-filtered) one, so switching segments never moves the
 *  other badges' numbers. */
export function segmentCounts(items: AdminActivityItem[]): Record<AuditSegment, number> {
  return {
    all: items.length,
    operator: items.filter(segmentPredicate("operator")).length,
    system: items.filter(segmentPredicate("system")).length,
    errors: items.filter(segmentPredicate("errors")).length,
  };
}

/** "actor_username or system" — the one label rule the row, the readout and
 *  the CSV export all share. */
export function actorLabel(item: Pick<AdminActivityItem, "actor_username">): string {
  return item.actor_username ?? "system";
}

/** "{target_type} {short id}" — short id omitted when there is none. */
export function targetLabel(item: Pick<AdminActivityItem, "target_type" | "target_id">): string {
  return item.target_id ? `${item.target_type} ${item.target_id.slice(0, 8)}` : item.target_type;
}
