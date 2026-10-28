// Fixed /v1 answers for the frozen harness. Shapes follow protocol/openapi.yaml.
// Timestamps are fixed (the harness also freezes the page clock to NOW).
export const NOW = "2026-10-28T12:00:00Z";

const USER = { id: "00000000-0000-4000-8000-000000000001", email: "admin@example.com", username: "admin", role: "admin", created_at: "2026-01-02T10:00:00Z" };

const GAMES = ["Hollow Peak", "Starfall Drift", "Iron Harbor", "Neon Courier", "Quiet Orchard", "Glass Tide", "Ember Vale", "Signal Lost", "Paper Moons", "Vector Rally", "Lantern Keep", "Tundra Line"];
const APPS = GAMES.map((name, i) => ({
  id: `00000000-0000-4000-8000-0000000001${String(i).padStart(2, "0")}`,
  name,
  description: `${name} — a fixture app.`,
  cover_url: null,
  hero_url: null,
  logo_url: null,
  icon_url: null,
  kind: i === 11 ? "desktop" : "game",
  favourite: i % 4 === 0,
  enabled: true,
  created_at: `2026-0${1 + (i % 9)}-1${i % 9}T10:00:00Z`,
}));

const REGION_INPUT = {
  region: { timezone: "Europe/London", language: "en-GB", region_format: null },
  input: { keyboard: { layout: "gb", variant: "" }, match_cursor: "auto", mouse_sensitivity: 1, raw_mouse: null },
  defaults: {
    region: { timezone: "UTC", language: "en-US", region_format: null },
    input: { keyboard: { layout: "us", variant: "" }, match_cursor: "auto", mouse_sensitivity: 1, raw_mouse: false },
  },
};

const SETTINGS = {
  settings: {
    registration_mode: "invite_only",
    storage_provider: "local",
    mic_capture_enabled: false,
    library_discovery_enabled: true,
    library_discovery_interval_minutes: 360,
    library_discovery_appdetails_enabled: false,
    image_update_policy: "notify",
    allowed_origins: ["https://play.example.com"],
    default_timezone: "UTC",
    default_language: "en-US",
    default_region_format: null,
    default_keyboard_layout: "us",
    default_keyboard_variant: "",
    default_match_cursor: "auto",
    default_mouse_sensitivity: 1,
    default_raw_mouse: false,
    updated_by: null,
    updated_at: "2026-07-29T00:00:00Z",
  },
};

const LIBRARY_STATUS = {
  interval_overridden_by_env: false, appdetails_overridden_by_env: false, last_scan_completed_at: null,
  recent_scans: [], enabled: true, storage_provider: "local", scan_interval_secs: 21600,
  appdetails_lookup: false, inert_reason: "", scans: { pending: 0, claimed: 0, failed: 0 },
};

const empty = { items: [], next_cursor: null };

const GET = {
  "/setup/status": { admin_exists: true, setup_completed: true },
  "/me": { user: USER },
  "/me/ui-preferences": { session_overlay: { strip_preset: "full", strip_position: "top", strip_auto_hide: "on_capture" } },
  "/me/region-input": REGION_INPUT,
  "/me/highlights": { items: [
    { app_id: APPS[1].id, reason: "most_played", session_id: null, session_started_at: null, play_seconds: 5400, last_played_at: "2026-10-27T20:00:00Z" },
    { app_id: APPS[2].id, reason: "resume", session_id: null, session_started_at: null, play_seconds: 0, last_played_at: "2026-10-26T20:00:00Z" },
    { app_id: APPS[3].id, reason: "recently_added", session_id: null, session_started_at: null, play_seconds: 0, last_played_at: null },
  ] },
  "/me/devices": { items: [{ id: "dev-ar", name: "Harness browser", first_seen_at: "2026-01-02T10:00:00Z", last_seen_at: NOW, current: true }] },
  "/me/storage": { items: [] },
  "/me/play-history": empty,
  "/me/steam-warm": { enabled: false },
  "/apps": { items: APPS, next_cursor: null },
  "/sessions": empty,
  "/admin/settings": SETTINGS,
  "/admin/library/status": LIBRARY_STATUS,
  "/me/profile-preferences": { default_profile_id: null, steam_dialogs: "show", global_default_profile_id: null, user_overrides_allowed: true },
  "/me/profiles": { recommended_id: "p-1080-60", confidence: "high", notes: [], profiles: [] },
  "/admin/secrets": { secrets: [] },
  "/admin/platform/identity": { identity: { version: "0.5.1", source_commit: null, built_at: null, schema_version: 90 } },
  "/admin/platform/releases": { channel: "stable", edge_branch: "develop", checked_at: NOW, last_error: null, installed: null, available: [], targets: [], faults: [] },
  "/admin/apps": { items: [], next_cursor: null },
  "/users": { items: [], next_cursor: null },
  "/hosts": { items: [] },
  "/admin/sessions": { items: [], next_cursor: null },
};

/** The fixture for (path, method), or undefined when the harness has none. */
export function fixtures(p, method) {
  if (method !== "GET") {
    if (p === "/me/devices") return { device: { id: "dev-ar", first_seen_at: "2026-01-02T10:00:00Z", last_seen_at: NOW } };
    return undefined;
  }
  return GET[p];
}
