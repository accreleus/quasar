// Wire types for the SPA's control-plane API. Source of truth is
// `protocol/openapi.yaml` → generated `./schema.d.ts` (`npm run gen:api`).
// `scripts/verify/web.sh` regenerates into a temp file and fails on drift; it
// calls `openapi-typescript` directly (its CLI rejects a second `-o`), so the
// spec path and generator flags MUST stay in sync with `schema_drift_check`
// there — package.json cannot hold a comment pointing back. Hand-written types
// below are web-only shapes, or ones the generated schema fits lossily.

import type { components, paths } from "./schema";

type Schemas = components["schemas"];
// Some endpoints answer with an inline schema, reachable only via the path map.
type Paths = paths;

/** Makes generated properties required, preserving `| null`. Only for schemas that
 *  still lack `required:` in openapi.yaml; drop it when they gain one. */
type Req<T> = T extends readonly (infer U)[]
  ? Req<U>[]
  : T extends object
    ? { [K in keyof T]-?: Req<T[K]> }
    : T;

// ── Core account / auth ───────────────────────────────────────────────────────

export type Role = Schemas["Role"];

export type User = Schemas["User"];

/** `expires_at` is required-massaged: the SPA persists it as the session expiry
 *  (`auth/storage.ts`) but the contract marks it optional. */
export type LoginResponse = Omit<Schemas["LoginResponse"], "expires_at"> & {
  expires_at: string;
};

export interface MeResponse {
  user: User;
}

/** semantics: control-api.md §errors */
export interface ApiErrorBody {
  error: {
    code: string;
    message: string;
    live_sessions?: number;
    /** On `home_in_use`: omitted, never empty, when the conflict is known but
     *  the session is not (steam-library-discovery spec §2.2). */
    session_id?: string;
  };
}

// ── Invites + instance settings (LP-SEC-01) ──────────────────────────────────

export type RegistrationMode = Schemas["RegistrationMode"];

/** `auto` and `local` are one behaviour: homes go under the session host's storage
 *  root, and a host with no root fails the launch rather than falling back to a
 *  docker volume. `volume` is refused at runtime (#473,
 *  `internal/settings.ValidStorageProvider`) and survives in the generated type only
 *  because the enum lives in frozen `protocol/openapi.yaml` — as does its stale doc. */
export type StorageProvider = Schemas["StorageProvider"];

/** `library/ImagesTab.tsx` defaults it to `"notify"` when absent, matching the column's
 *  `DEFAULT 'notify'` (migration 0054); optional for pre-P3 servers. */
export type ImageUpdatePolicy = NonNullable<InstanceSettings["image_update_policy"]>;

/** `library_discovery_enabled` (migration 0045) and `image_update_policy`
 *  (migration 0054) are generated now — no hand-written widening. */
export type InstanceSettings = NonNullable<Schemas["SettingsEnvelope"]["settings"]>;

export type SettingsResponse = Schemas["SettingsEnvelope"];

// ── Platform releases (#104/#110, control-api.md §Platform releases) ─────────

export type ReleaseChannel = NonNullable<InstanceSettings["release_channel"]>;

/** GET /v1/admin/platform/releases — the whole Releases page in one read. */
export type PlatformReleaseView = Schemas["PlatformReleaseView"];
export type PlatformRelease = Schemas["PlatformRelease"];
export type PlatformReleaseTarget = Schemas["PlatformReleaseTarget"];
export type PlatformReleaseFault = Schemas["PlatformReleaseFault"];
export type PlatformHostIdentity = Schemas["PlatformHostIdentity"];
export type PlatformIdentity = Schemas["PlatformIdentity"];
/** Closed vocabulary the UI maps to text; an unrecognised value is rendered
 *  verbatim rather than dropping the row (control-api.md). */
export type EligibilityReason = Schemas["EligibilityReason"];

/** Never carries the plaintext code. */
export type Invite = Schemas["Invite"];

