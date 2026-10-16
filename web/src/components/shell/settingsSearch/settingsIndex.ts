// One search, in the top bar (new design): what the palette (and the Home field)
// can find besides games and fleet objects, and where each one lives on the page.
// Pure: no React, no router, no API, so it is tested on its own (settingsIndex.test.ts).

import type { IconName } from "../../icons";
import type { PaletteGroup, PaletteItem } from "../paletteSearch";

/** How to find a setting once its page is up. `text` narrows `sel` to the element
 *  with exactly that text; `up` names the row to highlight around the match. */
export interface Target {
  sel: string;
  text?: string;
  up?: string;
}

export interface SettingEntry {
  id: string;
  label: string;
  /** Where it lives, shown in the result's meta column. */
  where: string;
  /** Words people type for it that the label does not carry. */
  keywords: string;
  icon: IconName;
  to: string;
  target?: Target;
  /** Admin only: the area (second path segment) this entry belongs to. */
  area?: string;
}

/** A palette row that reveals a setting after it navigates. */
export type SettingItem = PaletteItem & { reveal?: Target };

// ── Account (the one-page layout) ───────────────────────────────────────

const A = "/app/account";
const sec = (id: string, label: string, path: string, icon: IconName, keywords: string): SettingEntry => ({
  id: `acct-${id}`, label, where: "Account", keywords, icon, to: `${A}/${path}`, target: { sel: `#set-${id}` },
});
const overlaySwitch = (key: string, label: string, keywords: string): SettingEntry => ({
  id: `acct-overlay-${key}`, label, where: "Overlay", keywords: `overlay hud strip show hide ${keywords}`, icon: "overlay",
  to: `${A}/overlay`, target: { sel: `#strip-item-${key}`, up: ".ac-sw" },
});

export const ACCOUNT_SETTINGS: readonly SettingEntry[] = [
  sec("profile", "Profile", "profile", "profile", "name username email role avatar who am i signed in"),
  sec("streaming", "Stream quality", "streaming", "streaming", "stream quality resolution"),
  {
    id: "acct-stream-default", label: "Stream default", where: "Stream quality", icon: "streaming", to: `${A}/streaming`,
    keywords: "resolution quality picture 4k 2160p 1440p 1080p 720p fps 60 120 hz frame rate bitrate profile recommended launch",
    target: { sel: "#stream-default", up: ".field" },
  },
  sec("overlay", "In-session overlay", "overlay", "overlay", "hud strip overlay stats"),
  {
    id: "acct-overlay-preset", label: "Overlay content", where: "Overlay", icon: "overlay", to: `${A}/overlay`,
    keywords: "hud strip preset full minimal metrics custom what shows",
    target: { sel: '[aria-label="Overlay content preset"]', up: ".field" },
  },
  overlaySwitch("signal", "Connection signal", "network wifi connection quality"),
  overlaySwitch("identity", "App name and quality", "title game name"),
  overlaySwitch("codec", "Codec", "h264 hevc h265 av1 encoder"),
  overlaySwitch("metrics", "FPS, latency, bitrate", "fps frame rate latency ping lag bitrate stats performance"),
  overlaySwitch("hint", "Menu shortcut hint", "shortcut keyboard menu hint"),
  overlaySwitch("mic", "Microphone", "mic voice talk mute audio"),
  overlaySwitch("fullscreen", "Fullscreen", "full screen"),
  overlaySwitch("capture", "Capture input", "mouse lock pointer keyboard capture"),
  overlaySwitch("exit", "Exit session", "quit leave end stop"),
  {
    id: "acct-overlay-position", label: "Overlay position", where: "Overlay", icon: "overlay", to: `${A}/overlay`,
    keywords: "hud strip move top bottom left right edge where dock",
    target: { sel: '[aria-label="Overlay position"]', up: ".field" },
  },
  {
    id: "acct-overlay-autohide", label: "Auto-hide", where: "Overlay", icon: "overlay", to: `${A}/overlay`,
    keywords: "hud overlay strip hide always visible never show timeout autohide",
    target: { sel: "#auto-hide", up: ".field" },
  },
  sec("appearance", "Appearance", "appearance", "overlay", "layout design look classic old previous new"),
  {
    id: "acct-classic-layout", label: "Classic layout", where: "Appearance", icon: "overlay", to: `${A}/appearance`,
    keywords: "layout design old previous new switch classic turn on off revert back library game page account",
    target: { sel: "#classic-layout", up: ".ac-sw" },
  },
  sec("devices", "Devices", "devices", "devices", "device revoke rename trust sign out phone laptop browser decode"),
  sec("password", "Password", "security", "lock", "password security sign out everywhere all devices log out"),
  {
    id: "acct-password-change", label: "Change password", where: "Password", icon: "lock", to: `${A}/security`,
    keywords: "new password reset security sign out everywhere all devices log out everywhere",
    target: { sel: "#current-password", up: ".field" },
  },
  sec("storage", "Storage", "storage", "storage", "save data saves home disk space gb delete"),
  sec("sessions", "My sessions", "sessions", "sessions", "running live playing now end stop"),
];

