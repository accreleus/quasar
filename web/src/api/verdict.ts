// Client for the owner-scoped Verdict endpoint (ST-09).
//
// `GET /v1/sessions/{id}/verdict` answers for the caller's OWN session. The
// admin twin under /v1/admin/... is a different route with the same body; this
// module deliberately only knows the owner one, because the diagnostics panel a
// player opens must never be the thing that reaches for an admin surface.
//
// Observability only: reading a verdict never changes a session.

import { apiFetch } from "./client";
import type { components } from "./schema";

export type Falsifier = components["schemas"]["Falsifier"];

/**
 * The Verdict, with `verdict` widened to `string`.
 *
 * The generated type carries today's enum, and the contract is explicit that
 * the control plane grows this vocabulary and that an unknown value is DATA to
 * a consumer. Typing it as the enum would make a newer control plane a type
 * error at build time and — worse — invite a `switch` with no default. The
 * panel renders whatever string arrives.
 */
export type Verdict = Omit<components["schemas"]["Verdict"], "verdict"> & {
  verdict: string;
};

/** A verdict state the panel should colour as a warning rather than a fact. */
export function isLikelyState(verdict: string): boolean {
  return verdict.startsWith("likely_");
}

/** `GET /v1/sessions/{id}/verdict` — owner or admin. 403 for anyone else. */
export function getSessionVerdict(
  token: string,
  sessionId: string,
  signal?: AbortSignal,
): Promise<Verdict> {
  return apiFetch<Verdict>(`/sessions/${sessionId}/verdict`, { token, signal });
}
