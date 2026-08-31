// Auth API calls (control-api.md §Auth). Pure functions over the shared client;
// state/storage lives in auth/AuthContext.

import { apiFetch } from "./client";
import type { LoginResponse, MeResponse, User } from "./types";
import type { DeviceCapabilities } from "../webrtc/capability";

/** Response body of POST /v1/me/devices (P4-08). */
export interface PostDeviceResponse {
  device: {
    id: string;
    first_seen_at: string;
    last_seen_at: string;
  };
}

/**
 * One device row from GET /v1/me/devices (LP-SEC-01 §B.6 — now a list). `current` marks
 * the device the bearer token is bound to; `active_session_id` is its live session, if any.
 * capabilities is the sanitized, server-stamped blob — typed loosely since best-effort.
 */
export interface Device {
  id: string;
  device_key: string;
  name: string | null;
  trusted: boolean;
  first_seen_at: string;
  last_seen_at: string;
  current: boolean;
  active_session_id: string | null;
  capabilities: Partial<DeviceCapabilities> & { measured_at?: string };
}

/** `GET /v1/me/devices` 200 body (LP-SEC-01). */
export interface DevicesResponse {
  devices: Device[];
}

/**
 * `POST /v1/auth/login` — exchange email+password for an opaque bearer token. When
 * deviceKey is supplied (LP-SEC-01 §B.5) the minted token is bound to that device so it
 * can later be revoked per-device.
 */
export function login(email: string, password: string, deviceKey?: string): Promise<LoginResponse> {
  const body: Record<string, unknown> = { email, password };
  if (deviceKey) body.device_key = deviceKey;
  return apiFetch<LoginResponse>("/auth/login", { body });
}

/**
 * `POST /v1/auth/register` — create an account (LP-SEC-01 SEC-03). `inviteCode` is required
 * when the instance is in `invite_only` mode; the web form reads it from the magic link's
 * `?invite=` query param. The role is never sent — it rides the admin-minted invite.
 * Errors: `registration_closed` (403), `invalid_invite` (400), `conflict` (409).
 */
export function register(
  email: string,
  username: string,
  password: string,
  inviteCode?: string,
): Promise<{ user: User }> {
  const body: Record<string, unknown> = { email, username, password };
  if (inviteCode) body.invite_code = inviteCode;
  return apiFetch<{ user: User }>("/auth/register", { body });
}

/** `POST /v1/auth/logout` — revoke the presented bearer token. Idempotent (204). */
export function logout(token: string): Promise<void> {
  return apiFetch<void>("/auth/logout", { method: "POST", token });
}

/** `GET /v1/me` — the authenticated user, including their authoritative role. */
export function getMe(token: string): Promise<MeResponse> {
  return apiFetch<MeResponse>("/me", { token });
}

/**
 * `POST /v1/me/devices` — upsert the caller's device capability (P4-08).
 * Owner is the bearer token identity; device_key is the client-generated UUID.
 * Fire-and-forget after login (best-effort; failure must not block sign-in).
 */
export function postDevice(
  token: string,
  deviceKey: string,
  capabilities: DeviceCapabilities,
): Promise<PostDeviceResponse> {
  return apiFetch<PostDeviceResponse>("/me/devices", {
    token,
    body: { device_key: deviceKey, capabilities },
  });
}

/**
 * `GET /v1/me/devices` — list the caller's devices, newest-first (LP-SEC-01 §B.6).
 * Owner is the bearer identity. Returns `{ devices: [] }` when the caller has none.
 */
export function listDevices(token: string, signal?: AbortSignal): Promise<DevicesResponse> {
  return apiFetch<DevicesResponse>("/me/devices", { token, signal });
}

/**
 * `PATCH /v1/me/devices/{id}` — rename / set the trust flag on one of the caller's
 * devices (LP-SEC-01 §B.6). Owner-scoped; 403 on another user's device.
 */
export function updateDevice(
  token: string,
  id: string,
  patch: { name?: string; trusted?: boolean },
): Promise<{ device: Device }> {
  return apiFetch<{ device: Device }>(`/me/devices/${id}`, { method: "PATCH", body: patch, token });
}

/**
 * `DELETE /v1/me/devices/{id}` — revoke a device (LP-SEC-01 §B.6). This invalidates that
 * device's bearer tokens (real revocation) and ends its live sessions. Owner-scoped (403
 * on another user's device); 204 on success.
 */
export function revokeDevice(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/me/devices/${id}`, { method: "DELETE", token });
}

/**
 * `POST /v1/me/password` — change the caller's password (CP-01).
 * Body: `{ current_password, new_password }`.
 * Returns 204 on success and revokes ALL the user's tokens (log out everywhere).
 * Throws ApiError with code "invalid_credentials" (401) or "validation_failed" (400).
 */
export function changePassword(
  token: string,
  currentPassword: string,
  newPassword: string,
): Promise<void> {
  return apiFetch<void>("/me/password", {
    method: "POST",
    token,
    body: { current_password: currentPassword, new_password: newPassword },
  });
}
