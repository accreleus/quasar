// Admin API calls. Every route here is admin-gated server-side by RequireAuth →
// RequireAdmin; hiding a call from a non-admin UI is not the access control.

import { ApiError, apiFetch } from "./client";
import type {
  AdminAppsResponse,
  AdminApp,
  CreateAppRequest,
  UpdateAppRequest,
  HostsResponse,
  GPUsResponse,
  AdminSessionsResponse,
  AdminUsersResponse,
  AdminUser,
  MetricsResponse,
  AdminHomesResponse,
  ConfigCatalogResponse,
  HostSettingsResponse,
  UpdateHostSettingsResponse,
  ConsoleConfig,
  ConsoleConfigEnvelope,
  HostRestartResponse,
  Host,
  ProfilePolicyResponse,
  AdminStreamProfilesResponse,
  StreamProfile,
  StreamProfileWrite,
  AdminLaunchProfilesResponse,
  LaunchProfile,
  LaunchProfileWrite,
  DiagnosticBundle,
  SecretResponse,
  SecretsResponse,
  SettingsResponse,
  PlatformApplyAttemptEnvelope,
  PlatformApplyAttemptsResponse,
  PlatformReleaseView,
  ReleaseChannel,
  RegistrationMode,
  StorageProvider,
  InvitesResponse,
  MintInviteResponse,
  HostEnrollmentsResponse,
  MintHostEnrollmentResponse,
  AccessCheck,
  RuntimePresetEnvelope,
  RuntimePresetsResponse,
  RuntimePresetWrite,
  AppArtworkCandidates,
  AppArtworkEnvelope,
  AppArtworkWrite,
  ArtworkCrop,
  ArtworkReresolveResult,
  Entitlement,
  EntitlementGrantRequest,
  EntitlementsResponse,
  ProviderEntitlementMode,
  ProviderEntitlementModeEnvelope,
  LibraryRulesResponse,
  LibraryRuleWriteRequest,
  LibraryRuleWriteResult,
  LibraryStatus,
  LibraryUnpublishedResponse,
  ForceScanRequest,
  ForceScanResult,
  ImageCatalogEnvelope,
  CatalogImage,
  ImageInstallRequest,
  ImageUpdateResult,
  ImageUpdatePolicy,
  Job,
  JobsResponse,
  JobRunsResponse,
  JobPatchRequest,
  JobRunNowRequest,
  JobRunNowAccepted,
} from "./types";

/** Builds a query string from the defined params only, so an absent filter is
 *  an absent parameter rather than an empty one the server would have to treat
 *  as a value. Returns "" when nothing is set. */
function queryString(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined) q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

export interface AdminActivityItem {
  id: number;
  actor_user_id: string | null;
  action: string;
  target_type: string;
  target_id: string | null;
  details: Record<string, unknown>;
  created_at: string;
  /** Joined at read time; null when the actor row is gone or there was none. */
  actor_username: string | null;
  /** Derived server-side from `action` — never stored, never client-supplied. */
  severity: AdminActivitySeverity;
}

export type AdminActivitySeverity = "info" | "warn" | "err";

export interface AdminActivityResponse {
  items: AdminActivityItem[];
  next_cursor: number | null;
}

/** Every filter is optional and ANDed; an undefined one is omitted, so no
 *  argument is the pre-amendment request. `action` is a prefix match and `q` a
 *  substring over action/target_id/actor username. */
export interface ActivityQuery {
  cursor?: number;
  limit?: number;
  action?: string;
  actorUserId?: string;
  targetType?: string;
  /** RFC 3339; a malformed value is a 400, not a silently dropped filter. */
  since?: string;
  q?: string;
}

export function listAdminActivity(
  token: string,
  opts: ActivityQuery = {},
  signal?: AbortSignal,
): Promise<AdminActivityResponse> {
  const query = queryString({
    cursor: opts.cursor,
    limit: opts.limit,
    action: opts.action,
    actor_user_id: opts.actorUserId,
    target_type: opts.targetType,
    since: opts.since,
    q: opts.q,
  });
  return apiFetch<AdminActivityResponse>(`/admin/activity${query}`, { token, signal });
}

// ── Instance settings + invites (LP-SEC-01) ──────────────────────────────────

export function getSettings(token: string, signal?: AbortSignal): Promise<SettingsResponse> {
  return apiFetch<SettingsResponse>("/admin/settings", { token, signal });
}

