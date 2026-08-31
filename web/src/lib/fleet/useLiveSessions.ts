/**
 * The live-session poll: `GET /v1/admin/sessions?state=active` every 5 seconds.
 *
 * `state=active` is the UI-v3 amendment (spec §6.1) and is the whole point of
 * this hook: filtering client-side broke silently past the first page of 100,
 * so the rail badge and the KPI would quietly under-count a busy fleet.
 *
 * Called once, by `FleetProvider` (./FleetContext.tsx) — see ./useFleet.ts.
 */

import * as adminApi from "../../api/admin";
import type { AdminSession } from "../../api/types";
import { useResource, type UseResourceResult } from "../resource/react";
import { FLEET_POLL_MS } from "./useFleet";

export function useLiveSessions(): UseResourceResult<AdminSession[]> {
  return useResource<AdminSession[]>(
    {
      label: "live sessions",
      pollMs: FLEET_POLL_MS,
      initialData: [],
      fetch: async ({ token }) =>
        (await adminApi.listAllSessions(token, undefined, { state: "active" })).items ?? [],
    },
    [],
  );
}
