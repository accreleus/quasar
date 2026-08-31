/**
 * The /app/account information architecture — the v3 account rail
 * (design_handoff_v3 spec §A.21/§5.5).
 *
 * Same item shape as the admin rail (../../admin/adminNav.ts) because it
 * answers the same kind of question — "where do I change this thing about my
 * setup" — and a user who is also an operator should not have to learn two
 * navigation models. Like the admin rail it carries no `.rail-sec` headers:
 * the mock's `renderAccountRail` emits one untitled row per section, not one
 * per page, and the section's tabs handle nav within it. A row therefore lights
 * for any of its section's tab paths, not just its own `to`.
 *
 *  - Account     — who you are and what holds your token.
 *  - Preferences — how Quasar presents itself to you.
 *  - Usage       — what your launches have left behind, and what is live.
 *
 * The Password card merges into Profile in v3 (spec §3.3), so /security is no
 * longer a rail row.
 */

import type { IconName } from "../../../components/icons";
import type { RailSection } from "../../../components/shell/navTypes";
import type { SectionTab } from "../../../components/shell/sectionTabs";

type AccountSectionKey = "account" | "prefs" | "usage";

/**
 * The tabs inside each account section (handoff §A.22). The rail carries one
 * row per section; these carry the six pages as tabs under one section head,
 * which is the shape the mock renders.
 *
 * List order is destination order: a rail row goes to its section's first tab.
 */
export const ACCOUNT_TABS: Record<AccountSectionKey, SectionTab[]> = {
  account: [
    { id: "profile", label: "Profile", to: "/app/account/profile" },
    { id: "devices", label: "Devices", to: "/app/account/devices" },
  ],
  prefs: [
    { id: "overlay", label: "In-session overlay", to: "/app/account/overlay" },
    { id: "streaming", label: "Stream quality", to: "/app/account/streaming" },
  ],
  usage: [
    { id: "storage", label: "Storage", to: "/app/account/storage" },
    { id: "sessions", label: "My sessions", to: "/app/account/sessions" },
  ],
};

/** Lights the row for every page under `key`, plus any extra path. */
function sectionMatch(key: AccountSectionKey, ...extra: string[]) {
  const paths = new Set([...ACCOUNT_TABS[key].map((t) => t.to), ...extra]);
  return (path: string) => paths.has(path);
}

const ROWS: { id: AccountSectionKey; label: string; icon: IconName; extra?: string }[] = [
  // The bare /app/account index resolves to Profile, so Account owns it too.
  { id: "account", label: "Account", icon: "profile", extra: "/app/account" },
  { id: "prefs", label: "Preferences", icon: "overlay" },
  { id: "usage", label: "Usage", icon: "storage" },
];

export function buildAccountSections(): RailSection[] {
  return [
    {
      items: ROWS.map(({ id, label, icon, extra }) => ({
        id,
        label,
        icon,
        to: ACCOUNT_TABS[id][0].to,
        match: sectionMatch(id, ...(extra ? [extra] : [])),
      })),
    },
  ];
}
