/**
 * "Needs attention" — the Overview card (spec §5.6, mock §A.1), derived rather
 * than stored.
 *
 * There is no alerts entity on the control plane and this deliberately does
 * not ask for one: every fact below is already served by `GET /v1/hosts` and
 * `GET /v1/admin/sessions`, so an alert is a reading of the fleet at a moment,
 * not a row someone has to remember to clear. Nothing here has a lifecycle,
 * an ack, or a chance to go stale against the thing it describes.
 *
 * Pure by construction: no React, no fetch, no clock. `now` is passed in so
 * one poll renders one consistent set of ages.
 *
 * The rules, in the order they are emitted (the sort below then puts criticals
 * first, and within a severity the rows that know their onset newest-first):
 *
 * | # | Condition                                    | Severity | Onset            | CTA             |
 * |---|----------------------------------------------|----------|------------------|-----------------|
 * | 1 | any readiness check `status: "fail"`          | critical | readiness report | Open host       |
 * | 2 | `capacity_detection != "ok"`                  | critical | none             | Open host       |
 * | 3 | `status == "offline"`                         | critical | last heartbeat   | Open host       |
 * | 4 | sessions with a non-healthy `health_state`    | warning  | session start    | Open session(s) |
 * | 5 | a session `failed` in the last 15 minutes     | warning  | failure          | Open session    |
 * | 6 | a storage volume under 10% free               | warning  | none             | Open storage    |
 *
 * `draining` is not a rule and is not `needsAttention` either: an operator
 * drained that host on purpose, and a fault marker for the thing you just did
 * is how a fault marker gets ignored.
 *
 * Rules 2 and 6 have no onset the API can give us — a capacity report and a
 * disk usage figure are levels, not events — so they carry `since: null` and
 * render their age as a dash rather than inventing "just now" every poll.
 *
 * Rule 5 fires for any failed session it is given. The shared live poll
 * (`useLiveSessions`) carries only non-terminal states, so a page that wants
 * recent failures has to fetch them and pass them in.
 */

import { bytesFromMb } from "../format/bytes";
import { elapsedWords, relativeTime } from "../format/relativeTime";

// ── Inputs (structural, not the generated API types) ─────────────────────────
//
// Minimal shapes with open `string` fields, for the same reason
// `components/shell/paletteSearch.ts` uses them: this module must be testable
// from a four-line fixture, and `status`/`health_state` are open vocabularies
// server-side that a consumer is required to pass through rather than reject.

/** One agent-reported readiness check. `id` is optional here only so a fixture
 *  can leave it out; the wire always carries it. */
export interface ReadinessLike {
  id?: string;
  status: string;
  summary: string;
}

export interface AttentionHost {
  status: string;
  capacity_detection: string;
  readiness?: readonly ReadinessLike[] | null;
}

export interface AlertHost extends AttentionHost {
  id: string;
  node_name: string;
  capacity_reason?: string | null;
  storage?: readonly { label: string; total_mb: number; available_mb: number }[] | null;
  last_heartbeat_at?: string | null;
  readiness_reported_at?: string | null;
}

export interface AlertSession {
  id: string;
  state: string;
  host_id?: string | null;
  app_name?: string;
  username?: string;
  health_state?: string;
  health_reason?: string;
  failure_code?: string | null;
  state_detail?: string | null;
  started_at?: string | null;
  created_at?: string;
  ended_at?: string | null;
}

// ── Output ───────────────────────────────────────────────────────────────────

export type AlertSeverity = "critical" | "warning";

export interface Alert {
  /** Stable across polls while the condition holds — the React key. */
  id: string;
  severity: AlertSeverity;
  /** Bold first line. */
  title: string;
  /** Mono second line, truncated by the row. */
  body: string;
  /** ISO instant the condition started, or null when the fact has no onset. */
  since: string | null;
  /** `since` rendered for the row's age column; a dash when there is none. */
  age: string;
  /** Button label. */
  cta: string;
  to: string;
}

/** An alert before its age is attached — what each rule returns. */
type Finding = Omit<Alert, "age">;

/** What the age column shows for a condition with no onset (mock §A.1). */
const NO_AGE = "—";

