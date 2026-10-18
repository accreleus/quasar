// The library heartbeat (control-api.md §Warm Steam): PUT /v1/me/steam-warm once
// when the library opens, then every 60 s. It rides the resource machine, so the
// poll is a chained timeout that pauses while the tab is hidden and fires once on
// return. The answer is always 204 and means nothing; a failure is ignored (the
// machine re-arms the next beat on its own).

import { putSteamWarm } from "../../api/library";
import { useResource } from "../../lib/resource/react";

export const STEAM_WARM_BEAT_MS = 60_000;

export function useSteamWarmHeartbeat(): void {
  useResource<void>(
    {
      label: "Steam warm heartbeat",
      // async so the call settles to a promise whatever putSteamWarm returns.
      fetch: async ({ token, signal }) => {
        await putSteamWarm(token, signal);
      },
      pollMs: STEAM_WARM_BEAT_MS,
    },
    [],
  );
}
