// The rows of Admin ▸ Settings ▸ Region defaults (design_handoff_v3/screens/region-input-v3.html,
// frame 2): timezone, language and region format.

import { useMemo } from "react";
import { LANGUAGES, REGION_FORMATS, formatExample } from "../../../lib/region/options";
import { buildZones, formatOffset, groupZones } from "../../../lib/region/zones";
import { SettingRow } from "./SettingRow";
import { useInlineSave } from "./useInlineSave";
import type { UseSettingsResult } from "./useSettings";

/** The list, plus the saved value as itself when the list does not carry it. */
function withSaved(list: readonly (readonly [string, string])[], saved: string) {
  return list.some(([v]) => v === saved) ? list : [...list, [saved, saved] as const];
}

export function RegionDefaultRows({ settings }: { settings: UseSettingsResult }) {
  const { save, busy, errorFor } = useInlineSave(settings);
  const now = useMemo(() => new Date(), []);
  const groups = useMemo(() => groupZones(buildZones(now)), [now]);
  const tz = settings.settings?.default_timezone ?? "UTC";
  const listed = tz === "UTC" || groups.some(([, zs]) => zs.some((z) => z.tz === tz));
  const language = settings.settings?.default_language ?? "en-US";
  const format = settings.settings?.default_region_format ?? "";
  const languages = withSaved(LANGUAGES, language);
  const formats = format ? withSaved(REGION_FORMATS, format) : REGION_FORMATS;
  const languageName = languages.find(([v]) => v === language)?.[1] ?? language;

  return (
    <>
      <SettingRow
        label="Timezone"
        hint={'Used when a user\'s browser does not report one. Users on "Follow my browser" keep their browser\'s zone.'}
      >
        <div className="col gap2 admin-settings-end">
          <select
            className="select admin-settings-select"
            aria-label="Default timezone"
            value={tz}
            disabled={busy("default_timezone")}
            onChange={(e) => void save("default_timezone", { default_timezone: e.target.value })}
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
          {errorFor("default_timezone")}
        </div>
      </SettingRow>
      <SettingRow label="Language" hint="Games and Steam start in it where they have it">
        <div className="col gap2 admin-settings-end">
          <select
            className="select admin-settings-select"
            aria-label="Default language"
            value={language}
            disabled={busy("default_language")}
            onChange={(e) => void save("default_language", { default_language: e.target.value })}
          >
            {languages.map(([v, name]) => (
              <option key={v} value={v}>{name}</option>
            ))}
          </select>
          {errorFor("default_language")}
        </div>
      </SettingRow>
      <SettingRow
        label="Region format"
        hint={
          <>
            Dates, numbers and time:
            <span className="admin-settings-example">{formatExample(format || language, tz, now)}</span>
          </>
        }
      >
        <div className="col gap2 admin-settings-end">
          <select
            className="select admin-settings-select"
            aria-label="Default region format"
            value={format}
            disabled={busy("default_region_format")}
            onChange={(e) => void save("default_region_format", { default_region_format: e.target.value || null })}
          >
            <option value="">Same as language ({languageName})</option>
            {formats.map(([v, name]) => (
              <option key={v} value={v}>{name}</option>
            ))}
          </select>
          {errorFor("default_region_format")}
        </div>
      </SettingRow>
    </>
  );
}
