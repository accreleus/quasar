/**
 * What the Overview needs beyond the shared fleet polls: users, pending
 * invites, recent failures, the audit tail.
 *
 * Hosts and live sessions are absent on purpose — they come from
 * `useFleetContext()`, so this page adds no second poll of either. One
 * resource rather than four: they are read together, and none of them changes
 * on a streaming timescale.
 */

import * as adminApi from "../../../api/admin";
import type { AdminActivityItem } from "../../../api/admin";
import type { AdminSession, AdminUser } from "../../../api/types";
import { useResource } from "../../../lib/resource/react";

/** Users, invites and the audit tail move in minutes, not seconds. */
export const OVERVIEW_POLL_MS = 30_000;

/** How far back a failure is still news, matching `deriveAlerts`' window. This
 *  copy only bounds the list; the rule re-applies the cut against the render
 *  clock, so a row still leaves the card on time between polls. */
const RECENT_FAILURE_WINDOW_MS = 15 * 60 * 1000;

/** Failed sessions asked for per poll; see the call site. */
const FAILED_PAGE = 50;

/** Rows the Recent activity card shows (mock §A.1). */
export const ACTIVITY_ROWS = 6;

export interface OverviewData {
  users: AdminUser[];
  pendingInvites: number;
  /** `state=failed`, first page, cut to the last 15 minutes. */
  recentFailed: AdminSession[];
  activity: AdminActivityItem[];
}

export interface OverviewResource {
  data: OverviewData;
  /** True only before the first load — a poll never flashes the page. */
  loading: boolean;
  error: string | null;
  /** Epoch ms of the last applied load; the stamp its sparkline samples on. */
  lastFetchedAt: number | null;
  /** Refresh now. Never rejects; a failure lands in `error`. */
  reload: () => Promise<void>;
}

const EMPTY: OverviewData = { users: [], pendingInvites: 0, recentFailed: [], activity: [] };

export function useOverviewData(): OverviewResource {
  const resource = useResource<OverviewData>(
    {
      label: "overview",
      pollMs: OVERVIEW_POLL_MS,
      initialData: EMPTY,
      fetch: async ({ token }) => {
        const [users, invites, failed, activity] = await Promise.all([
          adminApi.listUsers(token),
          adminApi.listInvites(token, { state: "pending" }),
          // One page, newest first: only the last 15 minutes are shown, and a
          // burst of more than this many failures in that window loses the
          // oldest of them here rather than paging for rows about to be cut.
          adminApi.listAllSessions(token, undefined, { state: "failed", limit: FAILED_PAGE }),
          adminApi.listAdminActivity(token, { limit: ACTIVITY_ROWS }),
        ]);
        return {
          users: users.items,
          pendingInvites: invites.invites.length,
          recentFailed: recentlyFailed(failed.items ?? [], Date.now()),
          activity: activity.items,
        };
      },
    },
    [],
  );

  return {
    data: resource.data ?? EMPTY,
    loading: resource.loading,
    error: resource.errorMessage,
    lastFetchedAt: resource.updatedAt,
    reload: async () => {
      await resource.refresh();
    },
  };
}

/** The failures worth showing on a dashboard. A session with no timestamp is
 *  dropped: a failure from an unknown time cannot be claimed to be recent. */
export function recentlyFailed(sessions: AdminSession[], nowMs: number): AdminSession[] {
  return sessions.filter((session) => {
    const at = session.ended_at ?? session.created_at;
    const ms = at ? Date.parse(at) : Number.NaN;
    return Number.isFinite(ms) && nowMs - ms <= RECENT_FAILURE_WINDOW_MS;
  });
}