/** Absent fields are unchanged. Setting a library-discovery field never lifts an
 *  environment override — see LibraryStatus's `*_overridden_by_env`. */
export function updateSettings(
  token: string,
  patch: {
    registration_mode?: RegistrationMode;
    storage_provider?: StorageProvider;
    library_discovery_enabled?: boolean;
    library_discovery_interval_minutes?: number;
    library_discovery_appdetails_enabled?: boolean;
    image_update_policy?: ImageUpdatePolicy;
    mic_capture_enabled?: boolean;
    /** `QUASAR_ALLOWED_ORIGINS` overrides this when set. */
    allowed_origins?: string[];
    release_channel?: ReleaseChannel;
    /** Rejected with 400 unless it is a git ref name: 1-255 characters, no
     *  whitespace, no "..", no leading "-". */
    release_edge_branch?: string;
  },
): Promise<SettingsResponse> {
  return apiFetch<SettingsResponse>("/admin/settings", {
    method: "PATCH",
    body: patch,
    token,
  });
}

// ── Encrypted secrets ─────────────────────────────────────────────────────────
// No route ever returns a secret value (control-api.md §"Encrypted secrets") —
// responses carry configured/readable/origin and a masked hint only.

export function listSecrets(token: string, signal?: AbortSignal): Promise<SecretsResponse> {
  return apiFetch<SecretsResponse>("/admin/secrets", { token, signal });
}

/** 409 is a key-management problem, with two distinct messages: no master key
 *  (`QUASAR_SECRET_KEY`) vs. a mismatched one. Surface the server's message. */
export function setSecret(token: string, name: string, value: string): Promise<SecretResponse> {
  return apiFetch<SecretResponse>(`/admin/secrets/${encodeURIComponent(name)}`, {
    method: "PUT",
    token,
    body: { value },
  });
}

/** Any environment-var fallback the secret declares takes effect again. */
export function clearSecret(token: string, name: string): Promise<void> {
  return apiFetch<void>(`/admin/secrets/${encodeURIComponent(name)}`, {
    method: "DELETE",
    token,
  });
}

/** `pending` is "would still redeem right now": not revoked, not expired, and
 *  used_count < max_uses. Omitting it is `all`. */
export type InviteState = "all" | "pending";

/** Never returns the plaintext code. */
export function listInvites(token: string, opts: { state?: InviteState } = {}): Promise<InvitesResponse> {
  return apiFetch<InvitesResponse>(`/admin/invites${queryString({ state: opts.state })}`, { token });
}

/** The response carries the plaintext code + magic link ONCE, never retrievable
 *  again. All fields optional; `{}` = single-use user. */
export function mintInvite(
  token: string,
  req: { role?: "user" | "admin"; max_uses?: number; expires_at?: string | null; note?: string },
): Promise<MintInviteResponse> {
  return apiFetch<MintInviteResponse>("/admin/invites", { method: "POST", body: req, token });
}

export function revokeInvite(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/invites/${id}`, { method: "DELETE", token });
}

// ── Host enrollment tokens (#12/#96) ─────────────────────────────────────────
// Per-host, hashed, single-use by default, one-hour expiry. Same custody model
// as invites: the plaintext is returned exactly once by mint and never again.

/** Never returns the plaintext token. */
export function listHostEnrollments(
  token: string,
  opts: { state?: InviteState } = {},
): Promise<HostEnrollmentsResponse> {
  return apiFetch<HostEnrollmentsResponse>(
    `/admin/hosts/enrollments${queryString({ state: opts.state })}`,
    { token },
  );
}

/** `{}` mints a single-use, one-hour, any-node token. `node_name` binds it to
 *  exactly one host. */
export function mintHostEnrollment(
  token: string,
  req: { node_name?: string; max_uses?: number; expires_at?: string | null; note?: string } = {},
): Promise<MintHostEnrollmentResponse> {
  return apiFetch<MintHostEnrollmentResponse>("/admin/hosts/enrollments", {
    method: "POST",
    body: req,
    token,
  });
}

export function revokeHostEnrollment(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/hosts/enrollments/${id}`, { method: "DELETE", token });
}

/** This request's reachability. The enroll-host flow reads the certificate
 *  fingerprint from here, so the value it pins is the one THIS browser session
 *  was served — and tells the operator to compare it against the startup log. */
