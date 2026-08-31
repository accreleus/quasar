/**
 * The /admin information architecture — the v3 flat rail
 * (design_handoff_v3/screens/admin-console-v3.html `NAV`, spec §A.0).
 *
 * Eight destinations, no section headers: the v3 console folds the old
 * four-group rail into subject tabs inside each area (Fleet → Hosts/Storage/
 * Jobs, Library → Apps/Presets/Images/Sources, Streaming → Launch/Stream,
 * People → Users/Invites), so the rail carries one row per area and the tab
 * bar carries the rest.
 *
 * Each item owns its own `match` predicate rather than leaning on NavLink's
 * `end` flag, because "which rail row is lit" is a fact about the IA (a host's
 * settings page lights Fleet) and belongs beside the route it describes.
 *
 * Library-provider entries are gone from the rail: the v3 IA gives them the
 * Library → Sources page (spec §3.3), not a nav row of their own.
 *
 * UX only: admin endpoints are enforced server-side by RequireAuth →
 * RequireAdmin; the rail hiding a route is never the gate.
 */

import type { RailItem } from "../../components/shell/navTypes";

export const ADMIN_NAV: RailItem[] = [
  { id: "overview", label: "Overview", icon: "overview", to: "/admin", match: (p) => p === "/admin" },
  {
    id: "sessions",
    label: "Sessions",
    icon: "sessions",
    to: "/admin/sessions",
    badge: "live",
    match: (p) => p.startsWith("/admin/sessions"),
  },
  {
    id: "fleet",
    label: "Fleet",
    icon: "fleet",
    to: "/admin/fleet/hosts",
    badge: "fault",
    match: (p) => p.startsWith("/admin/fleet"),
  },
  {
    id: "library",
    label: "Library",
    icon: "library",
    to: "/admin/library/apps",
    match: (p) => p.startsWith("/admin/library"),
  },
  {
    id: "streaming",
    label: "Streaming",
    icon: "streaming",
    to: "/admin/streaming/launch",
    match: (p) => p.startsWith("/admin/streaming"),
  },
  {
    id: "people",
    label: "People",
    icon: "people",
    to: "/admin/people/users",
    match: (p) => p.startsWith("/admin/people"),
  },
  { id: "audit", label: "Audit log", icon: "audit", to: "/admin/audit", match: (p) => p.startsWith("/admin/audit") },
  {
    id: "settings",
    label: "Settings",
    icon: "settings",
    to: "/admin/settings",
    match: (p) => p.startsWith("/admin/settings"),
  },
];