/** One entry per device, found by its name. */
export function deviceSettings(devices: readonly { id: string; label: string; current: boolean }[]): SettingEntry[] {
  return devices.map((d) => ({
    id: `acct-dev-${d.id}`,
    label: d.label,
    where: d.current ? "This device" : "Device",
    keywords: "device revoke rename trust sign out",
    icon: "devices",
    to: `${A}/devices`,
    target: { sel: `#os-dev-${d.id}` },
  }));
}

// ── Admin: every page and tab, and the Settings page's sections and rows ────

const page = (area: string, id: string, label: string, where: string, to: string, icon: IconName, keywords: string): SettingEntry => ({
  id: `adm-${id}`, label, where, keywords, icon, to, area,
});
const S = "/admin/settings";
const adminSection = (title: string, keywords: string): SettingEntry => ({
  id: `adm-set-${title.toLowerCase().replace(/\W+/g, "-")}`, label: title, where: "Settings", keywords, icon: "settings", to: S, area: "settings",
  target: { sel: ".panel-title", text: title, up: ".card" },
});
const adminRow = (label: string, section: string, keywords: string): SettingEntry => ({
  id: `adm-row-${label.toLowerCase().replace(/\W+/g, "-")}`, label, where: `Settings · ${section}`, keywords, icon: "settings", to: S, area: "settings",
  target: { sel: ".setting-row .label", text: label, up: ".setting-row" },
});