export function accessCheck(token: string): Promise<AccessCheck> {
  return apiFetch<AccessCheck>("/admin/access-check", { token });
}

// ── App catalog (P2-08) ───────────────────────────────────────────────────────

export function listAdminApps(token: string, cursor?: string): Promise<AdminAppsResponse> {
  const params = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
  return apiFetch<AdminAppsResponse>(`/admin/apps${params}`, { token });
}

export function getApp(token: string, id: string): Promise<{ app: AdminApp }> {
  return apiFetch<{ app: AdminApp }>(`/apps/${id}`, { token });
}

export function createApp(token: string, req: CreateAppRequest): Promise<{ app: AdminApp }> {
  return apiFetch<{ app: AdminApp }>("/apps", { method: "POST", body: req, token });
}

export function updateApp(
  token: string,
  id: string,
  req: UpdateAppRequest,
): Promise<{ app: AdminApp }> {
  return apiFetch<{ app: AdminApp }>(`/apps/${id}`, { method: "PATCH", body: req, token });
}

// ── Entitlements (steam-library-discovery spec §6.6, Phase 2) ────────────────

/** 'all' rows first. */
export function listAppEntitlements(token: string, appId: string): Promise<EntitlementsResponse> {
  return apiFetch<EntitlementsResponse>(`/admin/apps/${appId}/entitlements`, { token });
}

/** `subject_id` is required when `subject_type` is "user" and must be omitted
 *  for "all". 409 when that subject already holds an entitlement on this app. */
export function grantEntitlement(
  token: string,
  appId: string,
  req: EntitlementGrantRequest,
): Promise<{ entitlement: Entitlement }> {
  return apiFetch<{ entitlement: Entitlement }>(`/admin/apps/${appId}/entitlements`, {
    method: "POST",
    body: req,
    token,
  });
}

/** Works on a `granted_by: "provider"` row too; the warning that a later library
 *  sync may re-grant it lives in the editor's Access tab, not here. */
export function revokeEntitlement(
  token: string,
  appId: string,
  entitlementId: string,
): Promise<void> {
  return apiFetch<void>(`/admin/apps/${appId}/entitlements/${entitlementId}`, {
    method: "DELETE",
    token,
  });
}

/** PERSONAL grants only — 'all' rows are excluded (spec §6.6), so callers must
 *  not read an empty list as "this user sees nothing". */
export function listUserEntitlements(token: string, userId: string): Promise<EntitlementsResponse> {
  return apiFetch<EntitlementsResponse>(`/admin/users/${userId}/entitlements`, { token });
}

/** Addressed by provider name ("steam"), not app id, and REPLACES the whole
 *  entitlement state: "all" writes one all-users row, "user" entitles the acting
 *  admin only, "none" clears every row. 404 means the provider app isn't created
 *  yet (async install pending) — treat it as "try again shortly". */
export function setProviderEntitlementMode(
  token: string,
  provider: string,
  mode: ProviderEntitlementMode,
): Promise<ProviderEntitlementModeEnvelope> {
  return apiFetch<ProviderEntitlementModeEnvelope>(
    `/admin/library-providers/${encodeURIComponent(provider)}/entitlement-mode`,
    { method: "POST", body: { mode }, token },
  );
}

// ── Library discovery (steam-library-discovery spec §7/§8/§11, Phase 4) ──────

/** Read alongside the `library_discovery_enabled` setting: no host with a
 *  managed-home storage root, or a `0` scan interval, makes the toggle inert
 *  while it still reads "on" (spec §7.5). */
export function getLibraryStatus(token: string): Promise<LibraryStatus> {
  return apiFetch<LibraryStatus>("/admin/library/status", { token });
}

/** Bypasses the janitor's recency pacing but none of discovery's other gates:
 *  the instance switch, no-storage-root, and `QUASAR_LIBRARY_SCAN_INTERVAL=0`
 *  still apply, surfaced as `inert_reason` on a 200 rather than an error status.
 *  Inertness is checked BEFORE `scope`, so a bad app_id on an inert instance is
 *  still 200 + inert_reason; on a live one it is 400/404. Omitted scope = scan
 *  everything now. */
export function forceLibraryScan(
  token: string,
  scope: ForceScanRequest = {},
  signal?: AbortSignal,
): Promise<ForceScanResult> {
  return apiFetch<ForceScanResult>("/admin/library/scan", {
    method: "POST",
    body: scope,
    token,
    signal,
  });
}

