// One PATCH per changed default, with the server's refusal shown under the row that sent it
// (a 400 names the value it did not accept). The control snaps back to the saved value because
// it is always read from the settings envelope, never held locally.

import { useState } from "react";
import type * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { UseSettingsResult } from "./useSettings";

type Patch = Parameters<typeof adminApi.updateSettings>[1];

export function useInlineSave(settings: UseSettingsResult) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<{ key: string; message: string } | null>(null);

  async function save(key: keyof Patch, body: Patch) {
    setBusy(key);
    setError(null);
    try {
      await settings.patchOrThrow(body);
    } catch (e: unknown) {
      setError({ key, message: e instanceof ApiError ? e.message : "Could not update this setting." });
    } finally {
      setBusy(null);
    }
  }

  const errorFor = (key: keyof Patch) =>
    error?.key === key ? (
      <p className="form-error admin-settings-error" role="alert">
        {error.message}
      </p>
    ) : null;

  return { save, busy, errorFor };
}
