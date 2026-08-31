/**
 * The Overview's four KPI cards (spec §5.6, mock §A.1), derived from the two
 * shared polls plus the user list and the pending-invite count.
 *
 * Pure, and structurally typed for the same reason `deriveAlerts` is: a KPI
 * that needs a rendered page and a live token to test is a KPI nobody tests.
 *
 * Definitions that were open questions and are now settled here (spec §9):
 *   · a user is active when they are not disabled and a device of theirs was
 *     seen in the last 24 hours (`AdminUser.last_seen_at`, which is device
 *     activity, not "last request");
 *   · Streaming counts users holding at least one non-terminal session, and is
 *     a subset of active in the sense that matters: a disabled account is not
 *     someone the operator can act on, so it is excluded from both;
 *   · a host needs attention per `needsAttention` in ./deriveAlerts, so this
 *     card and the rail badge cannot disagree;
 *   · Mb/s out is the sum of the AGENT-reported `bitrate_kbps` — the browser's
 *     figure is one client's inbound and summing it would double-count.
 */

import { ACTIVE_SESSION_STATES } from "../../api/sessionStates";
import { isDegraded, needsAttention, type AttentionHost } from "./deriveAlerts";

const DAY_MS = 24 * 60 * 60 * 1000;

// ── Inputs (structural, not the generated API types) ─────────────────────────

export interface KpiHost extends AttentionHost {
  capacity?: {
    slots_total: number;
    slots_used: number;
  } | null;
}

export interface KpiSession {
  state: string;
  health_state?: string;
  latest_metrics?: {
    agent?: { metrics?: { bitrate_kbps?: number } };
  };
}

export interface KpiUser {
  disabled: boolean;
  last_seen_at?: string | null;
  active_session_count?: number;
}

export interface KpiInput {
  hosts: readonly KpiHost[];
  sessions: readonly KpiSession[];
  users: readonly KpiUser[];
  pendingInvites: number;
  now?: Date | number;
}

// ── Output ───────────────────────────────────────────────────────────────────

export interface Kpis {
  sessions: { live: number; degraded: number; mbpsOut: number };
  /** `capacityHosts` is how many hosts the slot figures were summed over, which
   *  is what the card's sub-line should count ("N free across M hosts") —
   *  `onlineHosts` can include a host that reports no capacity at all. */
  slots: { used: number; total: number; free: number; onlineHosts: number; capacityHosts: number };
  hosts: { online: number; total: number; attention: number };
  users: { active: number; streaming: number; pendingInvites: number };
}

export function deriveKpis({ hosts, sessions, users, pendingInvites, now = Date.now() }: KpiInput): Kpis {
  const nowMs = now instanceof Date ? now.getTime() : now;

  const live = sessions.filter((s) => ACTIVE_SESSION_STATES.has(s.state));
  const kbps = live.reduce((sum, s) => sum + (s.latest_metrics?.agent?.metrics?.bitrate_kbps ?? 0), 0);

  // An offline host's slots are not schedulable, and drawing them as free
  // capacity is the one way this card could actively mislead. Draining hosts
  // stay in: their used slots are real sessions still running.
  let used = 0;
  let total = 0;
  let capacityHosts = 0;
  for (const host of hosts) {
    if (host.status === "offline" || !host.capacity) continue;
    used += host.capacity.slots_used;
    total += host.capacity.slots_total;
    capacityHosts += 1;
  }

  return {
    sessions: {
      live: live.length,
      degraded: live.filter(isDegraded).length,
      mbpsOut: Math.round(kbps / 100) / 10,
    },
    slots: {
      used,
      total,
      free: Math.max(0, total - used),
      onlineHosts: hosts.filter((h) => h.status === "online").length,
      capacityHosts,
    },
    hosts: {
      online: hosts.filter((h) => h.status === "online").length,
      total: hosts.length,
      attention: hosts.filter(needsAttention).length,
    },
    users: {
      active: users.filter(
        (u) => !u.disabled && u.last_seen_at != null && nowMs - Date.parse(u.last_seen_at) <= DAY_MS,
      ).length,
      streaming: users.filter((u) => !u.disabled && (u.active_session_count ?? 0) > 0).length,
      pendingInvites,
    },
  };
}
