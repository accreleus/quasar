// The console-config page's one load effect: host + console-config +
// app/user pickers, in parallel. Split out so HostConsole.tsx reads as the
// form, not the fetch.

import { useEffect, useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AdminApp, AdminUser, ConsoleCapabilities, ConsoleConfig, Host } from "../../../../api/types";
import { useAuth } from "../../../../auth/context";

export interface ConsoleLoadState {
  host: Host | null;
  config: ConsoleConfig | null;
  capabilities: ConsoleCapabilities | null;
  apps: AdminApp[];
  users: AdminUser[];
  loading: boolean;
  error: string | null;
  setError: (message: string | null) => void;
  /** Applies a freshly-saved config+capabilities pair without a re-fetch. */
  setLoaded: (config: ConsoleConfig, capabilities: ConsoleCapabilities) => void;
}

export function useConsoleLoad(id: string | undefined): ConsoleLoadState {
  const { token } = useAuth();
  const [host, setHost] = useState<Host | null>(null);
  const [config, setConfig] = useState<ConsoleConfig | null>(null);
  const [capabilities, setCapabilities] = useState<ConsoleCapabilities | null>(null);
  const [apps, setApps] = useState<AdminApp[]>([]);
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!token || !id) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    void (async () => {
      try {
        const [hostRes, consoleRes, appsRes, usersRes] = await Promise.all([
          adminApi.getHost(token, id),
          adminApi.getConsoleConfig(token, id),
          adminApi.listAdminApps(token),
          adminApi.listUsers(token),
        ]);
        if (cancelled) return;
        setHost(hostRes.host);
        setConfig(consoleRes.config);
        setCapabilities(consoleRes.capabilities);
        setApps(appsRes.items);
        setUsers(usersRes.items);
      } catch (e: unknown) {
        if (!cancelled) {
          setError(e instanceof ApiError ? e.message : "Could not load console config.");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [id, token]);

  return {
    host,
    config,
    capabilities,
    apps,
    users,
    loading,
    error,
    setError,
    setLoaded: (nextConfig, nextCapabilities) => {
      setConfig(nextConfig);
      setCapabilities(nextCapabilities);
    },
  };
}
