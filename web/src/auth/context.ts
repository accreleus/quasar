// Auth context definition + hook, kept separate from the provider component so
// the provider file can fast-refresh cleanly (it exports only a component).

import { createContext, useContext } from "react";
import type { User } from "../api/types";

export type AuthStatus = "loading" | "authenticated" | "unauthenticated";

export interface AuthContextValue {
  status: AuthStatus;
  user: User | null;
  token: string | null;
  /** True once the server has confirmed the user's role is "admin". */
  isAdmin: boolean;
  /**
   * Authenticate against control-api.md and persist the session. Throws ApiError.
   *
   * `remember` is the sign-in form's "Keep me signed in on this device": true
   * (the default, and the behaviour before the choice existed) keeps the token
   * in localStorage across browser restarts, false keeps it in sessionStorage
   * so it dies with the tab. See auth/storage.ts.
   */
  login: (email: string, password: string, remember?: boolean) => Promise<void>;
  /**
   * First-run claim (control-api.md "First-run setup" amendment, Spec B W1).
   * Creates the first admin via the per-boot setup token and persists the
   * session exactly as `login` does — the response is login-shaped, so the
   * operator ends up signed in as their new admin. Throws ApiError (401 bad
   * token, 409 already claimed, 400 weak password).
   */
  claim: (setupToken: string, email: string, username: string, password: string) => Promise<void>;
  /** Revoke the token server-side (best-effort) and clear local session. */
  logout: () => Promise<void>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within <AuthProvider>");
  return ctx;
}