export interface InvitesResponse {
  invites: Invite[];
}

export interface MintInviteResponse {
  invite: Schemas["InviteMinted"];
}

// ── Host enrollment tokens (#12/#96) ─────────────────────────────────────────

/** List shape — never carries the plaintext token. */
export type HostEnrollment = Schemas["HostEnrollment"];

export interface HostEnrollmentsResponse {
  enrollments: HostEnrollment[];
}

/** The plaintext token, ONCE. The one-paste enrollment string is composed
 *  client-side from it (`lib/enrollmentString`), never by the server. */
export interface MintHostEnrollmentResponse {
  enrollment: Schemas["HostEnrollmentMinted"];
}

/** GET /v1/admin/access-check — THIS request's reachability: the certificate in
 *  use (with its fingerprint), origin, secure context. */
export type AccessCheck = Schemas["AccessCheck"];

// ── Encrypted secrets ─────────────────────────────────────────────────────────
// Write-only on the wire: no type here can hold a secret value, and there is
// deliberately no reveal endpoint.

export type SecretStatus = Schemas["SecretStatus"];

export type SecretsResponse = Schemas["SecretsEnvelope"];

export type SecretResponse = Schemas["SecretEnvelope"];

/** `database` outranks `environment`, so a key typed in the UI is never silently
 *  overridden by a stale env var; clearing falls back to the env var. */
export type SecretOrigin = SecretStatus["origin"];

export const ARTWORK_API_KEY_SECRET = "artwork.steamgriddb.api_key";

// ── Library + Session (P1-10) ─────────────────────────────────────────────────

/** No `custom` mode: `profile_policy: "custom"` is a 400 server-side. */
export type ProfilePolicyMode = "inherit" | "prefer" | "force";

/** Presentation-only. Nothing in scheduling, admission, profile/codec resolution,
 *  or the agent wire reads it (steam-library-discovery §4.5.3). */
export type AppKind = Schemas["AppKind"];

/** Operator-set, never inferred (steam-library-discovery §1.1). Nothing
 *  server-side reads `kind` instead (§4.5.3). */
export type LibraryProvider = Schemas["LibraryProvider"];

export type App = Schemas["App"];

/** Web-only view shape; the OpenAPI `Stream` carries extra h264_profile/playout0_ms. */
export interface AppDisplayStream {
  width: number;
  height: number;
  fps: number;
  bitrate_kbps: number;
}

export type AppsResponse = Schemas["AppList"];

/** Wideable enum: a client must fall through to a neutral kicker for an unknown
 *  value, hence `homeData.buildRailCards` takes `reason` as a plain `string`. */
export type HighlightReason = Schemas["HighlightReason"];

export type Highlight = Schemas["Highlight"];

/** The server owns the ranking — `items` is never re-sorted client-side. */
export type HighlightsResponse = Schemas["HighlightList"];

/** Kept local: the OpenAPI `Stream` schema types playout0_ms/h264_profile as
 *  optional, but a session's stream always carries both. */
export interface StreamParams {
  width: number;
  height: number;
  fps: number;
  bitrate_kbps: number;
  h264_profile: string;
  /** Wire vocabulary (`h265`, not the catalog's `hevc`). Distinct from the
   *  browser's `telemetry.negotiatedCodec`; a disagreement is a real defect signal. */
  codec?: "h264" | "h265" | "av1";
  /** Tier-selected initial receiver playout target (ms), AS-02. Drives AS-05. */
  playout0_ms: number;
  /** Whether the server granted a client→host mic m-line (mic-capture §3.5). */
  mic?: boolean;
  /** Hand-written ahead of the schema (§D5) — must be reconciled against the
   *  generated shape on the next protocol pin bump. `width`/`height` stay the
   *  LAUNCH size for the session's life; these report what is encoded now, only
   *  while that differs, and revert to the launch size after a restart. */
  external_width?: number;
  external_height?: number;
  /** Server-ordered; rendered in the order given, never re-sorted. */
  rungs?: [number, number][];
  /** False when this host+codec cannot live-resize. Absent means supported. */
  external_resize_supported?: boolean;
  /** Hand-written ahead of the schema (quasar-protocol PR #15) — reconcile on the
   *  next pin bump. Present only when known AND the external size differs. */
  external_owner?: "auto" | "pinned";
}

