/**
 * The fleet poll: one `GET /v1/hosts` every 5 seconds, for the whole console.
 *
 * Called once, by `FleetProvider` (./FleetContext.tsx). Overview, Fleet, the
 * rail's fault badge and the command palette all read the same array from the
 * context, so there is one timer, one error surface, and no way for two
 * surfaces to disagree about how many hosts are online.
 *
 * `initialData: []` seeds the shape without claiming to have loaded: the
 * resource still reports `status: "loading"` until the first response.
 */

import * as adminApi from "../../api/admin";
import type { Host } from "../../api/types";
import { useResource, type UseResourceResult } from "../resource/react";

/** Shared by both console polls — the mock's "updated 3 seconds ago" cadence. */
export const FLEET_POLL_MS = 5000;

export function useFleet(): UseResourceResult<Host[]> {
  return useResource<Host[]>(
    {
      label: "fleet",
      pollMs: FLEET_POLL_MS,
      initialData: [],
      fetch: async ({ token }) => (await adminApi.listHosts(token)).items,
    },
    [],
  );
}
