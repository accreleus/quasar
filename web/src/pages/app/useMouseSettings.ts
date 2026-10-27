import { useMemo } from "react";
import { getRegionInput, type RegionInputWire } from "../../api/me";
import { listApps } from "../../api/library";
import type { App } from "../../api/types";
import { resolveMouseSettings, type MouseSettings } from "../../input/mouseScale";
import { useResource } from "../../lib/resource/react";

const NOT_LOADED = { input: null, apps: [] as App[] };

/** The running app's mouse speed. Either read failing leaves the default: the session is not held up by it. */
export function useMouseSettings(appId: string | undefined): MouseSettings {
  const res = useResource<{ input: RegionInputWire | null; apps: App[] }>({
    label: "your mouse settings",
    fetch: async ({ token }) => {
      const [input, apps] = await Promise.all([
        getRegionInput(token).catch(() => null),
        listApps(token).then((r) => r.items, () => [] as App[]),
      ]);
      return { input, apps };
    },
  });
  const { input, apps } = res.data ?? NOT_LOADED;
  return useMemo(
    () => resolveMouseSettings(input, apps.find((a) => a.id === appId)?.kind),
    [input, apps, appId],
  );
}
