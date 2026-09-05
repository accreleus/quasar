/**
 * The tab row inside each of the four /admin sections (spec §3.3; handoff
 * §A.4 FLEET_TABS, §A.9 LIB_TABS, §A.16 STR_TABS, §A.18 PPL_TABS).
 *
 * The v3 rail carries one row per section (adminNav.ts) and these carry the
 * rest, so this list and that one are the whole admin IA between them. Order
 * is the mock's order; `to` is the route the tab owns, and every route under
 * it lights the same tab.
 *
 * Jobs and Releases are Fleet tabs with no v3 mock: both are fleet operations,
 * and they reuse the Fleet head rather than earning a rail row of their own
 * (spec §3.3).
 */

export interface SectionTab {
  id: string;
  label: string;
  to: string;
}

export const FLEET_TABS: SectionTab[] = [
  { id: "hosts", label: "Hosts", to: "/admin/fleet/hosts" },
  { id: "storage", label: "Storage", to: "/admin/fleet/storage" },
  { id: "jobs", label: "Jobs", to: "/admin/fleet/jobs" },
  { id: "releases", label: "Releases", to: "/admin/fleet/releases" },
];

export const LIBRARY_TABS: SectionTab[] = [
  { id: "apps", label: "Apps", to: "/admin/library/apps" },
  { id: "presets", label: "Runtime presets", to: "/admin/library/presets" },
  { id: "images", label: "Images", to: "/admin/library/images" },
  { id: "sources", label: "Sources", to: "/admin/library/sources" },
];

export const STREAMING_TABS: SectionTab[] = [
  { id: "launch", label: "Launch profiles", to: "/admin/streaming/launch" },
  { id: "profiles", label: "Stream profiles", to: "/admin/streaming/profiles" },
];

export const PEOPLE_TABS: SectionTab[] = [
  { id: "users", label: "Users", to: "/admin/people/users" },
  { id: "invites", label: "Invites", to: "/admin/people/invites" },
];

/**
 * Which tab owns `pathname` — the longest `to` that the path equals or sits
 * under. Drill-downs count: a host's settings page is still the Hosts tab.
 * Matching on segment boundaries, not raw prefixes, so `/fleet/hostsomething`
 * is nobody's tab rather than Hosts'.
 */
export function activeTab(tabs: SectionTab[], pathname: string): string | undefined {
  const path = pathname.split(/[?#]/, 1)[0].replace(/\/+$/, "");
  let best: SectionTab | undefined;
  for (const tab of tabs) {
    if (path !== tab.to && !path.startsWith(`${tab.to}/`)) continue;
    if (!best || tab.to.length > best.to.length) best = tab;
  }
  return best?.id;
}
