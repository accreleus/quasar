// Shared GET /v1/setup/status cache: one <SetupStatusProvider> fetch serves
// both the bootstrap gate and the /admin resume banner. `setStatus` lets
// SetupWizard apply the completion response so every consumer reflects it in
// the same render pass.

import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { getSetupStatus, type SetupStatus } from "../api/setup";

export interface SetupStatusContextValue {
  /** null while loading OR when the fetch failed — callers must treat both
   *  as "unknown" and fail open (never brick routing on a transient error). */
  status: SetupStatus | null;
  loading: boolean;
  error: boolean;
  /** Re-fetch from the server (e.g. after a status-changing action fails and
   *  a caller wants the authoritative value rather than trusting a guess). */
  refresh: () => Promise<void>;
  /** Overwrite the cached value directly — for applying a mutation response
   *  (POST /v1/setup/complete returns the new SetupStatus) without a round-trip. */
  setStatus: (status: SetupStatus) => void;
}

const SetupStatusContext = createContext<SetupStatusContextValue | null>(null);

export function SetupStatusProvider({ children }: { children: ReactNode }) {
  const [status, setStatusState] = useState<SetupStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const s = await getSetupStatus();
      setStatusState(s);
      setError(false);
    } catch {
      setError(true);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const value: SetupStatusContextValue = { status, loading, error, refresh, setStatus: setStatusState };
  return <SetupStatusContext.Provider value={value}>{children}</SetupStatusContext.Provider>;
}

export function useSetupStatus(): SetupStatusContextValue {
  const ctx = useContext(SetupStatusContext);
  if (!ctx) throw new Error("useSetupStatus must be used within <SetupStatusProvider>");
  return ctx;
}
