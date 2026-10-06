// The console-config page's one read: host + console-config (with its
// default-app list) + the user picker, in parallel, on `web/src/lib/resource/`'s shared load/poll/error
// machine. Split out so HostConsole.tsx reads as the form, not the fetch.
//
// Polls every ~2s while the host's latest console-access report (amendment
// 18) is `applying` — a replacement is in flight through the recovery actor —
// and not otherwise, so an ordinary host never pays for a timer it has no use
// for.

import * as adminApi from "../../../../api/admin";
import type {
  AdminUser,
  ConsoleCapabilities,
  ConsoleConfig,
  ConsoleDefaultApp,
  Host,
  ReadinessCheck,
} from "../../../../api/types";
import { useResource, type UseResourceResult } from "../../../../lib/resource/react";

export interface ConsoleLoadData {
  host: Host;
  config: ConsoleConfig;
  capabilities: ConsoleCapabilities;
  /** Amendment 19: the apps the default-app pick may name (they can run direct). */
  defaultApps: ConsoleDefaultApp[];
  /** Amendment 19: the control plane's console readiness checks. */
  readiness: ReadinessCheck[];
  users: AdminUser[];
}

const APPLYING_POLL_MS = 2000;

export function useConsoleLoad(id: string | undefined): UseResourceResult<ConsoleLoadData> {
  const hostId = id ?? "";
  return useResource<ConsoleLoadData>(
    {
      label: "console config",
      pollMs: (data) => (data.capabilities.access?.state === "applying" ? APPLYING_POLL_MS : null),
      fetch: async (ctx) => {
        const [hostRes, consoleRes, usersRes] = await Promise.all([
          adminApi.getHost(ctx.token, hostId),
          adminApi.getConsoleConfig(ctx.token, hostId),
          adminApi.listUsers(ctx.token),
        ]);
        return {
          host: hostRes.host,
          config: consoleRes.config,
          capabilities: consoleRes.capabilities,
          // `?? []`: an older control plane predates amendment 19's envelope.
          defaultApps: consoleRes.default_apps ?? [],
          readiness: consoleRes.readiness ?? [],
          users: usersRes.items,
        };
      },
    },
    [hostId],
  );
}
