import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as authApi from "../api/auth";
import { markDeviceProbePosted } from "../webrtc/capability";
import { ensureDeviceProbe, ensureDeviceProbeForLaunch, LAUNCH_PROBE_WAIT_MS } from "./deviceProbe";

vi.mock("../api/auth", () => ({ postDevice: vi.fn() }));
let capabilities: () => Promise<unknown> = async () => ({ codecs: { h264: true } });
vi.mock("../webrtc/capability", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../webrtc/capability")>()),
  probeH264Decodable: async () => true,
  probeCapabilities: () => capabilities(),
}));

beforeEach(() => {
  capabilities = async () => ({ codecs: { h264: true } });
  localStorage.clear();
  vi.mocked(authApi.postDevice).mockReset().mockResolvedValue({} as never);
});
afterEach(() => vi.useRealTimers());

describe("ensureDeviceProbe", () => {
  it("posts when the stored record predates the WebCodecs keys, and not again once posted", async () => {
    // A record the planner reads as `device_no_webcodecs`: posted under the old stamp key.
    markDeviceProbePosted("u1", true);
    const [[key, stamp]] = Object.entries(localStorage);
    localStorage.clear();
    localStorage.setItem(key.replace("_v4", "_v3"), stamp);
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
    await ensureDeviceProbe("tok", "u1");
    expect(authApi.postDevice).toHaveBeenCalledTimes(1);
  });

  it("lets a hung probe go, so the next launch is not held by it", async () => {
    vi.useFakeTimers();
    capabilities = () => new Promise(() => {});
    const first = ensureDeviceProbe("tok", "u1").catch((e: Error) => e);
    await vi.advanceTimersByTimeAsync(20_001);
    expect(await first).toMatchObject({ message: "device probe timed out" });
    capabilities = async () => ({ codecs: { h264: true } });
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
