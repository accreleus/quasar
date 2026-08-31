/**
 * "Keep me signed in on this device" is a real choice, not a decoration:
 * checked persists the token in localStorage (it survives the browser being
 * closed), unchecked in sessionStorage (it dies with the tab). Whichever store
 * holds the token is the one loadSession() reads, and clearing empties both —
 * a sign-out must never leave a copy behind in the store that was not used.
 */

import { beforeEach, describe, expect, it } from "vitest";
import { clearSession, isRemembered, loadSession, saveSession, type PersistedSession } from "./storage";

const TOKEN_KEY = "quasar.auth.token";
const EXPIRES_KEY = "quasar.auth.expires_at";
const USER_KEY = "quasar.auth.user";

const session: PersistedSession = {
  token: "tok-1",
  expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  user: { id: "u1", email: "a@b.co", username: "ab", role: "user" },
};

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

describe("saveSession", () => {
  it("writes to sessionStorage and clears localStorage when not remembering", () => {
    localStorage.setItem(TOKEN_KEY, "stale");
    saveSession(session, { remember: false });

    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("tok-1");
    expect(sessionStorage.getItem(EXPIRES_KEY)).toBe(session.expiresAt);
    expect(JSON.parse(sessionStorage.getItem(USER_KEY) ?? "null")).toEqual(session.user);
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("writes to localStorage and clears sessionStorage when remembering", () => {
    sessionStorage.setItem(TOKEN_KEY, "stale");
    saveSession(session, { remember: true });

    expect(localStorage.getItem(TOKEN_KEY)).toBe("tok-1");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

describe("loadSession", () => {
  it("reads whichever store holds the token", () => {
    saveSession(session, { remember: false });
    expect(loadSession()).toEqual(session);
    expect(isRemembered()).toBe(false);

    clearSession();
    saveSession(session, { remember: true });
    expect(loadSession()).toEqual(session);
    expect(isRemembered()).toBe(true);
  });

  it("returns null with nothing stored", () => {
    expect(loadSession()).toBeNull();
    expect(isRemembered()).toBe(false);
  });

  it("drops an expired token from either store", () => {
    const expired = { ...session, expiresAt: new Date(Date.now() - 1000).toISOString() };
    saveSession(expired, { remember: false });
    expect(loadSession()).toBeNull();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("drops an unparseable cached user", () => {
    saveSession(session, { remember: true });
    localStorage.setItem(USER_KEY, "{not json");
    expect(loadSession()).toBeNull();
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

describe("clearSession", () => {
  it("empties both stores", () => {
    saveSession(session, { remember: true });
    sessionStorage.setItem(TOKEN_KEY, "other");
    clearSession();

    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});
