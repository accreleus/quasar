// Client for the caller-scoped /v1/me/* preference endpoints.
//
// These carry the WIRE shape (snake_case) and nothing else. Conversion to the
// camelCase domain model happens in exactly one place —
// settings/overlayPreferences.ts — so there is never a second, subtly different
// interpretation of the same field.

import { apiFetch } from "./client";
import type { components } from "./schema";

export type SessionOverlayWire = components["schemas"]["SessionOverlayPreferences"];
export type UIPreferencesWire = components["schemas"]["UIPreferences"];

/** `GET /v1/me/ui-preferences` — the caller's own preferences. Never another user's. */
export function getUIPreferences(token: string): Promise<UIPreferencesWire> {
  return apiFetch<UIPreferencesWire>("/me/ui-preferences", { token });
}

/**
 * `PATCH /v1/me/ui-preferences` — partial, one level deep. Send only the fields
 * that changed; absent fields are left alone server-side.
 */
export function patchUIPreferences(
  token: string,
  patch: UIPreferencesWire,
): Promise<UIPreferencesWire> {
  return apiFetch<UIPreferencesWire>("/me/ui-preferences", {
    method: "PATCH",
    body: patch,
    token,
  });
}
