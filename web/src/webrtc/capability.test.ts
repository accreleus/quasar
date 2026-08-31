// AS10-08 — unit tests for the client certification builder and feature
// detection. jsdom provides navigator/document/RTCRtpReceiver shims; we mock the
// pieces feature detection reads. Network probes (rtt/bandwidth) are mocked to
// no-ops so probeCapabilities stays pure.

import { afterEach, describe, expect, it, vi } from "vitest";
import {
  deviceProbeIsFresh,
  markDeviceProbePosted,
  probeCapabilities,
  probeFeatures,
  type DeviceCapabilities,
  type NativeCapabilities,
} from "./capability";

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  // Remove any navigator props defined per-test (jsdom lacks getGamepads).
  delete (navigator as unknown as { getGamepads?: unknown }).getGamepads;
  // Multi-codec spec §6.1: jsdom lacks mediaCapabilities; remove the per-test stub
  // so a test that doesn't define it never sees a prior test's leftover mock.
  delete (navigator as unknown as { mediaCapabilities?: unknown }).mediaCapabilities;
});

describe("probeFeatures", () => {
  it("detects supported playout/input features as true", () => {
    // jsdom may not define RTCRtpReceiver/PointerEvent — stub them with the
    // detected members present.
    vi.stubGlobal("RTCRtpReceiver", function () {} as unknown);
    (globalThis as unknown as { RTCRtpReceiver: { prototype: object } }).RTCRtpReceiver.prototype = {
      jitterBufferTarget: null,
      playoutDelayHint: null,
    };
    vi.stubGlobal("PointerEvent", function () {} as unknown);
    (globalThis as unknown as { PointerEvent: { prototype: object } }).PointerEvent.prototype = {
      getCoalescedEvents() {},
    };
    // jsdom's navigator has no getGamepads — define it so detection sees it.
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [],
      configurable: true,
    });

    const f = probeFeatures();
    expect(f.jitter_buffer_target).toBe(true);
    expect(f.playout_delay_hint).toBe(true);
    expect(f.coalesced_pointer_events).toBe(true);
    expect(f.gamepad).toBe(true);
    // jsdom's document always has pointerLockElement defined.
    expect(typeof f.pointer_lock).toBe("boolean");
  });

  it("reports false for unsupported features (empty prototypes)", () => {
    vi.stubGlobal("RTCRtpReceiver", function () {} as unknown);
    (globalThis as unknown as { RTCRtpReceiver: { prototype: object } }).RTCRtpReceiver.prototype = {};
    vi.stubGlobal("PointerEvent", function () {} as unknown);
    (globalThis as unknown as { PointerEvent: { prototype: object } }).PointerEvent.prototype = {};

    const f = probeFeatures();
    expect(f.jitter_buffer_target).toBe(false);
    expect(f.playout_delay_hint).toBe(false);
    expect(f.coalesced_pointer_events).toBe(false);
  });
});

