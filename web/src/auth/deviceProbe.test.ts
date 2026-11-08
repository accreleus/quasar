import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as authApi from "../api/auth";
import { deviceProbeIsFresh, INCOMPLETE_PROBE_RETRY_MS, markDeviceProbePosted } from "../webrtc/capability";
import { ensureDeviceProbe, ensureDeviceProbeForLaunch, LAUNCH_PROBE_WAIT_MS } from "./deviceProbe";

vi.mock("../api/auth", () => ({ postDevice: vi.fn() }));
const COMPLETE = { codecs: { h264: true }, webcodecs: { h264: true }, webgpu: { available: true, features: [] } };
let capabilities: () => Promise<unknown> = async () => COMPLETE;
vi.mock("../webrtc/capability", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../webrtc/capability")>()),
  probeH264Decodable: async () => true,
  probeWebGpu: async () => ({ available: true, features: [] }),
  probeCapabilities: () => capabilities(),
}));

beforeEach(() => {
  capabilities = async () => COMPLETE;
  localStorage.clear();
  vi.mocked(authApi.postDevice).mockReset().mockResolvedValue({} as never);
});
afterEach(() => vi.useRealTimers());

describe("ensureDeviceProbe", () => {
  it("posts when the stored record predates the WebCodecs keys, and not again once posted", async () => {
    // A record the planner reads as `device_no_webcodecs`: posted under the old stamp key.
    markDeviceProbePosted("u1", true, true);
    const [[key, stamp]] = Object.entries(localStorage);
    localStorage.clear();
    localStorage.setItem(key.replace("_v5", "_v3"), stamp);
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
  });

  it("stamps a complete probe for the day, and an incomplete one for minutes only", async () => {
    const HOUR = 60 * 60 * 1000;
    await ensureDeviceProbe("tok", "u1");
    expect(deviceProbeIsFresh("u1", true, true, Date.now() + HOUR)).toBe(true);

    // WebCodecs timed out: the key is absent, the planner reads that as `device_no_webcodecs`.
    localStorage.clear();
    capabilities = async () => ({ ...COMPLETE, webcodecs: undefined });
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(2);
    expect(deviceProbeIsFresh("u1", true, true, Date.now() + INCOMPLETE_PROBE_RETRY_MS - 1_000)).toBe(true);
    expect(deviceProbeIsFresh("u1", true, true, Date.now() + INCOMPLETE_PROBE_RETRY_MS + 1_000)).toBe(false);

    localStorage.clear();
    capabilities = async () => ({ ...COMPLETE, webgpu: undefined });
    await ensureDeviceProbe("tok", "u1");
    expect(deviceProbeIsFresh("u1", true, true, Date.now() + HOUR)).toBe(false);
  });

  it("lets a hung probe go, so the next launch is not held by it", async () => {
    vi.useFakeTimers();
    capabilities = () => new Promise(() => {});
    const first = ensureDeviceProbe("tok", "u1").catch((e: Error) => e);
    await vi.advanceTimersByTimeAsync(20_001);
    expect(await first).toMatchObject({ message: "device probe timed out" });
    capabilities = async () => COMPLETE;
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
  });

  it("shares one post between concurrent callers", async () => {
    await Promise.all([ensureDeviceProbe("tok", "u1"), ensureDeviceProbe("tok", "u1")]);
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
  });
});

describe("ensureDeviceProbeForLaunch", () => {
  it("never rejects, so a failed post cannot stop a launch", async () => {
    vi.mocked(authApi.postDevice).mockRejectedValue(new Error("429"));
    await expect(ensureDeviceProbeForLaunch("tok", "u1")).resolves.toBeUndefined();
  });

  it("stops waiting after the bound", async () => {
    vi.useFakeTimers();
    vi.mocked(authApi.postDevice).mockReturnValue(new Promise(() => {}));
    const done = vi.fn();
    void ensureDeviceProbeForLaunch("tok", "u2").then(done);
    await vi.advanceTimersByTimeAsync(LAUNCH_PROBE_WAIT_MS + 1);
    expect(done).toHaveBeenCalled();
  });
});
