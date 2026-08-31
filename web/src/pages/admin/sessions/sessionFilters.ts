/**
 * The Sessions toolbar's three narrowings, as pure functions (handoff §A.2).
 * The counts on the segmented control must come from the same rule that
 * filters the rows, or a count and its table disagree.
 */

import { ACTIVE_SESSION_STATES } from "../../../api/sessionStates";
import type { AdminSession } from "../../../api/types";

export type SessionSegment = "all" | "live" | "failed";

export interface SessionFilters {
  segment: SessionSegment;
  /** Free text over user, app and host. Trimmed and case-folded here. */
  q: string;
  /** A host id, or "" for every host. Matched exactly — a session with no host
   *  assigned yet matches no specific host, only "all". */
  hostId: string;
}

export interface SegmentCounts {
  all: number;
  live: number;
  failed: number;
}

function isLive(session: AdminSession): boolean {
  return ACTIVE_SESSION_STATES.has(session.state);
}

/**
 * `failed` is narrowed on the wire (`state=failed`) and must not be re-applied
 * here: `state === "failed"` is this client's guess at the server's
 * classification, and would drop any terminal failure state added later that
 * the wire filter still returns.
 */
function matchesSegment(session: AdminSession, segment: SessionSegment): boolean {
  if (segment === "live") return isLive(session);
  return true;
}

/** The three fields the search box promises to look at ("Filter by user, app or
 *  host"). Ids are deliberately not searched: a uuid prefix is not what an
 *  operator types into a box labelled with three names. */
function haystack(session: AdminSession): string {
  return [session.username, session.app_name, session.host_name]
    .filter((s): s is string => typeof s === "string" && s.length > 0)
    .join(" ")
    .toLowerCase();
}

export function filterSessions(
  rows: readonly AdminSession[],
  { segment, q, hostId }: SessionFilters,
): AdminSession[] {
  const needle = q.trim().toLowerCase();
  return rows.filter((session) => {
    if (!matchesSegment(session, segment)) return false;
    if (hostId && session.host_id !== hostId) return false;
    if (needle && !haystack(session).includes(needle)) return false;
    return true;
  });
}

/**
 * The numbers on the segmented control. Counted over the rows the page holds,
 * before the search box and the host select narrow them — the segment counts
 * describe the set you would switch to, not the set you are looking at.
 */
export function segmentCounts(rows: readonly AdminSession[]): SegmentCounts {
  let live = 0;
  let failed = 0;
  for (const session of rows) {
    if (isLive(session)) live += 1;
    if (session.state === "failed") failed += 1;
  }
  return { all: rows.length, live, failed };
}
