// #1189 — the device probe reports `webgpu` (control-api.md amendment 54/55). The probe
// is cached for the page's life, so each test loads a fresh copy of the module.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

async function load() {
  vi.resetModules();
  return import("./capability");
}

const adapter = (...features: string[]) => ({ features: new Set(features) });

function stubGpu(requestAdapter: () => Promise<unknown>) {
  Object.defineProperty(navigator, "gpu", { value: { requestAdapter }, configurable: true });
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  delete (navigator as unknown as { gpu?: unknown }).gpu;
});

describe("probeWebGpu", () => {
  it("reports not available when the browser has no navigator.gpu", async () => {
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: false, features: [] });
  });

  it("reports not available for a null adapter", async () => {
    stubGpu(async () => null);
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: false, features: [] });
  });

  it("reports not available when the request rejects", async () => {
    stubGpu(async () => {
      throw new Error("gpu process crashed");
    });
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: false, features: [] });
  });

  it("reports available with the decoder's features the adapter has", async () => {
    stubGpu(async () => adapter("subgroups", "shader-f16", "timestamp-query"));
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: true, features: ["subgroups", "shader-f16"] });
  });

  it("reports available with no features for an adapter that has none of them (fallback path)", async () => {
    stubGpu(async () => adapter("timestamp-query"));
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: true, features: [] });
  });

  it("reports not available for a fallback (software) adapter: info.isFallbackAdapter", async () => {
    stubGpu(async () => ({ ...adapter("subgroups", "shader-f16"), info: { isFallbackAdapter: true } }));
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: false, features: [] });
  });

  it("reports not available for a fallback adapter on older browsers: adapter.isFallbackAdapter", async () => {
    stubGpu(async () => ({ ...adapter("subgroups"), isFallbackAdapter: true }));
    const { probeWebGpu } = await load();
    expect(await probeWebGpu()).toEqual({ available: false, features: [] });
  });

  it("treats a hardware adapter with info.isFallbackAdapter false, or no info at all, as available", async () => {
    stubGpu(async () => ({ ...adapter(), info: { isFallbackAdapter: false } }));
    const first = await (await load()).probeWebGpu();
    stubGpu(async () => adapter());
    const second = await (await load()).probeWebGpu();
    expect([first.available, second.available]).toEqual([true, true]);
  });

  it("does not hang the probe on a request that never answers", async () => {
    stubGpu(() => new Promise(() => {}));
    const { probeWebGpu } = await load();
    const result = probeWebGpu();
    await vi.advanceTimersByTimeAsync(2000);
    expect(await result).toEqual({ available: false, features: [] });
  });

  it("asks once per page", async () => {
    const request = vi.fn(async () => adapter("subgroups"));
    stubGpu(request);
    const { probeWebGpu } = await load();
    await probeWebGpu();
    await probeWebGpu();
    expect(request).toHaveBeenCalledTimes(1);
  });
});

describe("the device record", () => {
  it("carries the webgpu key", async () => {
    vi.useRealTimers();
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 500 })) as unknown as typeof fetch);
    stubGpu(async () => adapter("subgroups"));
    const { probeCapabilities } = await load();
    expect((await probeCapabilities()).webgpu).toEqual({ available: true, features: ["subgroups"] });
  });
});

describe("the freshness stamp holds the WebGPU verdict (quasar-protocol#29 item 26)", () => {
  it("a timeout is not settled for a day: the next load's real answer re-posts", async () => {
    localStorage.clear();
    stubGpu(() => new Promise(() => {}));
    const first = await load();
    const timedOut = first.probeWebGpu();
    await vi.advanceTimersByTimeAsync(2000);
    const verdict = (await timedOut).available;
    expect(verdict).toBe(false);
    first.markDeviceProbePosted("u1", true, verdict, 1_000_000);
    expect(first.deviceProbeIsFresh("u1", true, verdict, 1_000_000)).toBe(true);

    // A later page load: the adapter answers.
    stubGpu(async () => adapter("subgroups"));
    const second = await load();
    const now = (await second.probeWebGpu()).available;
    expect(now).toBe(true);
    expect(second.deviceProbeIsFresh("u1", true, now, 1_000_000)).toBe(false);
  });
});
