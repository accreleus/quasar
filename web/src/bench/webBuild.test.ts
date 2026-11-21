import { afterEach, describe, expect, it, vi } from "vitest";

describe("web build identity", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  it("reads the constants Vite bakes in", async () => {
    vi.stubGlobal("__QUASAR_WEB_COMMIT__", "0123456789abcdef0123456789abcdef01234567");
    vi.stubGlobal("__QUASAR_WEB_BUILT_AT__", "2026-11-21T01:02:03.000Z");
    const { WEB_BUILD } = await import("./webBuild");
    expect(WEB_BUILD).toEqual({ commit: "0123456789abcdef0123456789abcdef01234567", built_at: "2026-11-21T01:02:03.000Z" });
  });

  it("says unknown where nothing was baked in (vitest does not run vite.config.ts)", async () => {
    const { WEB_BUILD } = await import("./webBuild");
    expect(WEB_BUILD).toEqual({ commit: "unknown", built_at: "unknown" });
  });
});