export type StreamProfile = Schemas["StreamProfile"];

/** `id` is required on POST (operator-chosen text) and ignored on PATCH. */
export type StreamProfileWrite = Schemas["StreamProfileWrite"];

export type AdminStreamProfilesResponse = Schemas["AdminStreamProfilesResponse"];

export type ProfileRef = Schemas["ProfileRef"];

/** Catalog codec vocabulary (`hevc`, not the wire's `h265`). */
export type CatalogCodec = Schemas["CatalogCodec"];

export type WriteWarning = Schemas["WriteWarning"];

export type ProfileReason = Schemas["ProfileReason"];

export type ProfileEvaluation = Schemas["ProfileEvaluation"];

export type LaunchProfileRung = Schemas["LaunchProfileRung"];

export type LaunchProfileUsedBy = Schemas["LaunchProfileUsedBy"];

export type LaunchProfile = Schemas["LaunchProfile"];

/** `rungs` order IS preference; the server assigns `position`. Must contain at
 *  least one h264 rung (400 otherwise); h264 not last is a warning only. */
export type LaunchProfileWrite = Schemas["LaunchProfileWrite"];

export type AdminLaunchProfilesResponse = Schemas["AdminLaunchProfilesResponse"];

/** The top rung's numbers — advertised, not resolved; the session's `stream` is
 *  the truth. */
export type ProfileNominal = Schemas["ProfileNominal"];

export type LaunchProfileEvaluation = Schemas["LaunchProfileEvaluation"];

export type ProfilesResponse = Schemas["ProfilesResponse"];

// Req-massaged: these two schemas still lack a `required:` array.
export type ProfilePreferencesResponse = Req<Schemas["ProfilePreferencesResponse"]>;

export type ProfilePolicyResponse = Req<Schemas["ProfilePolicyResponse"]>;

export type SessionState = Schemas["SessionState"];

export type HealthState =
  | "healthy"
  | "network_degrading"
  | "abr_at_floor"
  | "client_decode_degrading"
  | "client_presentation_degrading"
  | "unsustainable"
  | "failed";

/** Narrows `stream` and `health_state`, which OpenAPI types more loosely. */
export type Session = Omit<Schemas["Session"], "health_state" | "stream"> & {
  stream: StreamParams;
  health_state?: HealthState;
};

/** `session.stream.codec` is identical whether the rung won on merit, was forced,
 *  or is the h264 floor, so this alone tells them apart and a client must not
 *  collapse them. Read it through `lib/codecDisplay`'s `codecOutcome`. */
export type CodecDecision = Schemas["CodecDecision"];

export type CodecDecisionRung = Schemas["CodecDecisionRung"];

/** An OPEN enum — render an unrecognised value rather than assume it is closed. */
export type RungRejection = Schemas["RungRejection"];

/** The W3C RTCIceServer dictionary, so it passes straight into the
 *  RTCPeerConnection constructor with no translation (#509). */
export type ICEServer = Schemas["ICEServer"];

export type SignalingCoords = Schemas["SignalingCoords"];

export type LaunchResponse = Omit<Schemas["LaunchResponse"], "session"> & {
  session: Session;
};

export type SessionResponse = Omit<Schemas["SessionEnvelope"], "session"> & {
  session: Session;
};

// ── Admin types (P2-08 through P2-11) ─────────────────────────────────────────

export type AdminApp = Schemas["AdminApp"];

export type AdminAppsResponse = Schemas["AdminAppsResponse"];

