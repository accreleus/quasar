// `PATCH /v1/sessions/{id}/display` — the wire shape of updateSessionDisplay.
//
// The endpoint is both-or-neither on the dims and rejects a rejected update as
// a NO-OP (409 display_update_rejected), so what matters here is that the
// client sends exactly the fields it was given, at the right URL, with the
// right method — and that a 4xx surfaces as an ApiError with its code intact so
// SessionPage can revert to its last-acked value.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./client";
import { updateSessionDisplay } from "./library";

const realFetch = globalThis.fetch;

beforeEach(() => {
  globalThis.fetch = vi.fn() as unknown as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = realFetch;
  vi.clearAllMocks();
});

const mock = () => vi.mocked(globalThis.fetch);

function ok() {
  return new Response(JSON.stringify({ session: { id: "s1", state: "running" } }), { status: 202 });
}

function lastCall() {
  const [url, init] = mock().mock.calls[0] as [string, RequestInit];
  return { url, init, body: JSON.parse(String(init.body)) as Record<string, unknown> };
}

describe("updateSessionDisplay", () => {
  it("PATCHes /v1/sessions/{id}/display", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", { render_width: 1280, render_height: 720 });
    const { url, init } = lastCall();
    expect(url).toBe("/v1/sessions/s1/display");
    expect(init.method).toBe("PATCH");
  });

  it("sends both dims together", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", { render_width: 1280, render_height: 720 });
    expect(lastCall().body).toEqual({ render_width: 1280, render_height: 720 });
  });

  it("sends ui_scale alone as a partial update", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", { ui_scale: 1.5 });
    expect(lastCall().body).toEqual({ ui_scale: 1.5 });
  });

  it("sends the bearer token", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", { ui_scale: 2 });
    const headers = lastCall().init.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer tok");
  });

  it("percent-encodes the session id", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "a/b", { ui_scale: 2 });
    expect(lastCall().url).toBe("/v1/sessions/a%2Fb/display");
  });

  it("returns the 202 session envelope", async () => {
    mock().mockResolvedValueOnce(ok());
    await expect(updateSessionDisplay("tok", "s1", { ui_scale: 2 })).resolves.toMatchObject({
      session: { id: "s1" },
    });
  });

  it("surfaces 409 display_update_rejected as an ApiError with its code", async () => {
    mock().mockResolvedValueOnce(
      new Response(
        JSON.stringify({ error: { code: "display_update_rejected", message: "host refused" } }),
        { status: 409 },
      ),
    );
    const err = await updateSessionDisplay("tok", "s1", { ui_scale: 2 }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("display_update_rejected");
    expect((err as ApiError).status).toBe(409);
  });

  it("sends both STREAM dims together (adaptive external resolution)", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", { stream_width: 1280, stream_height: 720 });
    expect(lastCall().body).toEqual({ stream_width: 1280, stream_height: 720 });
  });

  it("carries a stream change and a render change in one merged body", async () => {
    mock().mockResolvedValueOnce(ok());
    await updateSessionDisplay("tok", "s1", {
      stream_width: 1280,
      stream_height: 720,
      render_width: 960,
      render_height: 540,
    });
    expect(lastCall().body).toEqual({
      stream_width: 1280,
      stream_height: 720,
      render_width: 960,
      render_height: 540,
    });
  });

  it("surfaces 409 external_resize_unsupported as an ApiError with its code", async () => {
    mock().mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          error: { code: "external_resize_unsupported", message: "encoder cannot resize" },
        }),
        { status: 409 },
      ),
    );
    const err = await updateSessionDisplay("tok", "s1", { stream_width: 1280, stream_height: 720 })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("external_resize_unsupported");
    expect((err as ApiError).status).toBe(409);
  });

  it("surfaces 400 validation_failed as an ApiError", async () => {
    mock().mockResolvedValueOnce(
      new Response(JSON.stringify({ error: { code: "validation_failed", message: "odd" } }), {
        status: 400,
      }),
    );
    const err = await updateSessionDisplay("tok", "s1", { render_width: 1281, render_height: 721 })
      .catch((e: unknown) => e);
    expect((err as ApiError).code).toBe("validation_failed");
  });
});
