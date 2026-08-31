// Auth provider: owns the session token + user, persists across reloads, and
// re-validates against the server on mount so the role is always server-sourced.

import { useCallback, useEffect, useState, type ReactNode } from "react";
import * as authApi from "../api/auth";
import { claimSetup } from "../api/setup";
import { ApiError } from "../api/client";
import type { User } from "../api/types";
import { AuthContext, type AuthStatus, type AuthContextValue } from "./context";
import { clearSession, isRemembered, loadSession, saveSession } from "./storage";
import {
  deviceProbeIsFresh,
  getOrCreateDeviceKey,
  markDeviceProbePosted,
  probeCapabilities,
} from "../webrtc/capability";
import { reportBestEffortFailure } from "../lib/reportBestEffortFailure";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("loading");
  const [user, setUser] = useState<User | null>(null);
  const [token, setToken] = useState<string | null>(null);

  // On mount: rehydrate from storage, then confirm the token with GET /v1/me so
  // the role we act on is the server's, not the cached one. A 401 (revoked or
  // expired) clears the session.
  useEffect(() => {
    const persisted = loadSession();
    if (!persisted) {
      setStatus("unauthenticated");
      return;
    }

    let cancelled = false;
    const rememberedStore = isRemembered();
    setToken(persisted.token);
    setUser(persisted.user); // optimistic; replaced by the /me result below

    authApi
      .getMe(persisted.token)
      .then(({ user: fresh }) => {
        if (cancelled) return;
        setUser(fresh);
        // Re-write into whichever store the token came from: a rehydrate must
        // not silently promote a session-only token to a persistent one.
        saveSession({ token: persisted.token, expiresAt: persisted.expiresAt, user: fresh }, { remember: rememberedStore });
        setStatus("authenticated");

        // SPT-07: re-post the capability probe on rehydration so the session
        // envelope keeps a fresh measurement. Rehydration is every full page
        // load, so it is gated on the freshness window in capability.ts —
        // unthrottled it rate-limited /v1/me/devices (429). Best-effort:
        // failures are silent (same posture as the login path).
        if (deviceProbeIsFresh(fresh.id)) return;
        const tok = persisted.token;
        void (async () => {
          try {
            const deviceKey = getOrCreateDeviceKey();
            const caps = await probeCapabilities();
            await authApi.postDevice(tok, deviceKey, caps);
            markDeviceProbePosted(fresh.id);
          } catch (err) {
            reportBestEffortFailure("silent-debug", "auth: rehydrate device capability post", err);
          }
        })();
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        // Token rejected (or any failure validating it): fall back to signed-out.
        if (err instanceof ApiError && err.status === 401) {
          clearSession();
          setToken(null);
          setUser(null);
        }
        setStatus(err instanceof ApiError && err.status === 401 ? "unauthenticated" : "authenticated");
      });

    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (email: string, password: string, remember = true) => {
    // Send the persistent device_key so the minted token is bound to this device
    // (LP-SEC-01 §B.5) and can later be revoked from the account → Devices area.
    const deviceKey = getOrCreateDeviceKey();
    const res = await authApi.login(email, password, deviceKey);
    // `remember` is the sign-in form's "Keep me signed in on this device".
    saveSession({ token: res.access_token, expiresAt: res.expires_at, user: res.user }, { remember });
    setToken(res.access_token);
    setUser(res.user);
    setStatus("authenticated");

    // Fire the device capability probe asynchronously AFTER setting
    // authenticated state so the redirect to /app is never blocked.
    // Failures are silent (best-effort per P4-08 and control-api.md).
    const tok = res.access_token;
    void (async () => {
      try {
        const deviceKey = getOrCreateDeviceKey();
        const caps = await probeCapabilities();
        await authApi.postDevice(tok, deviceKey, caps);
        markDeviceProbePosted(res.user.id);
      } catch (err) {
        // Intentionally silent — capability data is best-effort and must never
        // degrade the login experience (P4-08: "failures silent/logged, not fatal").
        reportBestEffortFailure("silent-debug", "auth: device capability post", err);
      }
    })();
  }, []);

  const claim = useCallback(
    async (setupToken: string, email: string, username: string, password: string) => {
      // Same device_key as login (LP-SEC-01 §B.5) so the founding admin's
      // first token is device-bound; optional on the wire.
      const deviceKey = getOrCreateDeviceKey();
      const res = await claimSetup(setupToken, { email, username, password, device_key: deviceKey });
      // The wizard runs across several steps and a redeploy is a real risk
      // mid-setup: the founding admin's session is always the persistent kind.
      saveSession({ token: res.access_token, expiresAt: res.expires_at, user: res.user }, { remember: true });
      setToken(res.access_token);
      setUser(res.user);
      setStatus("authenticated");

      // Same best-effort capability probe as login(), fired AFTER the state
      // flip so the wizard's next step is never blocked on it. Failures are
      // silent (P4-08 posture).
      const tok = res.access_token;
      void (async () => {
        try {
          const caps = await probeCapabilities();
          await authApi.postDevice(tok, deviceKey, caps);
          markDeviceProbePosted(res.user.id);
        } catch (err) {
          reportBestEffortFailure("silent-debug", "auth: claim device capability post", err);
        }
      })();
    },
    [],
  );

  const logout = useCallback(async () => {
    const current = token;
    clearSession();
    setToken(null);
    setUser(null);
    setStatus("unauthenticated");
    if (current) {
      // Best-effort revoke; local state is already cleared regardless of outcome.
      try {
        await authApi.logout(current);
      } catch {
        /* ignore — the token is gone locally either way */
      }
    }
  }, [token]);

  const value: AuthContextValue = {
    status,
    user,
    token,
    isAdmin: user?.role === "admin",
    login,
    claim,
    logout,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
