// Library provider registry (admin-libraries IA spec §1): one entry per
// library provider, driving the future Library -> Sources composition
// (Task 25, spec §3.3) — a second provider is one entry plus one page. W3's
// `library_providers` table replaces exactly this array with a fetch;
// consumers depend only on the shape.
//
// Not consumed by the v3 Settings page (../Settings.tsx has one generic
// `library_discovery_enabled` switch, not a per-provider list) or the v3
// rail (static — see AdminLayout.tsx's AdminOutletContext comment).
// `enabled()` is pure: callers pass freshly-fetched settings each time.

import type { InstanceSettings } from "../../api/types";

export interface LibraryProviderDef {
  /** Stable machine key. "steam" today; matches `AdminApp.library_provider`. */
  kind: string;
  /** Display label used in the nav and the Settings enablement row. */
  label: string;
  /** Route to this provider's dedicated admin page. */
  route: string;
  /** One-liner shown beside the enable toggle in Settings. */
  description: string;
  /** Whether this provider is currently enabled, given the instance settings. */
  enabled(settings: InstanceSettings): boolean;
}

export const LIBRARY_PROVIDERS: LibraryProviderDef[] = [
  {
    kind: "steam",
    label: "Steam",
    route: "/admin/library/sources",
    description:
      "Scan users' Steam installs and publish tiles automatically for the games they own.",
    enabled: (settings) => settings.library_discovery_enabled,
  },
];