/** A session counts as failed-recently for this long after it ended. */
const FAILED_WINDOW_MS = 15 * 60 * 1000;

/**
 * Free share below which a volume can no longer be trusted to provision homes.
 *
 * Exported because the Fleet row paints the same volume warning: two numbers
 * meant a row could sit amber while the alert list said the host was fine.
 */
export const LOW_STORAGE_FRACTION = 0.1;

/** The same threshold as a percentage, for callers holding a 0-100 figure. */
export const LOW_STORAGE_PCT = LOW_STORAGE_FRACTION * 100;

/**
 * Is this a host an operator would want to look at? (`status == offline ||
 * capacity_detection != ok || any readiness check failed`.) Behind the rail's
 * fault badge and the Hosts KPI's "N need attention", so those two can never
 * disagree — and it excludes `draining` for the same reason the rules do.
 */
export function needsAttention(host: AttentionHost): boolean {
  if (host.status === "offline") return true;
  if (host.capacity_detection !== "ok") return true;
  return (host.readiness ?? []).some((check) => check.status === "fail");
}

/** The API's health vocabulary has no literal "degraded" value — it names the
 *  specific degradation (`network_degrading`, `client_decode_degrading`, …).
 *  Anything reported that is not "healthy" is degraded for display. */
export function isDegraded(session: { health_state?: string }): boolean {
  return !!session.health_state && session.health_state !== "healthy";
}

export function deriveAlerts(
  hosts: readonly AlertHost[],
  sessions: readonly AlertSession[],
  now: Date | number = Date.now(),
): Alert[] {
  const nowMs = now instanceof Date ? now.getTime() : now;

  // Rule-major rather than host-major: the emitted order is the rule table
  // above, and the stable sort below preserves it through ties.
  const findings: Finding[] = [
    ...hosts.flatMap(readinessFindings),
    ...hosts.flatMap(capacityFindings),
    ...hosts.flatMap((h) => offlineFindings(h, sessions, nowMs)),
    ...degradedSessionFindings(sessions),
    ...sessions.flatMap((s) => failedSessionFindings(s, nowMs)),
    ...hosts.flatMap(storageFindings),
  ];

  return findings
    .sort(bySeverityThenNewest)
    .map((f) => ({ ...f, age: f.since ? relativeTime(f.since, nowMs) : NO_AGE }));
}

/** Criticals first; inside a severity, the rows that know when they started,
 *  newest first, then the ones that do not (in rule order — the sort is
 *  stable). */
function bySeverityThenNewest(a: Finding, b: Finding): number {
  const bySeverity = rank(a.severity) - rank(b.severity);
  if (bySeverity !== 0) return bySeverity;
  if (a.since && b.since) return Date.parse(b.since) - Date.parse(a.since);
  if (a.since) return -1;
  if (b.since) return 1;
  return 0;
}

function rank(severity: AlertSeverity): number {
  return severity === "critical" ? 0 : 1;
}

function hostRoute(host: AlertHost): string {
  return `/admin/fleet/hosts/${host.id}`;
}

/** An ISO instant, normalised, or null when there is nothing parseable. */
function instant(at: string | null | undefined): string | null {
  if (!at) return null;
  const ms = Date.parse(at);
  return Number.isFinite(ms) ? new Date(ms).toISOString() : null;
}

function readinessFindings(host: AlertHost): Finding[] {
  const failed = (host.readiness ?? []).filter((c) => c.status === "fail");
  if (!failed.length) return [];
  return [
    {
      id: `host:${host.id}:readiness`,
      severity: "critical",
      title:
        failed.length === 1
          ? `${host.node_name} failed a readiness check`
          : `${host.node_name} failed ${failed.length} readiness checks`,
      body: failed.map((c) => c.summary).join(" · "),
      since: instant(host.readiness_reported_at),
      cta: "Open host",
      to: hostRoute(host),
    },
  ];
}

function capacityFindings(host: AlertHost): Finding[] {
  if (host.capacity_detection === "ok") return [];
  return [
    {
      id: `host:${host.id}:capacity`,
      severity: "critical",
      title: `${host.node_name} is degraded`,
      body:
        host.capacity_reason ??
        `Capacity detection reported ${host.capacity_detection} · nothing can be scheduled here`,
      // A capacity report is a level, not an event: the wire says what it says
      // now, never since when.
      since: null,
      cta: "Open host",
      to: hostRoute(host),
    },
  ];
}

