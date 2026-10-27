// useSettings — the instance-settings envelope (GET /v1/admin/settings) plus
// the one pointer-PATCH action every row on the Settings page shares. One
// resource, one write path: two rows can never show snapshots from different
// responses, and a PATCH only ever names the key it changed (control-api.md
// pointer-decode rule) so two rows saving concurrently cannot clobber
// each other.

import { useCallback, useRef } from "react";
import * as adminApi from "../../../api/admin";
import type { InstanceSettings } from "../../../api/types";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";

type SettingsPatch = Required<Parameters<typeof adminApi.updateSettings>[1]>;
type SettingsKey = keyof SettingsPatch;

export interface UseSettingsResult {
  settings: InstanceSettings | null;
  loading: boolean;
  error: string | null;
  /** PATCHes exactly the one changed key. Never throws — failure goes to a
   *  toast, like every other useAdminAction write in this app. */
  patch: <K extends SettingsKey>(key: K, value: SettingsPatch[K]) => Promise<boolean>;
  /** Same PATCH, raw: resolves the updated settings or rejects with the
   *  server's error. For the one row (Allowed origins) that needs a 400
   *  shown inline rather than only in a toast. */
  patchOrThrow: (body: Partial<SettingsPatch>) => Promise<InstanceSettings>;
  /** The key currently saving via `patch`, or null. */
  pending: SettingsKey | null;
}

export function useSettings(): UseSettingsResult {
  const resource = useResource<InstanceSettings>(
    {
      label: "instance settings",
      fetch: async ({ token, signal }) => (await adminApi.getSettings(token, signal)).settings,
    },
    [],
  );

  // One PATCH at a time for the whole page: every response replaces the whole envelope, so a
  // late answer to an earlier write would revert a row saved after it (and a keyboard row would
  // then send a stale layout). A failed write rejects to its caller and never wedges the chain.
  const queue = useRef<Promise<unknown>>(Promise.resolve());
  const patchOrThrow = useCallback(
    (body: Partial<SettingsPatch>): Promise<InstanceSettings> => {
      const run = queue.current.then(async () => {
        const { settings } = await resource.mutate(
          (ctx) => adminApi.updateSettings(ctx.token, body),
          (_data, result) => result.settings,
        );
        return settings;
      });
      queue.current = run.catch(() => undefined);
      return run;
    },
    [resource],
  );

  // Plain-string `failure`: useAdminAction already extracts `.message` for an
  // ApiError and falls back to this copy otherwise (action.ts), so a custom
  // function here would only re-implement that branch.
  const action = useAdminAction<[SettingsKey, SettingsPatch[SettingsKey]], InstanceSettings>(
    (key, value) => patchOrThrow({ [key]: value } as Partial<SettingsPatch>),
    { failure: "could not update settings" },
  );

  const patch = useCallback(
    <K extends SettingsKey>(key: K, value: SettingsPatch[K]) => action.run(key, value),
    [action],
  );

  return {
    settings: resource.data ?? null,
    loading: resource.loading,
    error: resource.errorMessage,
    patch,
    patchOrThrow,
    // action.pending is the args of the newest unfinished run (action.ts) —
    // `[0]` is that run's key, so a second patch started while one is still
    // in flight reports the second key, not the first.
    pending: action.pending?.[0] ?? null,
  };
}
