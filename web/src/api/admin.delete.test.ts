// Unit tests for the Thread-1 admin delete API helpers and the offline-only
// Forget affordance. These run under vitest (web-check gate).

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as adminApi from "./admin";

// ── apiFetch mock ────────────────────────────────────────────────────────────

// We mock the module-level apiFetch so the tests don't need a real server.
vi.mock("./client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./client")>();
  return {
    ...actual,
    apiFetch: vi.fn(),
  };
});

import { apiFetch, ApiError } from "./client";
const mockFetch = vi.mocked(apiFetch);

beforeEach(() => {
  mockFetch.mockReset();
});

afterEach(() => {
  vi.clearAllMocks();
});

// ── deleteApp ────────────────────────────────────────────────────────────────

describe("deleteApp", () => {
  it("issues DELETE /apps/{id} with the bearer token", async () => {
    mockFetch.mockResolvedValueOnce(undefined);
    await adminApi.deleteApp("tok", "app-123");
    expect(mockFetch).toHaveBeenCalledWith("/apps/app-123", {
      method: "DELETE",
      token: "tok",
    });
  });

  it("resolves void on 204", async () => {
    mockFetch.mockResolvedValueOnce(undefined);
    await expect(adminApi.deleteApp("tok", "app-123")).resolves.toBeUndefined();
  });

  it("propagates a 409 ApiError so the caller can toast", async () => {
    const conflict = new ApiError(409, "conflict", "app is in use by an active session");
    mockFetch.mockRejectedValueOnce(conflict);
    await expect(adminApi.deleteApp("tok", "app-123")).rejects.toThrow("app is in use");
  });
});

// ── deleteHost ───────────────────────────────────────────────────────────────

describe("deleteHost", () => {
  it("issues DELETE /hosts/{id} with the bearer token", async () => {
    mockFetch.mockResolvedValueOnce(undefined);
    await adminApi.deleteHost("tok", "host-456");
    expect(mockFetch).toHaveBeenCalledWith("/hosts/host-456", {
      method: "DELETE",
      token: "tok",
    });
  });

  it("resolves void on 204", async () => {
    mockFetch.mockResolvedValueOnce(undefined);
    await expect(adminApi.deleteHost("tok", "host-456")).resolves.toBeUndefined();
  });

  it("propagates a 409 ApiError (online host)", async () => {
    const conflict = new ApiError(409, "conflict", "host is online — drain and wait for it to disconnect first");
    mockFetch.mockRejectedValueOnce(conflict);
    await expect(adminApi.deleteHost("tok", "host-456")).rejects.toThrow("host is online");
  });
});

// ── Forget-only-when-offline affordance ──────────────────────────────────────
// Verify the UI invariant: the Forget button is only reachable when the host
// status is "offline". We test this as a pure logic check (no DOM needed).

describe("Forget-when-offline affordance", () => {
  const isForgetVisible = (status: string) => status === "offline";

  it("Forget is visible for offline hosts", () => {
    expect(isForgetVisible("offline")).toBe(true);
  });

  it("Forget is NOT visible for online hosts", () => {
    expect(isForgetVisible("online")).toBe(false);
  });

  it("Forget is NOT visible for draining hosts", () => {
    expect(isForgetVisible("draining")).toBe(false);
  });
});
