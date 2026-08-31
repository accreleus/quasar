// Pure helpers for the Runtime presets admin page (UI-P3).

import type { PresetUser } from "../../../api/types";

/**
 * Client-side half of "delete is disabled while the preset is in use" —
 * see admin-runtime-presets.html and notes §13. This is a UX affordance
 * ONLY: the server still 409s on DELETE while any app references the
 * preset (control-plane's ON DELETE RESTRICT is the real enforcement), so
 * a stale `used_by` snapshot never lets an in-use preset actually be
 * removed — it just means the button briefly disagrees with the server
 * until the next list refresh.
 */
export function isPresetInUse(usedBy: PresetUser[]): boolean {
  return usedBy.length > 0;
}