export const ADMIN_SETTINGS: readonly SettingEntry[] = [
  page("overview", "overview", "Overview", "Console", "/admin", "overview", "home dashboard attention health status summary capacity"),
  page("sessions", "sessions", "Sessions", "Console", "/admin/sessions", "sessions", "live running streams players playing end"),
  page("fleet", "hosts", "Hosts", "Fleet", "/admin/fleet/hosts", "fleet", "fleet gpu machines nodes servers agents enroll add host drain readiness"),
  page("fleet", "storage", "Storage", "Fleet", "/admin/fleet/storage", "storage", "disk space homes save data volumes"),
  page("fleet", "jobs", "Jobs", "Fleet", "/admin/fleet/jobs", "fleet", "schedule cron tasks background scan detection"),
  page("fleet", "releases", "Releases", "Fleet", "/admin/fleet/releases", "fleet", "update upgrade version platform release channel stable edge"),
  page("library", "apps", "Apps", "Library", "/admin/library/apps", "library", "games catalog catalogue titles enable disable artwork"),
  page("library", "presets", "Runtime presets", "Library", "/admin/library/presets", "library", "runtime preset proton wine environment"),
  page("library", "images", "Images", "Library", "/admin/library/images", "image", "container docker image build failed"),
  page("library", "sources", "Sources", "Library", "/admin/library/sources", "library", "steam provider import discovered library sources"),
  page("streaming", "launch", "Launch profiles", "Streaming", "/admin/streaming/launch", "streaming", "resolution 4k 2160p 1440p 1080p 720p fps 60 120 quality default ladder rung"),
  page("streaming", "profiles", "Stream profiles", "Streaming", "/admin/streaming/profiles", "streaming", "bitrate codec h264 hevc h265 av1 abr encoder quality"),
  page("people", "users", "Users", "People", "/admin/people/users", "people", "accounts people admins roles members"),
  page("people", "invites", "Invites", "People", "/admin/people/invites", "people", "invite invitation sign up register code link"),
  page("audit", "audit", "Audit log", "Console", "/admin/audit", "audit", "activity history log events who changed"),
  page("settings", "settings", "Settings", "Console", S, "settings", "instance configuration preferences"),
  adminSection("Instance", "identity access"),
  adminRow("Registration", "Instance", "sign up signup open closed invite only register"),
  adminRow("Allowed origins", "Instance", "cors origin domain url"),
  adminSection("Libraries", "discovery scan steam"),
  adminRow("Library discovery", "Libraries", "steam scan discover installed"),
  adminRow("Scan interval", "Libraries", "minutes scan frequency how often"),
  adminRow("App details lookup", "Libraries", "metadata steam store details"),
  adminSection("Voice", "microphone mic audio"),
  adminRow("Microphone capture", "Voice", "mic voice audio talk"),
  adminSection("Platform updates", "release upgrade"),
  adminRow("Install updates automatically", "Platform updates", "auto update automatic apply release upgrade"),
  adminSection("Images", "image updates"),
  adminRow("Update policy", "Images", "image update manual notify auto"),
  adminSection("Secrets", "keys tokens secret store api key"),
  adminSection("Appearance", "theme density"),
  adminRow("Theme", "Appearance", "dark light mode appearance"),
  adminRow("Density", "Appearance", "compact dense comfortable rows"),
];

const AREA_LABEL: Record<string, string> = {
  overview: "Overview", sessions: "Sessions", fleet: "Fleet", library: "Library", streaming: "Streaming",
  people: "People", audit: "Audit log", settings: "Settings",
};

// ── Matching ────────────────────────────────────────────────────────────────

/** 0 = no match. Every typed word must start a word of the label, where, or
 *  keywords; the label decides the rank. */
export function scoreSetting(e: SettingEntry, query: string): number {
  const tokens = query.toLowerCase().split(/\s+/).map((t) => t.replace(/[^a-z0-9]/g, "")).filter(Boolean);
  if (!tokens.length) return 0;
  const label = e.label.toLowerCase();
  const words = `${label} ${e.where} ${e.keywords}`.toLowerCase().split(/[^a-z0-9]+/);
  if (!tokens.every((t) => words.some((w) => w.startsWith(t)))) return 0;
  const q = tokens.join(" ");
  const labelWords = label.split(/[^a-z0-9]+/);
  // A section or page (no row to light inside it) outranks one of its own rows
  // when the label names it: "overlay" is the overlay section before "Overlay content".
  const broad = e.target?.up ? 0 : 1.5;
  if (label.replace(/[^a-z0-9 ]/g, "") === q) return 4 + broad;
  if (label.startsWith(q)) return 3;
  if (tokens.every((t) => labelWords.some((w) => w.startsWith(t)))) return 2 + broad;
  return 1;
}

export function searchSettings(query: string, entries: readonly SettingEntry[]): SettingEntry[] {
  return entries
    .map((e, i) => ({ e, i, s: scoreSetting(e, query) }))
    .filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s || a.i - b.i)
    .map((x) => x.e);
}

export const toItem = (e: SettingEntry): SettingItem => ({
  id: `ss-${e.id}`, label: e.label, meta: e.where, icon: e.icon, to: e.to, reveal: e.target,
});