/** Kept local: the OpenAPI `AppWrite` schema omits profile_policy / managed_home /
 *  home_container_path / resource defaults, so it is a lossy fit. */
export interface CreateAppRequest {
  name: string;
  description: string;
  cover_url?: string | null;
  /** Never send "" to mean the server default — that is a 400, not a no-op. */
  kind?: AppKind;
  /** Marking it "steam" is the trigger for library discovery; refused on a
   *  derived tile. Independent of `kind`. */
  library_provider?: LibraryProvider;
  /** Deprecated (#383): accepted but no longer acted on in admission. */
  default_vram_mb?: number;
  /** Omit it so the INSERT takes Postgres' DEFAULT of 1 (migration
   *  0001_initial_schema). Never send 0 to mean "unset" — the cb97bfb bug, where 0
   *  produced apps the scheduler admitted as needing no encode slot. */
  default_encode_slots?: number;
  default_width: number;
  default_height: number;
  default_fps: number;
  default_bitrate_kbps: number;
  default_profile_id?: string | null;
  profile_policy?: ProfilePolicyMode;
  runtime_spec: Record<string, unknown>;
  managed_home?: boolean;
  home_container_path?: string;
  runtime_preset_id?: string | null;
  /** Absent or `[]` = unrestricted. Never send `null` — a 400, as is a non-empty
   *  list under `profile_policy: "force"`. The server enforces it independently. */
  launchable_profile_ids?: string[];
}

/** Local: see CreateAppRequest. */
export interface UpdateAppRequest {
  name?: string;
  description?: string;
  cover_url?: string | null;
  /** Never send "" to mean the server default — that is a 400, not a no-op. */
  kind?: AppKind;
  /** Absent = unchanged; absence is never a zero value (the cb97bfb trap). An
   *  explicit "" is a valid un-marking. */
  library_provider?: LibraryProvider;
  /** Deprecated (#383) — see CreateAppRequest.default_vram_mb. */
  default_vram_mb?: number;
  default_encode_slots?: number;
  default_width?: number;
  default_height?: number;
  default_fps?: number;
  default_bitrate_kbps?: number;
  default_profile_id?: string | null;
  profile_policy?: ProfilePolicyMode;
  enabled?: boolean;
  runtime_spec?: Record<string, unknown>;
  managed_home?: boolean;
  home_container_path?: string;
  /** Tri-state: absent = unchanged, `null` = clear, uuid = set. */
  runtime_preset_id?: string | null;
  /** Absent = unchanged, `[]` = clear, non-empty = replace wholesale. `null` is a
   *  400. Switching an app to `"force"` clears it server-side either way. */
  launchable_profile_ids?: string[];
}

// ── Entitlements (steam-library-discovery §6.6, Phase 2) ─────────────────────
// An authorization fact, not library metadata (CLAUDE.md invariant #6). Nothing
// here is the access control; the server enforces it on every request.

/** The generated component is `SubjectType`; this alias keeps the name consumers use. */
export type EntitlementSubjectType = Schemas["SubjectType"];

/** Revoking a 'provider' grant does not stop a future sync from re-granting it
 *  while the game is still installed (§6.6). */
export type EntitlementGrantedBy = Schemas["EntitlementGrantedBy"];

export type Entitlement = Schemas["Entitlement"];

export type EntitlementsResponse = Schemas["EntitlementList"];

/** `subject_id` is required for `subject_type: "user"` and must be omitted, null,
 *  or "" for "all"; anything else is a 400. */
export type EntitlementGrantRequest = Schemas["EntitlementGrant"];

/** One exclusive state, not an incremental grant (#465). "user" always means the
 *  acting admin. */
export type ProviderEntitlementMode = Schemas["ProviderEntitlementMode"];

export type ProviderEntitlementModeSet = Schemas["ProviderEntitlementModeSet"];

