// Tests for apiFetch's response handling — in particular bodyless 2xx.
//
// WHY: DELETE /v1/admin/storage/homes/{id} answers 202 Accepted with NO body
// (control-plane/internal/storage/handler.go). apiFetch only ever special-cased
// 204, so every other bodyless success fell through to `res.json()`, which
// throws SyntaxError on an empty body. The thrown error is not an ApiError, so
// fleet/StorageTab's catch reported the literal "tombstone failed" — for a request
// the server had actually accepted. The operator saw a red error and clicked
// Tombstone six times; the server returned 202 every time.
//
// The fix keys off an empty body rather than a status allowlist, so the next
// endpoint that returns a bodyless 200/202/205 does not reintroduce this.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { apiFetch, ApiError } from "./client";

const realFetch = globalThis.fetch;

function respond(status: number, body: string, headers: Record<string, string> = {}) {
  return new Response(body === "" ? null : body, { status, headers });
}

beforeEach(() => {
  globalThis.fetch = vi.fn() as unknown as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.clearAllMocks();
});

const mock = () => vi.mocked(globalThis.fetch);

describe("apiFetch — bodyless success responses", () => {
  it("resolves undefined on 202 with an empty body (the tombstone case)", async () => {
    mock().mockResolvedValueOnce(respond(202, ""));
    await expect(
      apiFetch<void>("/admin/storage/homes/abc", { method: "DELETE", token: "t" }),
    ).resolves.toBeUndefined();
  });

  it("resolves undefined on 204", async () => {
    mock().mockResolvedValueOnce(respond(204, ""));
    await expect(apiFetch<void>("/apps/abc", { method: "DELETE", token: "t" })).resolves.toBeUndefined();
  });

  it("resolves undefined on a bodyless 200", async () => {
    mock().mockResolvedValueOnce(respond(200, ""));
    await expect(apiFetch<void>("/whatever", { token: "t" })).resolves.toBeUndefined();
  });

  it("still parses a 202 that DOES carry a body", async () => {
    // DELETE /v1/sessions/{id} answers 202 with the session envelope.
    mock().mockResolvedValueOnce(
      respond(202, JSON.stringify({ session: { id: "s1", state: "stopping" } })),
    );
    const out = await apiFetch<{ session: { id: string; state: string } }>("/sessions/s1", {
      method: "DELETE",
      token: "t",
    });
    expect(out.session).toEqual({ id: "s1", state: "stopping" });
  });

  it("still parses a normal 200 JSON body", async () => {
    mock().mockResolvedValueOnce(respond(200, JSON.stringify({ items: [1, 2] })));
    await expect(apiFetch<{ items: number[] }>("/apps", { token: "t" })).resolves.toEqual({
      items: [1, 2],
    });
  });
});

describe("apiFetch — errors still surface as ApiError", () => {
  it("maps a JSON error envelope to ApiError with its code", async () => {
    mock().mockResolvedValueOnce(
      respond(409, JSON.stringify({ error: { code: "home_in_use", message: "in use" } })),
    );
    await expect(apiFetch("/admin/storage/homes/abc", { method: "DELETE", token: "t" })).rejects.toThrow(
      ApiError,
    );
  });

  it("carries the error code so callers can branch without string-matching", async () => {
    mock().mockResolvedValueOnce(
      respond(409, JSON.stringify({ error: { code: "home_in_use", message: "in use" } })),
    );
    const err = await apiFetch("/admin/storage/homes/abc", { method: "DELETE", token: "t" }).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("home_in_use");
    expect((err as ApiError).status).toBe(409);
  });

  // A bodyless error must NOT become a silent success.
  it("still throws on a bodyless 500", async () => {
    mock().mockResolvedValueOnce(respond(500, ""));
    await expect(apiFetch("/apps", { token: "t" })).rejects.toThrow(ApiError);
  });
});

// #494 — 503 capacity_exhausted carries Retry-After so a client can retry on
// the server's own timing instead of guessing (see AppHomeNext.tsx launchApp
// / capacityRetry.ts for what consumes this).
describe("apiFetch — Retry-After (#494)", () => {
  it("parses Retry-After into ApiError.retryAfterSeconds", async () => {
    mock().mockResolvedValueOnce(
      respond(503, JSON.stringify({ error: { code: "capacity_exhausted", message: "full" } }), {
        "Retry-After": "5",
      }),
    );
    const err = await apiFetch("/sessions", { method: "POST", token: "t" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("capacity_exhausted");
    expect((err as ApiError).retryAfterSeconds).toBe(5);
  });

  it("leaves retryAfterSeconds undefined when the header is absent", async () => {
    mock().mockResolvedValueOnce(
      respond(503, JSON.stringify({ error: { code: "no_host_available", message: "offline" } })),
    );
    const err = await apiFetch("/sessions", { method: "POST", token: "t" }).catch((e: unknown) => e);
    expect((err as ApiError).retryAfterSeconds).toBeUndefined();
  });

  it("ignores a malformed Retry-After rather than surfacing NaN", async () => {
    mock().mockResolvedValueOnce(
      respond(503, JSON.stringify({ error: { code: "capacity_exhausted", message: "full" } }), {
        "Retry-After": "not-a-number",
      }),
    );
    const err = await apiFetch("/sessions", { method: "POST", token: "t" }).catch((e: unknown) => e);
    expect((err as ApiError).retryAfterSeconds).toBeUndefined();
  });
});