// ── Where we are, and what the trigger promises there ───────────────────────

export type Place = { kind: "account" } | { kind: "home" } | { kind: "admin"; area: string; path: string };

/** The admin place needs an admin: the console index is never searched for anyone else. */
export function placeOf(pathname: string, isAdmin: boolean): Place {
  if (isAdmin && pathname.startsWith("/admin")) return { kind: "admin", area: pathname.split("/")[2] || "overview", path: pathname.replace(/\/+$/, "") };
  if (pathname.startsWith("/app/account")) return { kind: "account" };
  return { kind: "home" };
}

export function settingsTriggerLabel(kind: Place["kind"]): string {
  if (kind === "admin") return "Search hosts, sessions, apps, users, settings…";
  if (kind === "account") return "Search settings and games…";
  return "Search games and settings…";
}

/** The trigger on a phone, where the full label cannot fit on one line. */
export function settingsShortLabel(kind: Place["kind"]): string {
  return kind === "admin" ? "Search the console\u2026" : kind === "account" ? "Search settings\u2026" : "Search games\u2026";
}

export function settingsPlaceholder(kind: Place["kind"]): string {
  if (kind === "admin") return "Search hosts, sessions, apps, users, pages or settings";
  if (kind === "account") return "Search settings and games";
  return "Search games and settings";
}

// ── Arranging the palette's groups: the current place first ─────────────────

const SETTINGS_CAP = 6;
/** Home and Library are about games: settings follow them, fewer of them. */
const SETTINGS_CAP_HOME = 4;
const ELSEWHERE_CAP = 6;
const HERE_CAP_EMPTY = 8;

/**
 * Takes the shipped palette groups and adds settings, current place first:
 * account → Settings, games, actions; Home/Library → games, Settings, actions;
 * admin → this area's pages and settings, then hosts/sessions/apps/users, then the
 * rest of the console, then actions (the "Go to" jumps drop out once you type: the
 * page index answers them).
 */
export function arrangeWithSettings(
  groups: readonly PaletteGroup[],
  query: string,
  place: Place,
  devices: readonly SettingEntry[] = [],
): PaletteGroup[] {
  const q = query.trim();
  const real = groups.filter((g) => g.title !== "No matches" && g.items.length);
  const actions = real.find((g) => g.title === "Actions");
  const entities = real.filter((g) => g !== actions);
  const out: PaletteGroup[] = [];
  const push = (title: string, items: PaletteItem[]) => items.length && out.push({ title, items });

  if (place.kind === "admin") {
    // With nothing typed, this area's other tabs and settings: not the page you are on.
    const hits = q
      ? searchSettings(q, ADMIN_SETTINGS)
      : ADMIN_SETTINGS.filter((e) => e.area === place.area && (e.target || e.to !== place.path)).slice(0, HERE_CAP_EMPTY);
    push(AREA_LABEL[place.area] ?? "This page", hits.filter((e) => e.area === place.area).map(toItem));
    entities.forEach((g) => push(g.title, g.items));
    if (q) push("Elsewhere in the console", hits.filter((e) => e.area !== place.area).slice(0, ELSEWHERE_CAP).map(toItem));
    if (actions) push("Actions", q ? actions.items.filter((a) => !a.id.startsWith("go-") || a.id === "go-account") : actions.items);
  } else {
    const all = [...ACCOUNT_SETTINGS, ...devices];
    const cap = place.kind === "home" ? SETTINGS_CAP_HOME : SETTINGS_CAP;
    const hits = q ? searchSettings(q, all).slice(0, cap) : place.kind === "account" ? all.filter((e) => e.where === "Account") : [];
    const settings = hits.map(toItem);
    if (place.kind === "account") push("Settings", settings);
    entities.forEach((g) => push(g.title, g.items));
    if (place.kind === "home") push("Settings", settings);
    if (actions) push("Actions", actions.items);
  }
  return out.length ? out : [{ title: "No matches", items: [] }];
}
