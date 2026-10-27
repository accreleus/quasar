// The account page's Region section (design_handoff_v3/screens/region-input-v3.html, frame 1).
// Each pick saves at once through PATCH /v1/me/region-input and the status line says what happened.
// The world map (#847) sits beside the timezone list; language and region format (#848) go
// between the timezone field and the status line.

import { useEffect, useMemo, useState } from "react";
import { ApiError } from "../../../../api/client";
import { getRegionInput, patchRegionInput, type RegionInputWire } from "../../../../api/me";
import { ResourceStates } from "../../../../components/ResourceStates";
import { useResource } from "../../../../lib/resource/react";
import { browserZone, buildZones, clockIn, formatOffset, offsetMinutes, zoneFor } from "../../../../lib/region/zones";
import { TimezonePicker } from "./TimezonePicker";

const CLOCK_TICK_MS = 30_000;
const IDLE_STEAM_CLOSED = " Your idle Steam was closed so it picks this up.";

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

  // The pick, shown at once and held until the save settles; it also holds the list shut, so one save runs at a time.
  const [pending, setPending] = useState<{ tz: string | null } | null>(null);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);

  const data = res.data;
  const saved = data?.region?.timezone ?? null;
  const shown = pending ? pending.tz : saved;
  const fallbackTz = data?.defaults?.region?.timezone ?? "UTC";
  const followedTz = browserTz ?? fallbackTz;
  const nowZone = zoneFor(shown ?? followedTz, zones, now);

  const save = async (tz: string | null) => {
    if (pending || tz === saved) return;
    setPending({ tz });
    setStatus(null);
    try {
      const r = await res.mutate(
        ({ token }) => patchRegionInput(token, { region: { timezone: tz } }),
        (_d, result) => result,
      );
      const base = tz
        ? `Saved. Your next launch starts at ${zoneFor(tz, zones, now).city} time.`
        : "Saved. Your next launch follows your browser's timezone.";
      setStatus({ ok: true, text: r.parked_session_stopped ? base + IDLE_STEAM_CLOSED : base });
    } catch (e) {
      setStatus({ ok: false, text: e instanceof ApiError ? e.message : "Could not save your timezone." });
    } finally {
      setPending(null);
    }
  };

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
                value={shown}
                followed={zoneFor(followedTz, zones, now)}
                disabled={pending !== null}
                onPick={(tz) => void save(tz)}
              />
            </div>
            <p className="muted mt3">
              Now: {nowZone.city} ({nowZone.tz}){shown === null ? followNote : ""} · {clockIn(nowZone.tz, now)} ·{" "}
              {formatOffset(offsetMinutes(nowZone.tz, now) ?? 0)}
            </p>
          </div>
          <p className={status && !status.ok ? "form-error mt3" : "muted mt3"} role="status">
            {pending ? "Saving…" : (status?.text ?? "Saved as you pick.")}
          </p>
        </>
      )}
    </div>
  );
}
