/**
 * The two metric dicts on an `AdminSession`, read through one accessor each.
 *
 * `latest_metrics` is a pair of untyped envelopes on the wire, so every reader
 * would otherwise repeat the same cast. Agent and browser are not
 * interchangeable: the agent reports what the host encoded (bitrate, encode
 * time), the browser what the player saw (fps, rtt, presentation). Summing the
 * browser's bitrate across sessions double-counts one client's inbound.
 */

import type { AdminSession, AgentMetrics, BrowserMetrics } from "../../api/types";

export function browserMetrics(session: AdminSession): BrowserMetrics | undefined {
  return session.latest_metrics?.browser?.metrics as BrowserMetrics | undefined;
}

export function agentMetrics(session: AdminSession): AgentMetrics | undefined {
  return session.latest_metrics?.agent?.metrics as AgentMetrics | undefined;
}