/** Replaces the whole entitlement set — `items` is the result, not a delta. */
export type ProviderEntitlementModeEnvelope = Schemas["ProviderEntitlementModeEnvelope"];

// ── Library discovery (steam-library-discovery §7/§8/§11, Phase 4) ───────────

/** `"other"` means rungs 1-4 would have published the appid yet no enabled tile
 *  exists — unknowable, so stated. `"appdetails"` is its own case (§8.2). */
export type LibrarySuppressedBy = Schemas["LibrarySuppressedBy"];

/** (external_source, external_id) is the server-side idempotency key — writing
 *  again replaces the row rather than accumulating. */
export type LibraryAppidRule = Schemas["LibraryRule"];

export type LibraryRulesResponse = Schemas["LibraryRuleList"];

export type LibraryRuleWriteRequest = Schemas["LibraryRuleWrite"];

/** The outer `rule` key is the house envelope idiom, the inner `rule.rule` the
 *  stored column value; do not rename either. `disabled`/`revoked` matter only for
 *  `rule: "ignore"` (§8.2's three steps in one transaction). `rule: "allow"` never
 *  re-enables a tile — that happens on the next scan, so an admin's manual disable
 *  stands. */
export type LibraryRuleWriteResult = Schemas["LibraryRuleSetResult"];

/** A read and a button, not a review queue: nothing waits on it (§8.2). */
export type LibraryUnpublishedItem = Schemas["LibraryUnpublished"];

export type LibraryUnpublishedResponse = Schemas["LibraryUnpublishedList"];

export type LibraryScanCounts = Schemas["LibraryScanCounts"];

/** Answers "nothing appeared" vs "nothing ran" (§7.5). Never parse or enumerate
 *  `inert_reason`: the set grows. Render it verbatim. */
export type LibraryStatus = Schemas["LibraryStatus"];

/** Counts are stored at reconcile (migration 0048): a scan reported earlier reads
 *  all-zero, which the UI must present as "not recorded", never "nothing happened". */
export type LibraryRecentScan = LibraryStatus["recent_scans"][number];

export type ForceScanRequest = Schemas["LibraryForceScanRequest"];

/** Two different zeros: `queued: 0` with `eligible > 0` means already queued or
 *  being walked; with `eligible: 0` the scope matched nothing. `inert_reason` shares
 *  its instance-level reasons with `LibraryStatus` through one server-side helper
 *  and adds route-specific ones; never map it onto a known set. A 200 carrying one
 *  is a success. */
export type ForceScanResult = Schemas["LibraryForceScanResult"];

// ── Runtime presets (UI-P3) ───────────────────────────────────────────────────
// A reusable container configuration many apps inherit — not a launch profile (see
// StreamProfile). Merged server-side at launch, never flattened client-side on save.

export type PresetUser = Schemas["PresetUser"];

export type RuntimePreset = Schemas["RuntimePreset"];

export type RuntimePresetEnvelope = Schemas["RuntimePresetEnvelope"];

export type RuntimePresetsResponse = Schemas["RuntimePresetsResponse"];

/** Only `name` is required; an absent field takes the server default on create
 *  and is unchanged on patch. Never send a zero-value to mean "unset". */
export type RuntimePresetWrite = Schemas["RuntimePresetWrite"];

// ── Storage types (P5-01) ─────────────────────────────────────────────────────

/** `username`/`app_name`/`host_name` are nullable for the reason the ids are: a home
 *  outlives its user/app/host (the `LEFT JOIN`s in `storage.go`'s `ListHomes`). */
export type AdminHome = Schemas["AdminHome"];

export type AdminHomesResponse = Schemas["AdminHomesResponse"];

export type MyStorageItem = Schemas["MyStorageItem"];

export type MyStorageResponse = Schemas["MyStorageResponse"];

/** `Host.storage` has landed in protocol/openapi.yaml but `schema.d.ts` has not
 *  been regenerated, so this stays local until `npm run gen:api` catches up. */
