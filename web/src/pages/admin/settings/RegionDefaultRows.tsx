// The rows of Admin ▸ Settings ▸ Region defaults (design_handoff_v3/screens/region-input-v3.html,
// frame 2). Language and region format (#848) join the timezone row.

import { useMemo } from "react";
import { buildZones, formatOffset, groupZones } from "../../../lib/region/zones";
import { SettingRow } from "./SettingRow";
import type { UseSettingsResult } from "./useSettings";

export function RegionDefaultRows({ settings }: { settings: UseSettingsResult }) {
  const groups = useMemo(() => groupZones(buildZones(new Date())), []);
  const tz = settings.settings?.default_timezone ?? "UTC";
  const listed = tz === "UTC" || groups.some(([, zs]) => zs.some((z) => z.tz === tz));

  return (
    <SettingRow
      label="Timezone"
      hint={'Used when a user\'s browser does not report one. Users on "Follow my browser" keep their browser\'s zone.'}
    >
      <select
        className="select admin-settings-select"
        aria-label="Default timezone"
        value={tz}
        disabled={settings.pending === "default_timezone"}
        onChange={(e) => void settings.patch("default_timezone", e.target.value)}
      >
        <option value="UTC">UTC (built-in fallback)</option>
        {!listed && <option value={tz}>{tz}</option>}
        {groups.map(([area, zs]) => (
          <optgroup key={area} label={area}>
            {zs.map((z) => (
              <option key={z.tz} value={z.tz}>
                {z.city}, {z.country} · {formatOffset(z.offset)}
              </option>
            ))}
          </optgroup>
        ))}
      </select>
    </SettingRow>
  );
}
