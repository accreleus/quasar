// Pure routing decision for the first-run wizard (Spec B W1). Kept separate
// from any component so it's trivially unit-testable: a virgin instance
// (no admin yet) must never show a login screen no one can satisfy, so any
// path other than /setup itself is redirected there.

import type { SetupStatus } from "../api/setup";

/**
 * True when the SPA should redirect `pathname` to `/setup`.
 * `status === null` means "not loaded yet" — never redirect on unknown state,
 * since that would bounce every route on first paint before the status
 * fetch resolves.
 */
export function shouldRouteToSetup(status: SetupStatus | null, pathname: string): boolean {
  if (!status) return false;
  if (status.admin_exists) return false;
  return pathname !== "/setup";
}