/** `appId` is the PROVIDER app's id (`library_provider: "steam"`), never a
 *  derived tile's — as in every library-rule call below. Newest first. */
export function listLibraryRules(token: string, appId: string): Promise<LibraryRulesResponse> {
  return apiFetch<LibraryRulesResponse>(`/admin/apps/${appId}/library/rules`, { token });
}

/** `rule: "ignore"` disables the tile and revokes its provider entitlements in
 *  one server-side transaction; it never deletes the app row, its artwork, or a
 *  favourite (spec §8.2). `rule: "allow"` beats the built-in denylist on the
 *  next scan and touches no tile. `externalId` is the Steam appid. */
export function setLibraryRule(
  token: string,
  appId: string,
  externalId: string,
  req: LibraryRuleWriteRequest,
): Promise<LibraryRuleWriteResult> {
  return apiFetch<LibraryRuleWriteResult>(
    `/admin/apps/${appId}/library/rules/${encodeURIComponent(externalId)}`,
    { method: "PUT", body: req, token },
  );
}

/** Not the un-ignore action: deleting falls back to the built-in denylist, which
 *  may still suppress the appid, where writing `rule: "allow"` would not. */
export function deleteLibraryRule(
  token: string,
  appId: string,
  externalId: string,
  externalSource = "steam",
): Promise<void> {
  return apiFetch<void>(
    `/admin/apps/${appId}/library/rules/${encodeURIComponent(externalId)}` +
      `?external_source=${encodeURIComponent(externalSource)}`,
    { method: "DELETE", token },
  );
}

export function listUnpublishedLibraryItems(
  token: string,
  appId: string,
): Promise<LibraryUnpublishedResponse> {
  return apiFetch<LibraryUnpublishedResponse>(`/admin/apps/${appId}/library/unpublished`, { token });
}

// ── Runtime presets (UI-P3) ───────────────────────────────────────────────────

export function listRuntimePresets(token: string): Promise<RuntimePresetsResponse> {
  return apiFetch<RuntimePresetsResponse>("/admin/runtime-presets", { token });
}

export function getRuntimePreset(token: string, id: string): Promise<RuntimePresetEnvelope> {
  return apiFetch<RuntimePresetEnvelope>(`/admin/runtime-presets/${id}`, { token });
}

export function createRuntimePreset(
  token: string,
  req: RuntimePresetWrite,
): Promise<RuntimePresetEnvelope> {
  return apiFetch<RuntimePresetEnvelope>("/admin/runtime-presets", {
    method: "POST",
    body: req,
    token,
  });
}

/** Absent fields are unchanged. Takes effect on the next launch of every app
 *  using it; nothing is flattened client-side. */
export function updateRuntimePreset(
  token: string,
  id: string,
  req: RuntimePresetWrite,
): Promise<RuntimePresetEnvelope> {
  return apiFetch<RuntimePresetEnvelope>(`/admin/runtime-presets/${id}`, {
    method: "PATCH",
    body: req,
    token,
  });
}

/** Refuse-if-in-use: the 409 is the enforcement, the disabled Delete button is
 *  UX only (CLAUDE.md invariant #6) — as for the profile deletes below. */
export function deleteRuntimePreset(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/runtime-presets/${id}`, { method: "DELETE", token });
}

// ── App-image catalog (Spec A P1: read + sync only) ───────────────────────────

export function listImages(token: string): Promise<ImageCatalogEnvelope> {
  return apiFetch<ImageCatalogEnvelope>("/admin/images", { token });
}

/** A manifest-fetch failure is reported as `sync_error` on a 200 with the cached
 *  catalog, never thrown as an ApiError. */
export function syncImages(token: string): Promise<ImageCatalogEnvelope> {
  return apiFetch<ImageCatalogEnvelope>("/admin/images/sync", { method: "POST", token });
}

/** 409 `digest_unresolved` means the last sync couldn't resolve a content digest
 *  for this image: re-sync the catalog and retry. */
export function installImage(
  token: string,
  id: string,
  req?: ImageInstallRequest,
): Promise<CatalogImage> {
  return apiFetch<CatalogImage>(`/admin/images/${encodeURIComponent(id)}/install`, {
    method: "POST",
    body: req ?? {},
    token,
  });
}

/** Best-effort `image_remove` to every host that has it, then drops the adoption row. */
export function uninstallImage(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/images/${encodeURIComponent(id)}/install`, {
    method: "DELETE",
    token,
  });
}

