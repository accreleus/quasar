import { describe, expect, it } from "vitest";

import { explainCodecGap } from "./hostCodecs";

describe("explainCodecGap", () => {
  it("returns null when the host has not reported codecs at all (not a gap, an unknown)", () => {
    expect(explainCodecGap(null, "vulkan")).toBeNull();
    expect(explainCodecGap(undefined, "vulkan")).toBeNull();
    expect(explainCodecGap([], "vulkan")).toBeNull();
  });

  it("returns null when the host reports every wire codec", () => {
    expect(explainCodecGap(["h264", "h265", "av1"], "va")).toBeNull();
  });

  it("names the QUASAR_VULKAN_HEVC knob for an h264-only Vulkan host", () => {
    const gap = explainCodecGap(["h264"], "vulkan");
    expect(gap).not.toBeNull();
    expect(gap!.missing).toEqual(["h265", "av1"]);
    expect(gap!.reason).toContain("QUASAR_VULKAN_HEVC");
    // The knob defaults ON, so the wording must not claim HEVC is disabled by default.
    expect(gap!.reason).toContain("by default");
    expect(gap!.reason).toContain("vulkanh265enc");
    // AV1 has no one-line fix on Vulkan — must say so, not imply misconfiguration.
    expect(gap!.reason).toContain("vendor AV1 encoder");
  });

  it("does NOT claim the Vulkan gate for a non-Vulkan encoder", () => {
    const gap = explainCodecGap(["h264"], "va");
    expect(gap).not.toBeNull();
    expect(gap!.reason).not.toContain("QUASAR_VULKAN_HEVC");
    expect(gap!.reason).toMatch(/element/i);
  });

  it("does NOT claim the Vulkan gate for a Vulkan host missing only AV1 (h265 already present)", () => {
    const gap = explainCodecGap(["h264", "h265"], "vulkan");
    expect(gap).not.toBeNull();
    expect(gap!.missing).toEqual(["av1"]);
    expect(gap!.reason).not.toContain("QUASAR_VULKAN_HEVC");
  });

  it("names the element/registry cause generically when there is no known one-line fix", () => {
    const gap = explainCodecGap(["h264"], "nvenc");
    expect(gap).not.toBeNull();
    expect(gap!.reason).not.toMatch(/misconfigur/i);
    expect(gap!.reason).toMatch(/not registered/i);
  });
});
