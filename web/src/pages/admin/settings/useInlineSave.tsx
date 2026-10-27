// Per-row state for a default that saves as it changes: busy from the click until its own
// save settles, and the server's refusal (a 400 names the value it did not accept) under the
// row that sent it. The control snaps back to the saved value because it is always read from
// the settings envelope, never held locally. Ordering is `useSettings.patchOrThrow`'s job.

import { useState } from "react";
import type * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { UseSettingsResult } from "./useSettings";

type Patch = Parameters<typeof adminApi.updateSettings>[1];

export function useInlineSave(settings: UseSettingsResult) {
  const [inFlight, setInFlight] = useState<Record<string, number>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});

  const count = (key: string, by: number) => setInFlight((n) => ({ ...n, [key]: (n[key] ?? 0) + by }));

  async function save(key: keyof Patch, body: Patch): Promise<void> {
    count(key, 1);
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => k !== key)));
    try {
      await settings.patchOrThrow(body);
    } catch (e: unknown) {
      const message = e instanceof ApiError ? e.message : "Could not update this setting.";
      setErrors((er) => ({ ...er, [key]: message }));
    } finally {
      count(key, -1);
    }
  }

  /** True from the click until this row's own save has settled. */
  const busy = (key: keyof Patch) => (inFlight[key] ?? 0) > 0;

  const errorFor = (key: keyof Patch) =>
    errors[key] ? (
      <p className="form-error admin-settings-error" role="alert">
        {errors[key]}
      </p>
    ) : null;

  return { save, busy, errorFor };
}