export function pinImage(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/images/${encodeURIComponent(id)}/pin`, {
    method: "POST",
    token,
  });
}

/** Does not itself trigger an update. */
export function unpinImage(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/images/${encodeURIComponent(id)}/pin`, {
    method: "DELETE",
    token,
  });
}

/** `applied:false` on a 200 is a no-op (already at the catalog version), not an
 *  error. 409 when the image is pinned. */
export function updateImage(token: string, id: string): Promise<ImageUpdateResult> {
  return apiFetch<ImageUpdateResult>(`/admin/images/${encodeURIComponent(id)}/update`, {
    method: "POST",
    token,
  });
}

// ── Hosts / capacity (P2-09, P3-07) ─────────────────────────────────────────

export function listHosts(token: string, cursor?: string): Promise<HostsResponse> {
  const params = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
  return apiFetch<HostsResponse>(`/hosts${params}`, { token });
}

export function getHost(token: string, id: string): Promise<{ host: Host }> {
  return apiFetch<{ host: Host }>(`/hosts/${id}`, { token });
}

export function getHostGPUs(token: string, hostId: string): Promise<GPUsResponse> {
  return apiFetch<GPUsResponse>(`/hosts/${hostId}/gpus`, { token });
}

export function drainHost(
  token: string,
  id: string,
  force = false,
): Promise<{ host: import("./types").Host }> {
  return apiFetch<{ host: import("./types").Host }>(`/hosts/${id}/drain`, {
    method: "POST",
    body: { force },
    token,
  });
}

export function uncordonHost(token: string, id: string): Promise<{ host: import("./types").Host }> {
  return apiFetch<{ host: import("./types").Host }>(`/hosts/${id}/uncordon`, {
    method: "POST",
    token,
  });
}

// ── Session oversight (P2-10) ─────────────────────────────────────────────────

/** `active` is every non-terminal state. Omitting it is `all`, which is exactly
 *  the pre-amendment request. */
export type AdminSessionState = "all" | "active" | "ended" | "failed";

export function listAllSessions(
  token: string,
  cursor?: string,
  opts: { state?: AdminSessionState; limit?: number } = {},
): Promise<AdminSessionsResponse> {
  const params = queryString({ cursor, state: opts.state, limit: opts.limit });
  return apiFetch<AdminSessionsResponse>(`/admin/sessions${params}`, { token });
}

export function forceStopSession(token: string, id: string): Promise<unknown> {
  return apiFetch(`/sessions/${id}`, { method: "DELETE", token });
}

/** Newest-first. */
export function getSessionMetrics(
  token: string,
  sessionId: string,
  opts: { limit?: number; cursor?: string; source?: "agent" | "browser" } = {},
): Promise<MetricsResponse> {
  const params = new URLSearchParams();
  if (opts.limit !== undefined) params.set("limit", String(opts.limit));
  if (opts.cursor) params.set("cursor", opts.cursor);
  if (opts.source) params.set("source", opts.source);
  const qs = params.toString();
  return apiFetch<MetricsResponse>(
    `/admin/sessions/${sessionId}/metrics${qs ? `?${qs}` : ""}`,
    { token },
  );
}

// ── User management (P2-11) ───────────────────────────────────────────────────

export function listUsers(token: string, cursor?: string): Promise<AdminUsersResponse> {
  const params = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
  return apiFetch<AdminUsersResponse>(`/users${params}`, { token });
}

export function updateUser(
  token: string,
  id: string,
  patch: { role?: string; disabled?: boolean; max_concurrent_sessions?: number },
): Promise<{ user: AdminUser }> {
  return apiFetch<{ user: AdminUser }>(`/users/${id}`, { method: "PATCH", body: patch, token });
}

/** Hard delete; terminal history cascades. semantics: control-api.md §errors */
export function deleteUser(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/users/${id}`, { method: "DELETE", token });
}

// ── App + host delete ────────────────────────────────────────────────────────

export function deleteApp(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/apps/${id}`, { method: "DELETE", token });
}