function offlineFindings(
  host: AlertHost,
  sessions: readonly AlertSession[],
  nowMs: number,
): Finding[] {
  if (host.status !== "offline") return [];
  const affected = sessions.filter((s) => s.host_id === host.id).length;
  const heartbeat = host.last_heartbeat_at
    ? `No heartbeat for ${elapsedWords(host.last_heartbeat_at, nowMs)}`
    : "No heartbeat recorded";
  return [
    {
      id: `host:${host.id}:offline`,
      severity: "critical",
      title: `${host.node_name} offline`,
      body: `${heartbeat} · ${affected} session${affected === 1 ? "" : "s"} affected`,
      since: instant(host.last_heartbeat_at),
      cta: "Open host",
      to: hostRoute(host),
    },
  ];
}

function degradedSessionFindings(sessions: readonly AlertSession[]): Finding[] {
  // Newest first, once: the same session then supplies both the body and the
  // onset, so the row cannot describe one session and be aged by another.
  const degraded = sessions
    .filter(isDegraded)
    .slice()
    .sort((a, b) => startedMs(b) - startedMs(a));
  if (!degraded.length) return [];
  const [newest] = degraded;
  const reason = newest.health_reason ?? newest.health_state ?? "health degraded";
  const detail = `${newest.app_name ?? "Unnamed app"} · ${newest.username ?? "unknown user"} · ${reason}`;
  const single = degraded.length === 1;
  return [
    {
      id: "sessions:degraded",
      severity: "warning",
      title: `${degraded.length} session${single ? "" : "s"} degraded`,
      body: single ? detail : `${detail} and ${degraded.length - 1} more`,
      // No health_changed_at exists on the wire, so this is how long the
      // newest degraded session has been running, not how long it has been ill.
      since: instant(newest.started_at ?? newest.created_at),
      cta: single ? "Open session" : "Open sessions",
      to: single ? `/admin/sessions/${newest.id}` : "/admin/sessions",
    },
  ];
}

/** Epoch ms a session started, or -Infinity when it will not say — which sorts
 *  it last rather than randomly. */
function startedMs(session: AlertSession): number {
  const at = session.started_at ?? session.created_at;
  const ms = at ? Date.parse(at) : Number.NaN;
  return Number.isFinite(ms) ? ms : Number.NEGATIVE_INFINITY;
}

function failedSessionFindings(session: AlertSession, nowMs: number): Finding[] {
  if (session.state !== "failed") return [];
  const at = session.ended_at ?? session.created_at;
  const ms = at ? Date.parse(at) : Number.NaN;
  // No timestamp means we cannot claim it is recent, and a failure from an
  // unknown time is not news.
  if (!Number.isFinite(ms) || nowMs - ms > FAILED_WINDOW_MS) return [];
  return [
    {
      id: `session:${session.id}:failed`,
      severity: "warning",
      title: `${session.app_name ?? "A session"} failed for ${session.username ?? "unknown user"}`,
      body: session.failure_code ?? session.state_detail ?? "No failure detail was reported",
      since: new Date(ms).toISOString(),
      cta: "Open session",
      to: `/admin/sessions/${session.id}`,
    },
  ];
}

function storageFindings(host: AlertHost): Finding[] {
  return (host.storage ?? [])
    .filter((v) => v.total_mb > 0 && v.available_mb / v.total_mb < LOW_STORAGE_FRACTION)
    .map((v) => ({
      id: `host:${host.id}:storage:${v.label}`,
      severity: "warning" as const,
      title: `Storage below ${LOW_STORAGE_PCT}% on ${host.node_name}`,
      body: `${bytesFromMb(v.available_mb)} free of ${bytesFromMb(v.total_mb)} · new homes will fail to provision`,
      // A free-space figure is a level, like capacity: true now, since unknown.
      since: null,
      cta: "Open storage",
      to: "/admin/fleet/storage",
    }));
}
