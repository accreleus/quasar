/**
 * Command-palette search — pure, and deliberately API-free.
 *
 * The palette searches four entity kinds plus a fixed action list
 * (design_handoff_v3/screens/admin-console-v3.html `palResults`, spec §A.0).
 * Everything here is a plain function over plain data: the inputs are minimal
 * Structural types, not the generated API types, so this module has no import
 * edge into `api/` and can be reasoned about (and tested) without a renderer,
 * a router or a token.
 *
 * Routes are the v3 table (spec §3.3), not the mock's hash routes.
 */

import type { IconName } from "../icons";

// ── Inputs (structural, not the API types) ───────────────────────────────────

export interface PaletteHost {
  id: string;
  node_name: string;
  status: string;
}

export interface PaletteSession {
  id: string;
  app_name?: string;
  username?: string;
  state: string;
}

export interface PaletteApp {
  id: string;
  name: string;
  kind: string;
}

export interface PaletteUser {
  id: string;
  username: string;
  email: string;
}

export interface PaletteSources {
  hosts: readonly PaletteHost[];
  sessions: readonly PaletteSession[];
  apps: readonly PaletteApp[];
  users: readonly PaletteUser[];
  /** Server-enforced elsewhere; here it only decides what is worth offering. */
  isAdmin: boolean;
}

// ── Output ───────────────────────────────────────────────────────────────────

/** A side effect the palette runs instead of navigating. */
export type PaletteAction = "toggle-appearance";

export interface PaletteItem {
  /** Stable within one result set — used as the React key. */
  id: string;
  label: string;
  /** Right-aligned mono column: a mnemonic for actions, a state/kind/email otherwise. */
  meta?: string;
  icon: IconName;
  /** Where Enter goes. Exactly one of `to` / `action` is set. */
  to?: string;
  action?: PaletteAction;
}

export interface PaletteGroup {
  title: string;
  items: PaletteItem[];
}

// ── Actions ──────────────────────────────────────────────────────────────────

/** The admin action list (spec §A.0), re-pointed at the v3 routes (spec §3.3). */
export const PALETTE_ACTIONS: readonly PaletteItem[] = [
  { id: "go-overview", label: "Go to Overview", meta: "g o", icon: "overview", to: "/admin" },
  { id: "go-sessions", label: "Go to Sessions", meta: "g s", icon: "sessions", to: "/admin/sessions" },
  { id: "go-fleet", label: "Go to Fleet", meta: "g f", icon: "fleet", to: "/admin/fleet/hosts" },
  { id: "go-storage", label: "Go to Storage", icon: "storage", to: "/admin/fleet/storage" },
  { id: "go-images", label: "Go to Images", icon: "image", to: "/admin/library/images" },
  { id: "go-library", label: "Go to Library", meta: "g l", icon: "library", to: "/admin/library/apps" },
  { id: "go-presets", label: "Go to Runtime presets", icon: "library", to: "/admin/library/presets" },
  { id: "go-sources", label: "Go to Sources", icon: "library", to: "/admin/library/sources" },
  { id: "go-launch", label: "Go to Launch profiles", icon: "streaming", to: "/admin/streaming/launch" },
  { id: "go-stream", label: "Go to Stream profiles", icon: "streaming", to: "/admin/streaming/profiles" },
  { id: "go-users", label: "Go to Users", meta: "g u", icon: "people", to: "/admin/people/users" },
  { id: "go-invites", label: "Go to Invites", icon: "people", to: "/admin/people/invites" },
  { id: "go-audit", label: "Go to Audit log", meta: "g a", icon: "audit", to: "/admin/audit" },
  { id: "go-settings", label: "Go to Settings", icon: "settings", to: "/admin/settings" },
  { id: "go-account", label: "Go to Account", icon: "profile", to: "/app/account/profile" },
  { id: "toggle-appearance", label: "Toggle appearance", icon: "sun", action: "toggle-appearance" },
  { id: "enroll-host", label: "Enroll a host", icon: "plus", to: "/admin/fleet/hosts" },
  { id: "mint-invite", label: "Mint an invite", icon: "plus", to: "/admin/people/invites" },
];

/** What a non-admin can reach: the whole user area is four destinations. */
export const USER_PALETTE_ACTIONS: readonly PaletteItem[] = [
  { id: "go-home", label: "Go to Home", icon: "home", to: "/app" },
  { id: "go-user-library", label: "Go to Library", icon: "library", to: "/app/library" },
  { id: "go-user-account", label: "Go to Account", icon: "profile", to: "/app/account/profile" },
  { id: "toggle-appearance", label: "Toggle appearance", icon: "sun", action: "toggle-appearance" },
];

// ── What the palette may PROMISE ─────────────────────────────────────────────