/** GPUs + history cascade. semantics: control-api.md §errors */
export function deleteHost(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/hosts/${id}`, { method: "DELETE", token });
}

// ── Storage oversight (P5-01 / P5-05) ────────────────────────────────────────

export function listAdminHomes(
  token: string,
  opts: { userId?: string; appId?: string; pendingGc?: boolean; cursor?: string } = {},
): Promise<AdminHomesResponse> {
  const params = new URLSearchParams();
  if (opts.userId) params.set("user_id", opts.userId);
  if (opts.appId) params.set("app_id", opts.appId);
  if (opts.pendingGc !== undefined) params.set("pending_gc", String(opts.pendingGc));
  if (opts.cursor) params.set("cursor", opts.cursor);
  const qs = params.toString();
  return apiFetch<AdminHomesResponse>(`/admin/storage/homes${qs ? `?${qs}` : ""}`, { token });
}

// ── Host runtime settings (host-settings admin UI) ────────────────────────────

export function getConfigCatalog(token: string): Promise<ConfigCatalogResponse> {
  return apiFetch<ConfigCatalogResponse>(`/admin/config/catalog`, { token });
}

export function getHostSettings(token: string, hostId: string): Promise<HostSettingsResponse> {
  return apiFetch<HostSettingsResponse>(`/admin/hosts/${hostId}/settings`, { token });
}

/** A null value clears that key back to the catalog default. 409
 *  "restart_required" when a restart-class knob changed with live sessions
 *  present and restartConfirm was not set. */
export function updateHostSettings(
  token: string,
  hostId: string,
  overrides: Record<string, boolean | number | string | null>,
  restartConfirm = false,
): Promise<UpdateHostSettingsResponse> {
  return apiFetch<UpdateHostSettingsResponse>(`/admin/hosts/${hostId}/settings`, {
    method: "PATCH",
    body: { overrides, restart_confirm: restartConfirm },
    token,
  });
}

/** Restarts the agent without changing overrides — the lever for applying an
 *  already-persisted restart-class change. 409 "restart_required" (with
 *  `.liveSessions`) when sessions block it, "conflict" when the host is offline. */
export function restartHost(
  token: string,
  hostId: string,
  confirm = false,
): Promise<HostRestartResponse> {
  return apiFetch<HostRestartResponse>(`/admin/hosts/${hostId}/restart`, {
    method: "POST",
    body: { confirm },
    token,
  });
}

// ── Console mode (CM-01/CM-05) ────────────────────────────────────────────────

export function getConsoleConfig(token: string, hostId: string): Promise<ConsoleConfigEnvelope> {
  return apiFetch<ConsoleConfigEnvelope>(`/admin/hosts/${hostId}/console-config`, { token });
}

export function updateConsoleConfig(
  token: string,
  hostId: string,
  patch: ConsoleConfig,
): Promise<ConsoleConfigEnvelope> {
  return apiFetch<ConsoleConfigEnvelope>(`/admin/hosts/${hostId}/console-config`, {
    method: "PATCH",
    body: patch,
    token,
  });
}

export function getProfilePolicy(token: string): Promise<ProfilePolicyResponse> {
  return apiFetch<ProfilePolicyResponse>("/admin/profile-policy", { token });
}

export function updateProfilePolicy(
  token: string,
  req: ProfilePolicyResponse,
): Promise<ProfilePolicyResponse> {
  return apiFetch<ProfilePolicyResponse>("/admin/profile-policy", {
    method: "PATCH",
    body: req,
    token,
  });
}

// ── Stream profiles / launch profiles (UI-P4) ────────────────────────────────
// A stream profile is one encode rung; a launch profile is an ordered chain of
// rungs. Write responses here are bare objects, not `{ profile: ... }`; list
// responses stay `{ items: [...] }` (openapi.yaml `/v1/admin/{stream,launch}-profiles`).

export function listStreamProfiles(token: string): Promise<AdminStreamProfilesResponse> {
  return apiFetch<AdminStreamProfilesResponse>("/admin/stream-profiles", { token });
}

export function createStreamProfile(
  token: string,
  req: StreamProfileWrite,
): Promise<StreamProfile> {
  return apiFetch<StreamProfile>("/admin/stream-profiles", { method: "POST", body: req, token });
}

export function updateStreamProfile(
  token: string,
  id: string,
  req: StreamProfileWrite,
): Promise<StreamProfile> {
  return apiFetch<StreamProfile>(`/admin/stream-profiles/${id}`, {
    method: "PATCH",
    body: req,
    token,
  });
}

/** Refuse-if-in-use: 409 while any launch profile lists this rung. */
export function deleteStreamProfile(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/stream-profiles/${id}`, { method: "DELETE", token });
}

