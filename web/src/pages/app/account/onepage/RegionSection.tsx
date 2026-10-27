// The account page's Region section (design_handoff_v3/screens/region-input-v3.html, frame 1).
// Each pick saves at once through PATCH /v1/me/region-input and the status line says what happened.
// The world map (#847) sits beside the timezone list; language and region format (#848) sit
// under it, as two selects.

import { useEffect, useMemo, useState } from "react";
import { ApiError } from "../../../../api/client";
import { getRegionInput, patchRegionInput, type RegionInputWire } from "../../../../api/me";
import { ResourceStates } from "../../../../components/ResourceStates";
import { SelectField } from "../../../../components/TextField";
import { LANGUAGES, REGION_FORMATS, formatExample, withSaved } from "../../../../lib/region/options";
import { useResource } from "../../../../lib/resource/react";
import { browserZone, buildZones, clockIn, offsetLabel, zoneFor } from "../../../../lib/region/zones";
import { probeRegion } from "../../../../webrtc/capability";
import { TimezonePicker } from "./TimezonePicker";

const CLOCK_TICK_MS = 30_000;
const IDLE_STEAM_CLOSED = " Your idle Steam was closed so it picks this up.";

type RegionPatch = NonNullable<RegionInputWire["region"]>;

const nameOf = (list: readonly (readonly [string, string])[], tag: string) => list.find(([v]) => v === tag)?.[1] ?? tag;

export function RegionSection() {
  const res = useResource<RegionInputWire>({
    label: "your region settings",
    fetch: ({ token }) => getRegionInput(token),
  });
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), CLOCK_TICK_MS);
    return () => clearInterval(t);
  }, []);
  const zones = useMemo(() => buildZones(new Date()), []);
  const browserTz = useMemo(browserZone, []);
  const browserLang = useMemo(() => probeRegion().language ?? null, []);

  // The pick, shown at once and held until the save settles; it also holds the list shut, so one save runs at a time.
  const [pending, setPending] = useState<RegionPatch | null>(null);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);

  const data = res.data;
  const shown = <K extends keyof RegionPatch>(key: K): string | null =>
    (pending && key in pending ? pending[key] : data?.region?.[key]) ?? null;
  const savedTz = data?.region?.timezone ?? null;
  const tz = shown("timezone");
  const fallbackTz = data?.defaults?.region?.timezone ?? "UTC";
  const followedTz = browserTz ?? fallbackTz;
  const nowZone = zoneFor(tz ?? followedTz, zones, now);

  const language = shown("language");
  const followedLang = browserLang ?? data?.defaults?.region?.language ?? "en-US";
  const languages = language ? withSaved(LANGUAGES, language) : LANGUAGES;
  const format = shown("region_format");
  const formats = format ? withSaved(REGION_FORMATS, format) : REGION_FORMATS;
  // A user with no format of their own gets the install's format, and only then the language.
  const installFormat = data?.defaults?.region?.region_format ?? null;
  const example = formatExample(format ?? installFormat ?? language ?? followedLang, nowZone.tz, now);

  const save = async (patch: RegionPatch, ok: string, failed: string) => {
    if (pending) return;
    setPending(patch);
    setStatus(null);
    try {
      const r = await res.mutate(
        ({ token }) => patchRegionInput(token, { region: patch }),
        (_d, result) => result,
      );
      setStatus({ ok: true, text: r.parked_session_stopped ? ok + IDLE_STEAM_CLOSED : ok });
    } catch (e) {
      setStatus({ ok: false, text: e instanceof ApiError ? e.message : failed });
    } finally {
      setPending(null);
    }
  };

  const saveTz = (next: string | null) => {
    if (next === savedTz) return;
    const ok = next
      ? `Saved. Your next launch starts at ${zoneFor(next, zones, now).city} time.`
      : "Saved. Your next launch follows your browser's timezone.";
    void save({ timezone: next }, ok, "Could not save your timezone.");
  };
  const saveLanguage = (next: string | null) =>
    void save(
      { language: next },
      next ? `Saved. Your next launch starts in ${nameOf(LANGUAGES, next)}.` : "Saved. Your next launch follows your browser's language.",
      "Could not save your language.",
    );
  const saveFormat = (next: string | null) =>
    void save({ region_format: next }, "Saved. Applies from your next launch.", "Could not save your region format.");

  const followNote = browserTz ? ", following your browser" : ", the install default";
  return (
    <div className="card sec-card">
      <ResourceStates loading={res.loading} error={res.errorMessage} />
      {data && (
        <>
          <div className="field">
            <span className="label" id="tz-label">Timezone</span>
            <div className="tz-pick">
              <TimezonePicker
                zones={zones}
                value={tz}
                followed={zoneFor(followedTz, zones, now)}
                disabled={pending !== null}
                onPick={saveTz}
              />
            </div>
            <p className="muted mt3">
              Now: {nowZone.city} ({nowZone.tz}){tz === null ? followNote : ""}
              {nowZone.offset !== null && ` · ${clockIn(nowZone.tz, now)} · ${offsetLabel(nowZone)}`}
            </p>
          </div>
          <div className="grid g2 mt5 os-field-wide">
            <SelectField
              label="Language"
              id="region-language"
              value={language ?? ""}
              onChange={(e) => saveLanguage(e.target.value || null)}
              hint="Games and Steam start in this language where they have it."
            >
              <option value="">
                Follow my browser ({browserLang ? nameOf(LANGUAGES, browserLang) : `install default: ${nameOf(LANGUAGES, followedLang)}`})
              </option>
              {languages.map(([v, name]) => (
                <option key={v} value={v}>{name}</option>
              ))}
            </SelectField>
            <SelectField
              label="Region format"
              id="region-format"
              value={format ?? ""}
              onChange={(e) => saveFormat(e.target.value || null)}
              hint={
                <>
                  For dates, numbers and time{example && ", like"}
                  {example && <span className="rg-ex">{example}</span>}
                </>
              }
            >
              <option value="">
                {installFormat
                  ? `Install default (${nameOf(REGION_FORMATS, installFormat)})`
                  : `Same as language (${nameOf(languages, language ?? followedLang)})`}
              </option>
              {formats.map(([v, name]) => (
                <option key={v} value={v}>{name}</option>
              ))}
            </SelectField>
          </div>
          <p className={status && !status.ok ? "form-error mt3" : "muted mt3"} role="status">
            {pending ? "Saving…" : (status?.text ?? "Saved as you pick.")}
          </p>
        </>
      )}
    </div>
  );
}
