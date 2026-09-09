/**
 * Which 401s end the session (#154).
 *
 * The distinction is the whole point: a 401 on a request that carried a bearer
 * token means the SESSION is over and the app must sign out; a 401 on one that
 * did not is a statement about credentials just typed, and must stay in the
 * sign-in form. Getting this wrong in the permissive direction makes a wrong
 * password log the user out of a session they never had.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiFetch, ApiError } from "./client";
import { onUnauthorized } from "./unauthorized";

const seen = vi.fn();
let off: () => void;

function reply(status: number, code = "invalid_token") {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: new Headers(),
    json: async () => ({ error: { code, message: "invalid or expired token" } }),
    text: async () => "",
  } as unknown as Response;
}

beforeEach(() => {
  seen.mockClear();
  off = onUnauthorized(seen);
});
afterEach(() => {
  off();
  vi.unstubAllGlobals();
});

describe("apiFetch and a rejected token", () => {
  it("announces a 401 on a request that carried a bearer token", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(401)));

    await expect(apiFetch("/me", { token: "tok-1" })).rejects.toBeInstanceOf(ApiError);
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("says nothing about a 401 on a request with no bearer token", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(401, "invalid_credentials")));

    // This is POST /v1/auth/login with a wrong password.
    await expect(apiFetch("/auth/login", { body: { email: "a@b.co" } })).rejects.toBeInstanceOf(
      ApiError,
    );
    expect(seen).not.toHaveBeenCalled();
  });

  it("says nothing about a 403 — the session is valid, the action is not", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(reply(403, "forbidden")));

    await expect(apiFetch("/admin/hosts", { token: "tok-1" })).rejects.toBeInstanceOf(ApiError);
    expect(seen).not.toHaveBeenCalled();
  });
});
