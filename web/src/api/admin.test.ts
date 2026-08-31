// Query-string building for the UI v3 admin filters. These helpers are the only
// place a filter becomes a URL, and the rule they encode is that an absent
// filter is an absent parameter — an empty one is a value the server would have
// to interpret.

import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./client")>();
  return { ...actual, apiFetch: vi.fn() };
});

import { apiFetch } from "./client";
import * as adminApi from "./admin";

const mockFetch = vi.mocked(apiFetch);

beforeEach(() => {
  mockFetch.mockReset();
  mockFetch.mockResolvedValue(undefined as never);
});

/** The path the client asked for. */
function calledPath(): string {
  return mockFetch.mock.calls[0][0] as string;
}

describe("listAllSessions", () => {
  it("sends ?state= when asked", async () => {
    await adminApi.listAllSessions("tok", undefined, { state: "active" });
    expect(calledPath()).toBe("/admin/sessions?state=active");
  });

  it("sends no query at all with no arguments — the pre-amendment request", async () => {
    await adminApi.listAllSessions("tok");
    expect(calledPath()).toBe("/admin/sessions");
  });

  it("keeps the existing cursor call site working, and composes with state", async () => {
    await adminApi.listAllSessions("tok", "100");
    expect(calledPath()).toBe("/admin/sessions?cursor=100");

    mockFetch.mockReset();
    mockFetch.mockResolvedValue(undefined as never);
    await adminApi.listAllSessions("tok", "100", { state: "failed", limit: 25 });
    expect(calledPath()).toBe("/admin/sessions?cursor=100&state=failed&limit=25");
  });
});

describe("listAdminActivity", () => {
  it("encodes every filter and omits the undefined ones", async () => {
    await adminApi.listAdminActivity("tok", {
      action: "user.",
      since: "2026-08-01T00:00:00Z",
      q: "devon",
    });
    const url = new URL(calledPath(), "https://example.test");
    expect(url.pathname).toBe("/admin/activity");
    expect(url.searchParams.get("action")).toBe("user.");
    expect(url.searchParams.get("since")).toBe("2026-08-01T00:00:00Z");
    expect(url.searchParams.get("q")).toBe("devon");
    expect(url.searchParams.has("cursor")).toBe(false);
    expect(url.searchParams.has("actor_user_id")).toBe(false);
    expect(url.searchParams.has("target_type")).toBe(false);
  });

  it("maps the camelCase options onto the wire's snake_case names", async () => {
    await adminApi.listAdminActivity("tok", {
      actorUserId: "11111111-1111-4111-8111-111111111111",
      targetType: "session",
    });
    const url = new URL(calledPath(), "https://example.test");
    expect(url.searchParams.get("actor_user_id")).toBe("11111111-1111-4111-8111-111111111111");
    expect(url.searchParams.get("target_type")).toBe("session");
  });

  it("pages through the cursor field, as Audit.tsx passes it", async () => {
    await adminApi.listAdminActivity("tok", { cursor: 412 });
    expect(calledPath()).toBe("/admin/activity?cursor=412");
  });

  it("sends no query with no arguments", async () => {
    await adminApi.listAdminActivity("tok");
    expect(calledPath()).toBe("/admin/activity");
  });

  it("escapes a value that would otherwise break the query string", async () => {
    await adminApi.listAdminActivity("tok", { q: "a&b=c d" });
    const url = new URL(calledPath(), "https://example.test");
    expect(url.searchParams.get("q")).toBe("a&b=c d");
  });
});

describe("listInvites", () => {
  it("sends ?state=pending when asked and nothing otherwise", async () => {
    await adminApi.listInvites("tok", { state: "pending" });
    expect(calledPath()).toBe("/admin/invites?state=pending");

    mockFetch.mockReset();
    mockFetch.mockResolvedValue(undefined as never);
    await adminApi.listInvites("tok");
    expect(calledPath()).toBe("/admin/invites");
  });
});
