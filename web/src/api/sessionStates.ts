/**
 * Session lifecycle states that count as "in flight".
 *
 * A session in any of these states is holding an encode slot on a host, so
 * these are the states that make up the "active sessions" count everywhere it
 * is shown (admin overview KPI, sessions filter, the admin rail's live marker).
 *
 * Kept in one place because the three call sites must agree: a session counted
 * as active by the rail but not by the Sessions page reads as a bug.
 */
export const ACTIVE_SESSION_STATES: ReadonlySet<string> = new Set([
  "pending",
  "assigned",
  "starting",
  "running",
  "stopping",
]);
