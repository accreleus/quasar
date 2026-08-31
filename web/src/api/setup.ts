// First-run setup API calls (Spec B W1 — control-api.md "First-run setup"
// amendment). Both routes are unauthenticated: `/v1/setup/status` always, and
// `/v1/setup/claim` authenticates via the per-boot X-Quasar-Setup-Token header
// rather than a bearer token, and self-disables (409) once any admin exists.

import { apiFetch } from "./client";
import type { LoginResponse } from "./types";

/** `GET /v1/setup/status` 200 body. Deliberately minimal — routing only. */
export interface SetupStatus {
  admin_exists: boolean;
  setup_completed: boolean;
}

/** `GET /v1/setup/status` — lets the SPA route a virgin instance to /setup
 * instead of an unsatisfiable login screen. No auth; leaks nothing else. */
export function getSetupStatus(): Promise<SetupStatus> {
  return apiFetch<SetupStatus>("/setup/status");
}

export interface SetupClaimRequest {
  email: string;
  username: string;
  password: string;
  /** Per-device key (LP-SEC-01 §B.5, same value login sends) so the founding
   * admin's first token is device-bound; a pre-field server mints unbound. */
  device_key?: string;
}

/**
 * `POST /v1/setup/claim` — create the first admin. Auth is the per-boot
 * setup token (container log at WARN / `/run/quasar/setup-token`), sent as
 * `X-Quasar-Setup-Token`, never as a bearer token. The response is
 * login-shaped (201 -> LoginResponse); the caller signs the operator in
 * exactly as `auth/login` does. Throws `ApiError` — 401 on a wrong/missing
 * token (indistinguishable), 409 `setup_already_complete` once an admin
 * exists, 400 on a weak password.
 */
export function claimSetup(setupToken: string, body: SetupClaimRequest): Promise<LoginResponse> {
  return apiFetch<LoginResponse>("/setup/claim", {
    method: "POST",
    body,
    headers: { "X-Quasar-Setup-Token": setupToken },
  });
}

/**
 * `POST /v1/setup/complete` — RequireAuth → RequireAdmin, idempotent. Sets
 * setup_completed_at: completion is instance state, so a skip is permanent for
 * every admin on every device (step position stays client-side, setup/progress.ts).
 */
export function completeSetup(token: string): Promise<SetupStatus> {
  return apiFetch<SetupStatus>("/setup/complete", { method: "POST", token });
}