export interface HostStorageVolume {
  label: string;
  path: string;
  total_mb: number;
  available_mb: number;
}

/** The three `agent_*` fields (#429) are a LOCAL-ONLY intersection: protocol/ is a
 *  frozen submodule, so they are absent from openapi.yaml until a follow-up lands
 *  them there. Derived in `control-plane/internal/agentws` from WS connect timing —
 *  never agent-reported, no agent-api.md change. Null/0 until the first connect. */
export type Host = Schemas["Host"] & {
  storage: HostStorageVolume[] | null;
  cpu_model: string | null;
  agent_connected_since: string | null;
  agent_restart_count: number;
  agent_last_restart_at: string | null;
};

export type HostsResponse = Omit<Schemas["HostList"], "items"> & { items: Host[] };

/** `status` is an open enum server-side — an unrecognized value must render
 *  neutrally, never be rejected. */
export type ReadinessCheck = Schemas["ReadinessCheck"];

// ── App-image catalog (Spec A P1: read + sync only) ───────────────────────────

export type CatalogImage = Schemas["CatalogImage"];

export type ImageHostState = Schemas["ImageHostState"];

/** Where the served catalog came from (#548). The manifest is fetched
 *  unauthenticated at a MUTABLE ref, so a force-push upstream silently changes
 *  what every host installs; nothing here prevents that, it makes it visible.
 *  `changed` is about the LAST sync only and self-clears on the next unchanged
 *  one — `previous_sha256`/`changed_at` are the durable record. */
export type ManifestProvenance = Schemas["ManifestProvenance"];

/** `GET /v1/admin/images` / `POST /v1/admin/images/sync` 200 body.
 *  `sync_error` is non-null when the last manifest fetch failed — the
 *  cached catalog is still served, so this must be surfaced, not treated
 *  as an empty-state (Spec A acceptance: "visible in the admin UI"). */
export type ImageCatalogEnvelope = Schemas["ImageCatalogEnvelope"];

export type ImageInstallRequest = Schemas["ImageInstallRequest"];

/** `applied:false` (still 200) means a no-op, not an error. */
export type ImageUpdateResult = Schemas["ImageUpdateResult"];

/** `render_node` is a local addition, null until the agent reports it. The #383
 *  live-VRAM fields are generated and nullable — null means UNKNOWN, never zero. */
export type GPUAvailability = Schemas["GPUAvailability"] & {
  render_node: string | null;
};

export type GPUsResponse = Omit<Schemas["GPUsResponse"], "items"> & { items: GPUAvailability[] };

export interface HostRestartRequest {
  confirm?: boolean;
}

export interface HostRestartResponse {
  restart_triggered: boolean;
}

// ── Telemetry types (P4-01 / P4-05 / P4-06) ──────────────────────────────────

/** All fields optional by contract. The ABR-ladder fields are hand-added:
 *  `node-agent/src/session/metrics.rs` serialises them as untyped JSON passthrough,
 *  but `protocol/openapi.yaml` has no amendment for this additive set yet. */
export type AgentMetrics = Schemas["AgentMetrics"] & {
  abr_setpoint_kbps?: number;
  /** The raw GCC estimate the setpoint is derived from. */
  gcc_estimate_kbps?: number;
  adaptation_state?: string;
  stream_width?: number;
  stream_height?: number;
  ladder_speed_bias?: number;
  ladder_res_rung?: number;
  /** Present only when BELOW the launch rate — absent means "at the launch rate". */
  ladder_fps?: number;
  /** Current ABR floor (kbit/s); absent means "at the launch floor". A setpoint
   *  sitting at it is the governor pinned. It follows the rung, so it is not fixed. */
  abr_floor_kbps?: number;
  /** Present only when the external size differs from the launch size. */
  external_owner?: "auto" | "pinned";
};

export type BrowserMetrics = Schemas["BrowserMetrics"];