export function listLaunchProfiles(token: string): Promise<AdminLaunchProfilesResponse> {
  return apiFetch<AdminLaunchProfilesResponse>("/admin/launch-profiles", { token });
}

/** `rungs` must contain at least one h264 rung (400 otherwise); h264 not being
 *  last is a `warnings[]` entry, never a rejection. */
export function createLaunchProfile(
  token: string,
  req: LaunchProfileWrite,
): Promise<LaunchProfile> {
  return apiFetch<LaunchProfile>("/admin/launch-profiles", { method: "POST", body: req, token });
}

/** Reordering sends the full ordered id array; order IS preference order. */
export function updateLaunchProfile(
  token: string,
  id: string,
  req: LaunchProfileWrite,
): Promise<LaunchProfile> {
  return apiFetch<LaunchProfile>(`/admin/launch-profiles/${id}`, {
    method: "PATCH",
    body: req,
    token,
  });
}

/** Refuse-if-referenced: 409 while any app, the global policy, or any user
 *  preference points at it. */
export function deleteLaunchProfile(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/launch-profiles/${id}`, { method: "DELETE", token });
}

/** Tombstones the home for GC (202). semantics: control-api.md §errors */
export function tombstoneHome(token: string, id: string): Promise<void> {
  return apiFetch<void>(`/admin/storage/homes/${id}`, { method: "DELETE", token });
}

// ── Session trace (ST-06/ST-07) ───────────────────────────────────────────────

export function getDiagnosticBundle(token: string, sessionId: string): Promise<DiagnosticBundle> {
  return apiFetch<DiagnosticBundle>(`/admin/sessions/${sessionId}/diagnostic-bundle`, { token });
}

// ── Cover artwork (UI-P7) ─────────────────────────────────────────────────────

export function getAppArtwork(token: string, appId: string): Promise<AppArtworkEnvelope> {
  return apiFetch<AppArtworkEnvelope>(`/admin/apps/${appId}/artwork`, { token });
}

/** 409 means no provider is configured — the documented default, not a failure
 *  to surface as one. */
export function searchAppArtwork(
  token: string,
  appId: string,
  query?: string,
): Promise<AppArtworkCandidates> {
  return apiFetch<AppArtworkCandidates>(`/admin/apps/${appId}/artwork/search`, {
    method: "POST",
    token,
    body: { query: query ?? "" },
  });
}

/** Exactly one intent per call: `provider_ref`, `tile_url`/`hero_url`
 *  (SSRF-guarded server-side), or `rematch`. Always sets `locked`, so the
 *  background sweep cannot undo it. */
export function setAppArtwork(
  token: string,
  appId: string,
  body: AppArtworkWrite,
): Promise<AppArtworkEnvelope> {
  return apiFetch<AppArtworkEnvelope>(`/admin/apps/${appId}/artwork`, {
    method: "PUT",
    token,
    body,
  });
}

/** Re-runs matching for EVERY app. Exists because automatic resolution returns
 *  early for apps that already have an artwork record, so a changed provider
 *  query or tile crop would never reach them. Locked records are skipped and
 *  counted; `force` is the only way past a manual correction. */
export function reresolveAllArtwork(
  token: string,
  force = false,
): Promise<ArtworkReresolveResult> {
  return apiFetch<ArtworkReresolveResult>("/admin/artwork/reresolve", {
    method: "POST",
    token,
    body: { force },
  });
}

export function clearAppArtwork(token: string, appId: string): Promise<void> {
  return apiFetch<void>(`/admin/apps/${appId}/artwork`, { method: "DELETE", token });
}

/** Not `apiFetch`: that helper JSON-encodes the body and forces a JSON
 *  Content-Type, and this endpoint takes raw bytes with their own image/* type.
 *  The server re-validates size and sniffs the bytes regardless, so a wrong
 *  Content-Type here is a rejection, never a stored file. */
export async function uploadAppArtwork(
  token: string,
  appId: string,
  crop: ArtworkCrop,
  file: File,
): Promise<AppArtworkEnvelope> {
  const res = await fetch(`/v1/admin/apps/${appId}/artwork/upload?crop=${crop}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": file.type || "application/octet-stream",
    },
    body: file,
  });
  if (!res.ok) {
    let message = res.statusText || "upload failed";
    let code = "internal";
    try {
      const data = (await res.json()) as { error?: { code?: string; message?: string } };
      if (data?.error?.message) message = data.error.message;
      if (data?.error?.code) code = data.error.code;
    } catch {
      // A 413 from a proxy can be non-JSON — keep the status-derived default.
      if (res.status === 413) message = "that image is too large";
    }
    throw new ApiError(res.status, code, message);
  }
  return (await res.json()) as AppArtworkEnvelope;
}

