// The console-config page's one read: host + console-config + app/user
// pickers, in parallel, on `web/src/lib/resource/`'s shared load/poll/error
// machine. Split out so HostConsole.tsx reads as the form, not the fetch.
//
// Polls every ~2s while the host's latest console-access report (amendment
// 18) is `applying` — a replacement is in flight through the recovery actor —
// and not otherwise, so an ordinary host never pays for a timer it has no use
// for.

import * as adminApi from "../../../../api/admin";
import type { AdminApp, AdminUser, ConsoleCapabilities, ConsoleConfig, Host } from "../../../../api/types";
import { useResource, type UseResourceResult } from "../../../../lib/resource/react";

export interface ConsoleLoadData {
  host: Host;
  config: ConsoleConfig;
  capabilities: ConsoleCapabilities;
  apps: AdminApp[];
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
        const [hostRes, consoleRes, appsRes, usersRes] = await Promise.all([
          adminApi.getHost(ctx.token, hostId),
          adminApi.getConsoleConfig(ctx.token, hostId),
          adminApi.listAdminApps(ctx.token),
          adminApi.listUsers(ctx.token),
        ]);
        return {
          host: hostRes.host,
          config: consoleRes.config,
          capabilities: consoleRes.capabilities,
          apps: appsRes.items,
          users: usersRes.items,
        };
      },
    },
    [hostId],
  );
}