// ── Device capability reports (P4-08 / AS10-08 / AS10-12) ─────────────────────
// Built in web/src/webrtc/capability.ts; re-exported so consumers import wire types
// from one place. NativeCapabilities (protocol/native-client.md) is documentation
// only — no web code sends it.
export type {
  DeviceCapabilities,
  NativeCapabilities,
  NativeOsInfo,
  NativeDisplayInfo,
  NativeDecodeMatrix,
  CodecDecodeInfo,
  NativeAudioInfo,
  NativeController,
  NativeInputInfo,
  NativeMetrics,
  NativeHealth,
  CodecsDetail,
  CodecDecodingDetail,
} from "../webrtc/capability";

/** Browser-classified (clientHealth.ts), a sibling field on each stats sample
 *  rather than part of the metrics dict; the server never re-derives it. */
export type ClientHealth =
  | "smooth"
  | "decode_degrading"
  | "presentation_degrading"
  | "backgrounded_or_hidden"
  | "client_unsupported";

/** Each source carries a full MetricPoint, so values live under `.metrics`. */
export type LatestMetrics = Schemas["LatestMetrics"];

export type MetricPoint = Schemas["MetricPoint"];

export type MetricsResponse = Schemas["MetricsResponse"];

/** The local `Session` is narrowed, so this cannot be `Schemas["AdminSession"]`.
 *  The additive fields are `Pick`ed off the generated schema so `gen:api` stays the
 *  source of truth. Optional because the server LEFT JOINs them. */
export type AdminSession = Session &
  Pick<Schemas["AdminSession"], "username" | "app_name" | "host_name"> & {
    latest_metrics?: LatestMetrics;
  };

export interface AdminSessionsResponse {
  items: AdminSession[];
  next_cursor: string | null;
}

export type AdminUser = Schemas["AdminUser"];

export type AdminUsersResponse = Schemas["AdminUsersResponse"];

// ── Host runtime settings (host-settings admin UI) ────────────────────────────

export type ConfigKnob = Schemas["ConfigKnob"];

export type ConfigCatalogResponse = Schemas["ConfigCatalogResponse"];

/** Kept local: the OpenAPI `HostSettings` schema types resolved/overrides values
 *  as `unknown`, lossier than these `boolean | number | string` maps. */
export interface HostSettingsResponse {
  resolved: Record<string, boolean | number | string>;
  overrides: Record<string, boolean | number | string>;
  pending_restart: boolean;
  /** The agent's true running config (env <- overrides), unlike `resolved`, a display
   *  view that cannot see agent env. Always strings. Null until the agent reports. */
  effective: Record<string, string> | null;
  /** The WIRE codec set (never the catalog's `hevc`) this host last reported. `null`
   *  means NEVER REPORTED: the launch resolver collapses it to `["h264"]`, a UI must not. */
  codecs?: string[] | null;
}

/** Local: see HostSettingsResponse. */
export interface UpdateHostSettingsResponse {
  resolved: Record<string, boolean | number | string>;
  overrides: Record<string, boolean | number | string>;
  restart_triggered: boolean;
  effective: Record<string, string> | null;
}

// ── Console mode (CM-01/CM-05) ────────────────────────────────────────────────

export type ConsoleConfig = Schemas["ConsoleConfig"];

export type ConsoleCapabilities = Schemas["ConsoleCapabilities"];

export type ConsoleConfigEnvelope = Schemas["ConsoleConfigEnvelope"];

// ── Session trace types (ST-07) ───────────────────────────────────────────────

// Req-massaged: the trace/diagnostic schemas still lack `required:` arrays.
export type TraceSeriesPoint = Req<Schemas["TraceSeriesPoint"]>;

export type TraceEvent = Req<Schemas["TraceEvent"]>;

/** `value` is null and `note` set when the series had no samples — a missing
 *  measurement must never read as a passing one. `note` is re-relaxed over Req. */
