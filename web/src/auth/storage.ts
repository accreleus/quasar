// Persistence of the session token + cached user across reloads. The token is
// the opaque bearer from control-api.md, persisted so a refresh keeps the user
// signed in until it expires or is revoked.
//
// Which store holds it is the user's choice, made on the sign-in form: "Keep me
// signed in on this device" checked writes localStorage (survives closing the
// browser, today's behaviour), unchecked writes sessionStorage (dies with the
// tab). Exactly one store ever holds a session — every write clears the other,
// so there is no stale copy for a later load to resurrect, and clearSession()
// empties both.
//
// The cached user is a UX convenience only (instant rehydrate). It is NEVER the
// source of truth for authorization — the server re-validates the token and
// returns the authoritative role on every load via GET /v1/me. A tampered cached
// `role: "admin"` buys nothing: admin API calls still 401/403 server-side.

import type { User } from "../api/types";

const TOKEN_KEY = "quasar.auth.token";
const EXPIRES_KEY = "quasar.auth.expires_at";
const USER_KEY = "quasar.auth.user";

export interface PersistedSession {
  token: string;
  expiresAt: string;
  user: User;
}

export interface SaveOptions {
  /** true → localStorage (persists across browser restarts); false → sessionStorage. */
  remember: boolean;
}

/** The store currently holding a token, or null when neither does. */
function activeStore(): Storage | null {
  if (localStorage.getItem(TOKEN_KEY)) return localStorage;
  if (sessionStorage.getItem(TOKEN_KEY)) return sessionStorage;
  return null;
}

/** True when the stored session is the persistent kind — i.e. "keep me signed
 *  in" was chosen. False when there is no session at all. */
export function isRemembered(): boolean {
  return activeStore() === localStorage;
}

export function loadSession(): PersistedSession | null {
  const store = activeStore();
  if (!store) return null;

  const token = store.getItem(TOKEN_KEY);
  const expiresAt = store.getItem(EXPIRES_KEY);
  const userRaw = store.getItem(USER_KEY);
  if (!token || !expiresAt || !userRaw) return null;

  // Drop an obviously-expired token without a network round-trip.
  if (Date.parse(expiresAt) <= Date.now()) {
    clearSession();
    return null;
  }

  try {
    return { token, expiresAt, user: JSON.parse(userRaw) as User };
  } catch {
    clearSession();
    return null;
  }
}

export function saveSession(session: PersistedSession, { remember }: SaveOptions): void {
  const target = remember ? localStorage : sessionStorage;
  const other = remember ? sessionStorage : localStorage;

  target.setItem(TOKEN_KEY, session.token);
  target.setItem(EXPIRES_KEY, session.expiresAt);
  target.setItem(USER_KEY, JSON.stringify(session.user));

  other.removeItem(TOKEN_KEY);
  other.removeItem(EXPIRES_KEY);
  other.removeItem(USER_KEY);
}

export function clearSession(): void {
  for (const store of [localStorage, sessionStorage]) {
    store.removeItem(TOKEN_KEY);
    store.removeItem(EXPIRES_KEY);
    store.removeItem(USER_KEY);
  }
}
