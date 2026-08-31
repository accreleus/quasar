// Small-screen primary nav for the user area (UX assessment §2.3/§2.8/§3.4).
// Below 820px the topbar pills vanish with no replacement, and account's
// `sidebar` AppShell mode drops them at every width too, so both areas lost
// their route back to Home/Library. A bottom tab bar (not a hamburger) keeps
// both destinations permanently visible, matching Plex/Jellyfin/Stadia. It is
// shared by both shells because "which area of the app am I in" is one
// question; the account rail answers a different one ("which section") and
// stays alongside it. /admin is out of scope: too many routes for three slots.
// Account is its own tab, not a menu item, so the bar has something to light
// while the account shell is mounted.

import type { ReactNode } from "react";
import { buildUserNav } from "./navItems";

export interface UserTab {
  to: string;
  label: string;
  icon: ReactNode;
  /** Exact-match active state (the topbar pills' rule) vs prefix match. */
  end: boolean;
}

function HomeIcon() {
  return (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <path d="M3 8.4 10 3l7 5.4V16a1 1 0 0 1-1 1h-3v-5H7v5H4a1 1 0 0 1-1-1z" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function LibraryIcon() {
  return (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <rect x="2.75" y="2.75" width="6" height="6" rx="1.4" />
      <rect x="11.25" y="2.75" width="6" height="6" rx="1.4" />
      <rect x="2.75" y="11.25" width="6" height="6" rx="1.4" />
      <rect x="11.25" y="11.25" width="6" height="6" rx="1.4" />
    </svg>
  );
}

function AccountIcon() {
  return (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <circle cx="10" cy="6.75" r="3.25" />
      <path d="M3.5 16.5c0-3.6 2.9-5.9 6.5-5.9s6.5 2.3 6.5 5.9" strokeLinecap="round" />
    </svg>
  );
}

// Keyed on label, not route — keying on `to` once produced the wrong icon.
const ICONS: Record<string, ReactNode> = {
  Home: <HomeIcon />,
  Library: <LibraryIcon />,
};

/** Derived from `buildUserNav` (not restated) plus an appended Account tab. */
export function buildUserTabs(): UserTab[] {
  const primary = buildUserNav().map((item) => ({
    ...item,
    // `/app` prefixes every other user route, so primary entries need exact
    // match — same rule the topbar pills use (`end` on NavLink).
    end: true,
    icon: ICONS[item.label] ?? <LibraryIcon />,
  }));
  return [
    ...primary,
    // Prefix match: /app/account/storage must still light "Account".
    { to: "/app/account", label: "Account", end: false, icon: <AccountIcon /> },
  ];
}