/**
 * The entity kinds this palette can actually answer for.
 *
 * A kind counts only when the shell HANDED the list down — `undefined` means
 * "this area does not load it", which is different from an empty list — and
 * only when the viewer is allowed to see it. The trigger label and the input
 * placeholder are both built from this, so the console cannot advertise a
 * search it will never return a row for (the account area, for instance, has
 * apps and nothing else, admin or not).
 *
 * Apps are called "games" to a non-admin: it is the same list, but their
 * library is the only thing in it and "apps" is operator vocabulary.
 */
export function paletteScope(sources: {
  isAdmin: boolean;
  hosts?: readonly unknown[];
  sessions?: readonly unknown[];
  apps?: readonly unknown[];
  users?: readonly unknown[];
}): string[] {
  const kinds: string[] = [];
  if (sources.isAdmin && sources.hosts) kinds.push("hosts");
  if (sources.isAdmin && sources.sessions) kinds.push("sessions");
  if (sources.apps) kinds.push(sources.isAdmin ? "apps" : "games");
  if (sources.isAdmin && sources.users) kinds.push("users");
  return kinds;
}

/** The dialog input's placeholder. Jumping to a page always works, so that
 *  half of the promise is unconditional. */
export function palettePlaceholder(kinds: readonly string[]): string {
  return kinds.length ? `Search ${kinds.join(", ")} or jump to a page` : "Jump to a page";
}

/** The topbar trigger's shorter label for the same promise. */
export function paletteTriggerLabel(kinds: readonly string[]): string {
  return kinds.length ? `Search ${kinds.join(", ")}\u2026` : "Jump to a page";
}

const ACTION_CAP_EMPTY = 6;
const ACTION_CAP_QUERY = 5;
const ENTITY_CAP = 4;

const has = (haystack: string, needle: string) =>
  haystack.toLowerCase().includes(needle.toLowerCase());

/**
 * Search actions and entities for `query`.
 *
 * Actions: filtered by label substring and capped (6 on an empty query, 5
 * otherwise), exactly as the mock does. The non-admin list is the same rule
 * over a shorter list (the user area is four destinations) — filtering it the
 * same way is what keeps "No matches" reachable for a user, and what stops a
 * query that clearly means one thing from being answered with four unrelated
 * jumps.
 *
 * Entities: only for a non-empty query, four each, and only for an admin —
 * hosts, sessions and users are admin surfaces. A non-admin's app hits route
 * into their own library search instead of the admin app editor.
 */
export function searchPalette(query: string, sources: PaletteSources): PaletteGroup[] {
  const q = query.trim().toLowerCase();
  const { hosts, sessions, apps, users, isAdmin } = sources;
  const groups: PaletteGroup[] = [];

  const actions = (isAdmin ? PALETTE_ACTIONS : USER_PALETTE_ACTIONS)
    .filter((a) => !q || has(a.label, q))
    .slice(0, q ? ACTION_CAP_QUERY : ACTION_CAP_EMPTY);
  if (actions.length) groups.push({ title: "Actions", items: [...actions] });

  if (!q) return groups;

  if (isAdmin) {
    const hostItems = hosts
      .filter((h) => has(h.node_name, q) || has(h.id, q))
      .slice(0, ENTITY_CAP)
      .map<PaletteItem>((h) => ({
        id: `host-${h.id}`,
        label: h.node_name,
        meta: h.status,
        icon: "fleet",
        to: `/admin/fleet/hosts/${h.id}`,
      }));
    if (hostItems.length) groups.push({ title: "Hosts", items: hostItems });

    const sessionItems = sessions
      .filter((s) => has(`${s.app_name ?? ""}${s.username ?? ""}${s.id}`, q))
      .slice(0, ENTITY_CAP)
      .map<PaletteItem>((s) => ({
        id: `session-${s.id}`,
        label: `${s.app_name ?? "Session"} · ${s.username ?? "unknown"}`,
        meta: s.id,
        icon: "sessions",
        to: `/admin/sessions/${s.id}`,
      }));
    if (sessionItems.length) groups.push({ title: "Sessions", items: sessionItems });
  }

  const appItems = apps
    .filter((a) => has(a.name, q))
    .slice(0, ENTITY_CAP)
    .map<PaletteItem>((a) => ({
      id: `app-${a.id}`,
      label: a.name,
      meta: a.kind,
      icon: "library",
      to: isAdmin
        ? `/admin/library/apps/${a.id}`
        : `/app/library?q=${encodeURIComponent(a.name)}`,
    }));
  if (appItems.length) groups.push({ title: "Apps", items: appItems });

  if (isAdmin) {
    const userItems = users
      .filter((u) => has(`${u.username}${u.email}`, q))
      .slice(0, ENTITY_CAP)
      .map<PaletteItem>((u) => ({
        id: `user-${u.id}`,
        label: u.username,
        meta: u.email,
        icon: "people",
        to: "/admin/people/users",
      }));
    if (userItems.length) groups.push({ title: "Users", items: userItems });
  }

  if (!groups.length) return [{ title: "No matches", items: [] }];
  return groups;
}
