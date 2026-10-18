// "Steam dialogs" (amendment 39): whether the launch helper may answer Steam's
// controller notice for you. Shared by both account layouts. Saves on change;
// the PATCH names only `steam_dialogs`, so the default profile is left alone.

import { useState } from "react";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { SteamDialogs } from "../../../api/types";
import { SelectField } from "../../../components/TextField";
import { useResource } from "../../../lib/resource/react";

export function SteamDialogsField() {
  const res = useResource<SteamDialogs>({
    label: "your Steam dialogs setting",
    fetch: async ({ token }) => (await libraryApi.getProfilePreferences(token)).steam_dialogs,
  });
  const [picked, setPicked] = useState<SteamDialogs | null>(null);
  const [error, setError] = useState<string | null>(null);

  const save = async (next: SteamDialogs) => {
    if (picked !== null) return;
    setPicked(next);
    setError(null);
    try {
      await res.mutate(
        ({ token }) => libraryApi.updateSteamDialogs(token, next),
        (_d, r) => r.steam_dialogs,
      );
    } catch (e: unknown) {
      setError(e instanceof ApiError ? e.message : "Could not save the Steam dialogs setting.");
    } finally {
      setPicked(null);
    }
  };

  return (
    <div>
      <SelectField
        label="Steam dialogs"
        value={picked ?? res.data ?? "show"}
        disabled={!res.data || picked !== null}
        onChange={(e) => void save(e.target.value as SteamDialogs)}
        hint="Answers only Steam's controller notice. Sign-in, Steam Guard, cloud conflicts and installs are always shown. A change takes effect at the next Steam start."
      >
        <option value="show">Show Steam's dialogs</option>
        <option value="dismiss_safe">Dismiss safe ones automatically</option>
      </SelectField>
      {(error ?? res.errorMessage) && <p className="form-error mt3" role="alert">{error ?? res.errorMessage}</p>}
    </div>
  );
}
