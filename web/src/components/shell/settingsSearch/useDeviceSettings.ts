// The signed-in devices as findable settings. Fetched once, and only when a
// search first needs them, so a page that never searches never asks.

import { useEffect, useState } from "react";
import * as authApi from "../../../api/auth";
import { useAuth } from "../../../auth/context";
import { deviceLabel } from "../../../pages/app/account/DeviceCard";
import { deviceSettings, type SettingEntry } from "./settingsIndex";

export function useDeviceSettings(wanted: boolean): SettingEntry[] {
  const { token } = useAuth();
  const [entries, setEntries] = useState<SettingEntry[] | null>(null);
  const need = wanted && entries === null && !!token;
  useEffect(() => {
    if (!need || !token) return;
    const ctl = new AbortController();
    authApi
      .listDevices(token, ctl.signal)
      .then((r) => setEntries(deviceSettings(r.devices.map((d) => ({ id: d.id, label: deviceLabel(d), current: d.current })))))
      .catch(() => {}); // devices are a bonus to the search; the rest still works without them
    return () => ctl.abort();
  }, [need, token]);
  return entries ?? [];
}