describe("probeCapabilities", () => {
  // Stub the network probes (fetch) and rAF so the builder runs deterministically.
  function stubEnv(opts: { uaData?: unknown } = {}) {
    // fetch: HEAD warmups + bandwidth GET — return a tiny ok response.
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => ({
        ok: true,
        async arrayBuffer() {
          return new ArrayBuffer(0);
        },
      })) as unknown as typeof fetch,
    );
    // requestAnimationFrame: drive enough ticks to estimate ~60Hz.
    let t = 0;
    vi.stubGlobal(
      "requestAnimationFrame",
      ((cb: FrameRequestCallback) => {
        t += 16.67;
        setTimeout(() => cb(t), 0);
        return 1;
      }) as unknown as typeof requestAnimationFrame,
    );
    if ("uaData" in opts) {
      Object.defineProperty(navigator, "userAgentData", {
        value: opts.uaData,
        configurable: true,
      });
    }
  }

  it("builds the extended certification record with the AS10-08 fields", async () => {
    stubEnv({
      uaData: {
        brands: [
          { brand: "Not.A/Brand", version: "99" },
          { brand: "Chromium", version: "126" },
          { brand: "Google Chrome", version: "126" },
        ],
        platform: "macOS",
      },
    });

    const caps: DeviceCapabilities = await probeCapabilities();

    expect(caps.client_type).toBe("web");
    expect(caps.codecs).toBeDefined();
    expect(typeof caps.max_decode_height).toBe("number");
    // browser: a real brand chosen (not the GREASE "Not.A/Brand").
    expect(caps.browser.name).not.toMatch(/not.?a.?brand/i);
    expect(caps.browser.version).toBe("126");
    expect(caps.platform).toBe("macOS");
    // display geometry present.
    expect(typeof caps.display.width).toBe("number");
    expect(typeof caps.display.height).toBe("number");
    expect(typeof caps.display.device_pixel_ratio).toBe("number");
    // features object present with boolean members.
    expect(typeof caps.features.gamepad).toBe("boolean");
    // profiles is the empty shape (AS10-08 — AS10-10 fills it).
    expect(caps.profiles).toEqual({});
  });

  // AS10-12: the native report is an additive superset of the web probe. A web
  // DeviceCapabilities must be a valid (minimal) native-family report — this both
  // type-checks the superset relationship and asserts it at runtime (the
  // "web subset is a valid report" invariant, the byte-identical-store gate's
  // client-side mirror).
  it("a web DeviceCapabilities is a valid (minimal) NativeCapabilities", async () => {
    stubEnv({ uaData: { brands: [{ brand: "Chromium", version: "126" }], platform: "macOS" } });
    const web: DeviceCapabilities = await probeCapabilities();

    // Assignable to the native superset with native-only fields added. If the type
    // relationship regressed, this would not compile (tsc -b in CI catches it).
    const native: NativeCapabilities = {
      ...web,
      report_version: 1,
      client_name: "quasar-native-macos",
      client_version: "0.1.0",
      os: { name: "macOS", version: "15.5", arch: "arm64" },
      display: { ...web.display, hdr: true, vrr: true },
      decode: { h264: { hw: true, profiles: ["high"], levels: ["5.1"], max_height: 2160 } },
      audio: { channels: 2, sample_rate: 48000, codecs: ["opus"] },
      input: { raw_mouse: true, keyboard: true, high_rate_input: true, controllers: [] },
      metrics: { glass_to_glass_ms_p50: 45, glass_to_glass_ms_p95: 104, render_path: "webrtcbin+videotoolbox" },
      health: { class: "smooth" },
    };

    // The flat codecs map (the eligibility surface) is carried through unchanged.
    expect(native.codecs).toEqual(web.codecs);
    expect(native.report_version).toBe(1);
    expect(native.metrics?.glass_to_glass_ms_p95).toBe(104);
  });

  it("falls back to UA-string sniffing when UA-CH is absent", async () => {
    stubEnv({ uaData: undefined });
    Object.defineProperty(navigator, "userAgent", {
      value: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
      configurable: true,
    });

    const caps = await probeCapabilities();
    expect(caps.browser.name).toBe("Chrome");
    expect(caps.browser.version).toBe("120.0.0.0");
  });

  // Multi-codec spec §6.1: MediaCapabilities decode-quality refinement.
  describe("codecs_detail (MediaCapabilities decodingInfo)", () => {
    it("attaches hevc/av1 decode detail when the API is present", async () => {
      stubEnv();
      Object.defineProperty(navigator, "mediaCapabilities", {
        value: {
          decodingInfo: vi.fn(async (config: { video?: { contentType: string } }) => {
            const isHevc = config.video?.contentType === "video/H265";
            return {
              supported: true,
              smooth: isHevc,
              powerEfficient: !isHevc,
            };
          }),
        },
        configurable: true,
      });

      const caps = await probeCapabilities();
      expect(caps.codecs_detail?.hevc).toEqual({ supported: true, smooth: true, power_efficient: false });
      expect(caps.codecs_detail?.av1).toEqual({ supported: true, smooth: false, power_efficient: true });
    });

    it("is best-effort: absent API yields an empty codecs_detail, never a thrown error", async () => {
      stubEnv();
      // No mediaCapabilities defined at all (the afterEach hook already deletes it,
      // but stubEnv() doesn't touch navigator.userAgentData here, so be explicit).
      const caps = await probeCapabilities();
      expect(caps.codecs_detail).toEqual({});
    });

    it("is best-effort: a throwing decodingInfo yields an empty codecs_detail", async () => {
      stubEnv();
      Object.defineProperty(navigator, "mediaCapabilities", {
        value: { decodingInfo: vi.fn(async () => { throw new Error("boom"); }) },
        configurable: true,
      });

      const caps = await probeCapabilities();
      expect(caps.codecs_detail).toEqual({});
    });
  });
});

describe("device capability post freshness", () => {
  const DAY = 24 * 60 * 60 * 1000;

  it("is stale with nothing recorded", () => {
    localStorage.clear();
    expect(deviceProbeIsFresh("u1")).toBe(false);
  });

  it("stays fresh for a day and goes stale after it", () => {
    localStorage.clear();
    markDeviceProbePosted("u1", 1_000_000);
    expect(deviceProbeIsFresh("u1", 1_000_000)).toBe(true);
    expect(deviceProbeIsFresh("u1", 1_000_000 + DAY - 1)).toBe(true);
    expect(deviceProbeIsFresh("u1", 1_000_000 + DAY)).toBe(false);
  });

  it("is per user, so a second account on the browser still posts", () => {
    localStorage.clear();
    markDeviceProbePosted("u1", 1_000_000);
    expect(deviceProbeIsFresh("u2", 1_000_000)).toBe(false);
  });

  it("treats unreadable storage as stale rather than throwing", () => {
    localStorage.setItem("quasar_device_posted", "not json");
    expect(deviceProbeIsFresh("u1")).toBe(false);
  });
});