export type Falsifier = Omit<Req<Schemas["Falsifier"]>, "note"> & { note?: string };

/** Two relaxations over Req: `verdict` widens to `string` because the control plane
 *  grows that vocabulary, so an unrecognised value is data to render, never a type
 *  error; and `clock.offset_ms`/`uncertainty_ms` stay optional because an unmeasured
 *  clock carries neither and a zero default is the lie the field prevents. */
export type Verdict = Omit<Req<Schemas["Verdict"]>, "verdict" | "clock" | "falsifiers"> & {
  verdict: string;
  clock: {
    quality: "measured" | "unmeasured";
    offset_ms?: number;
    uncertainty_ms?: number;
    /** Whether the offset was actually APPLIED to the client series before the rules
     *  ran. A measured clock that is only reported is the defect this makes visible. */
    applied?: boolean;
    /** now − measured_at (ms); absent when unmeasured. */
    age_ms?: number;
  };
  falsifiers: Falsifier[];
};

export type DiagnosticBundle = Omit<
  Req<Schemas["DiagnosticBundle"]>,
  "derived_windows" | "classifier" | "ingest"
> & {
  /** Member arrays are additive/optional, so re-relaxed over the Req massage. */
  derived_windows: Partial<Req<Schemas["DiagnosticBundle"]>["derived_windows"]>;
  classifier: Verdict;
  /** Present only when this control plane dropped client points for the session
   *  (an implausible ts_unix_ms). Absent means nothing was rejected. */
  ingest?: Schemas["DiagnosticBundle"]["ingest"];
};

// ── Cover artwork (UI-P7) ─────────────────────────────────────────────────────

/** `source: "none"` is a negative cache and a first-class outcome, not an error.
 *  `locked` is set by any admin override; the automatic sweep never touches one. */
export type AppArtwork = Schemas["AppArtwork"];

/** `provider_configured: false` is the shipped default, not a fault — this
 *  deployment has not opted in to a third-party artwork provider. */
export type AppArtworkEnvelope = Schemas["AppArtworkEnvelope"];

export type AppArtworkCandidates = Schemas["AppArtworkCandidates"];

/** `thumb_url` is REMOTE and preview-only. */
export type AppArtworkCandidate = AppArtworkCandidates["candidates"][number];

/** Exactly one intent per request. */
export type AppArtworkWrite = Schemas["AppArtworkWrite"];

export type ArtworkCrop = "tile" | "hero";

/** `skipped_locked` counts apps whose manual correction was left alone — a
 *  success outcome, not a failure (#385). */
export type ArtworkReresolveResult =
  Paths["/v1/admin/artwork/reresolve"]["post"]["responses"]["200"]["content"]["application/json"];

// ── Background jobs (jobs-framework spec, WP7) ────────────────────────────────
// No Req massage: `Job`'s optional fields are genuinely conditional on
// `managed`/`scope`; requiring them would just move the discrimination to `!`.

export type JobPlane = Schemas["JobPlane"];

export type JobScope = Schemas["JobScope"];

export type JobScheduleKind = Schemas["JobScheduleKind"];

export type JobRunState = Schemas["JobRunState"];

export type JobRunTrigger = Schemas["JobRunTrigger"];

/** Resolved exactly as the dispatcher resolves it. */
export type JobSchedule = Schemas["JobSchedule"];

export type JobRun = Schemas["JobRun"];

export type JobTarget = Schemas["JobTarget"];

/** Three shapes discriminated by `managed`/`scope` — see the schema doc. */
export type Job = Schemas["Job"];

export type JobsResponse = Schemas["JobsResponse"];

export type JobRunsResponse = Schemas["JobRunsResponse"];

export type JobPatchRequest = Schemas["JobPatchRequest"];

export type JobRunNowRequest = Schemas["JobRunNowRequest"];

/** The run is queued, not executed inline. */
export type JobRunNowAccepted = Schemas["JobRunNowAccepted"];
