// One PATCH per changed default, with the server's refusal shown under the row that sent it
// (a 400 names the value it did not accept). The control snaps back to the saved value because
// it is always read from the settings envelope, never held locally.
// One PATCH at a time: each response carries the whole envelope, so a late one from an earlier
// save would revert a row saved after it (and the keyboard row would then send a stale layout).

import { useRef, useState } from "react";
import type * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { UseSettingsResult } from "./useSettings";

type Patch = Parameters<typeof adminApi.updateSettings>[1];

export function useInlineSave(settings: UseSettingsResult) {
  const [inFlight, setInFlight] = useState<Record<string, number>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const queue = useRef<Promise<void>>(Promise.resolve());

  const count = (key: string, by: number) => setInFlight((n) => ({ ...n, [key]: (n[key] ?? 0) + by }));

  function save(key: keyof Patch, body: Patch): Promise<void> {
    count(key, 1);
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => k !== key)));
    const run = queue.current.then(async () => {
      try {
        await settings.patchOrThrow(body);
      } catch (e: unknown) {
        const message = e instanceof ApiError ? e.message : "Could not update this setting.";
        setErrors((er) => ({ ...er, [key]: message }));
      } finally {
        count(key, -1);
      }
    });
    queue.current = run;
    return run;
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
