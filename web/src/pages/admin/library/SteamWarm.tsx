// Warm Steam (amendment 39): the three instance settings, next to Steam
// preparation. A parked Steam holds a GPU slot, hence off by default.

import { useState } from "react";
import type { SteamWarmRewarm } from "../../../api/types";
import { Button } from "../../../components/Button";
import { Switch } from "../../../components/TextField";
import type { UseSettingsResult } from "../settings/useSettings";

export const MIN_IDLE_MINUTES = 1;
export const MAX_IDLE_MINUTES = 60;

/** The whole number the field holds, or null when it is not 1-60. */
export function parseIdleMinutes(text: string): number | null {
  const n = Number(text);
  return text.trim() !== "" && Number.isInteger(n) && n >= MIN_IDLE_MINUTES && n <= MAX_IDLE_MINUTES ? n : null;
}

export function SteamWarm({ settings }: { settings: UseSettingsResult }) {
  const s = settings.settings;
  const [idleText, setIdleText] = useState<string | null>(null);
  const [idleError, setIdleError] = useState<string | null>(null);
  if (!s || typeof s.steam_warm_enabled !== "boolean") return null; // pre-amendment control plane

  const idleValue = idleText ?? String(s.steam_warm_idle_minutes ?? 10);
  const saveIdle = async () => {
    const n = parseIdleMinutes(idleValue);
    if (n === null) {
      setIdleError(`Enter a whole number of minutes between ${MIN_IDLE_MINUTES} and ${MAX_IDLE_MINUTES}.`);
      return;
    }
    setIdleError(null);
    if (await settings.patch("steam_warm_idle_minutes", n)) setIdleText(null);
  };

  return (
    <div className="col gap3 mt4">
      <div className="rowflex">
        <label htmlFor="steam-warm-enabled" className="label">Hot Loading for Steam Apps</label>
        <Switch id="steam-warm-enabled" aria-label="Hot Loading for Steam Apps"
          checked={s.steam_warm_enabled} disabled={settings.pending === "steam_warm_enabled"}
          onChange={(next) => void settings.patch("steam_warm_enabled", next)} />
      </div>
      <p className="hint m0">
        Hot Loading keeps a hidden Steam running while its user browses the library, which holds a GPU slot, so it is off by default.
      </p>
      <div className="rowflex">
        <label htmlFor="steam-warm-rewarm" className="label">Start Hot Loading again</label>
        <select id="steam-warm-rewarm" className="select" style={{ width: 260 }} value={s.steam_warm_rewarm ?? "library_open"}
          disabled={settings.pending === "steam_warm_rewarm"}
          onChange={(e) => void settings.patch("steam_warm_rewarm", e.target.value as SteamWarmRewarm)}>
          <option value="library_open">When the library opens</option>
          <option value="next_steam_launch">At the next Steam launch</option>
        </select>
      </div>
      <div className="rowflex">
        <label htmlFor="steam-warm-idle" className="label">Keep Hot Loading for (minutes)</label>
        <input id="steam-warm-idle" className="input" type="number" min={MIN_IDLE_MINUTES} max={MAX_IDLE_MINUTES}
          value={idleValue} style={{ width: 110 }} disabled={settings.pending === "steam_warm_idle_minutes"}
          onChange={(e) => { setIdleText(e.target.value); setIdleError(null); }} />
        <Button size="sm" aria-label="Save Hot Loading minutes" onClick={() => void saveIdle()}
          disabled={settings.pending === "steam_warm_idle_minutes"}>Save</Button>
      </div>
      {idleError && <p className="form-error" role="alert">{idleError}</p>}
    </div>
  );
}