// ── Background jobs (jobs-framework spec, WP7) ────────────────────────────────
// Codes a caller may branch on (ApiError.code): "job_unmanaged", "job_disabled",
// "job_already_running", "schedule_locked" (control-plane internal/httpx/respond.go).

export function listJobs(token: string, signal?: AbortSignal): Promise<JobsResponse> {
  return apiFetch<JobsResponse>("/admin/jobs", { token, signal });
}

export function getJob(token: string, jobId: string): Promise<Job> {
  return apiFetch<Job>(`/admin/jobs/${jobId}`, { token });
}

/** Adopted jobs only (409 `job_unmanaged`). 409 `schedule_locked` when an
 *  environment variable is authoritative over `interval_secs`. */
export function patchJob(token: string, jobId: string, req: JobPatchRequest): Promise<Job> {
  return apiFetch<Job>(`/admin/jobs/${jobId}`, { method: "PATCH", body: req, token });
}

/** `host_id` is required for a host-scoped job and must be omitted for an
 *  instance-scoped one. A manual run bypasses the schedule window but never the
 *  job's own gates (design §3.4), so a deferred outcome is not an error. */
export function runJobNow(
  token: string,
  jobId: string,
  req: JobRunNowRequest = {},
): Promise<JobRunNowAccepted> {
  return apiFetch<JobRunNowAccepted>(`/admin/jobs/${jobId}/run`, {
    method: "POST",
    body: req,
    token,
  });
}

/** The whole admin Releases page in one read. READ ONLY — it never triggers
 *  detection; "Check now" is runJobNow("platform.release_detect"). */
export function getPlatformReleases(
  token: string,
  signal?: AbortSignal,
): Promise<PlatformReleaseView> {
  return apiFetch<PlatformReleaseView>("/admin/platform/releases", { token, signal });
}

/** Apply one release to one host. 202 with the attempt; the work is
 *  asynchronous and watched through active_apply on the release view. */
export function applyPlatformReleaseToHost(
  token: string,
  hostId: string,
  req: { release_id: string; force?: boolean },
): Promise<PlatformApplyAttemptEnvelope> {
  return apiFetch<PlatformApplyAttemptEnvelope>(`/admin/platform/hosts/${hostId}/apply`, {
    method: "POST",
    body: req,
    token,
  });
}

/** Put one host back on the digests recorded as previous on its last succeeded
 *  attempt. A revert is an apply with an older digest set; the control plane is
 *  never revertible (ADR 0002). */
export function revertPlatformHost(
  token: string,
  hostId: string,
  req: { force?: boolean } = {},
): Promise<PlatformApplyAttemptEnvelope> {
  return apiFetch<PlatformApplyAttemptEnvelope>(`/admin/platform/hosts/${hostId}/revert`, {
    method: "POST",
    body: req,
    token,
  });
}

/** Apply history, newest first, control-plane attempts included. */
export function listPlatformAttempts(
  token: string,
  opts: { hostId?: string; limit?: number } = {},
  signal?: AbortSignal,
): Promise<PlatformApplyAttemptsResponse> {
  const query = queryString({ host_id: opts.hostId, limit: opts.limit });
  return apiFetch<PlatformApplyAttemptsResponse>(`/admin/platform/attempts${query}`, {
    token,
    signal,
  });
}

/** Bounded, newest first. */
export function listJobRuns(
  token: string,
  jobId: string,
  opts: { hostId?: string; cursor?: string; limit?: number } = {},
): Promise<JobRunsResponse> {
  const params = new URLSearchParams();
  if (opts.hostId) params.set("host_id", opts.hostId);
  if (opts.cursor) params.set("cursor", opts.cursor);
  if (opts.limit) params.set("limit", String(opts.limit));
  const qs = params.toString();
  return apiFetch<JobRunsResponse>(`/admin/jobs/${jobId}/runs${qs ? `?${qs}` : ""}`, { token });
}
