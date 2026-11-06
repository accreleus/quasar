import { describe, expect, it } from "vitest";
import { PyroWaveDecoder, chooseDecoderPath } from "./decoder";

describe("chooseDecoderPath", () => {
  it.each([
    [["subgroups", "shader-f16"], "fast"],
    [["subgroups"], "subgroups-fp32"], // Chrome on NVIDIA: no shader-f16
    [["shader-f16"], "fallback"],
    [[], "fallback"],
  ])("%j -> %s", (features, path) => {
    expect(chooseDecoderPath(new Set(features))).toBe(path);
  });

  it.each(["fast", "subgroups-fp32"] as const)("refuses a forced %s path on a device without subgroups", async (forcePath) => {
    const device = { features: new Set<string>(["shader-f16"]), lost: new Promise(() => {}) } as unknown as GPUDevice;
    await expect(PyroWaveDecoder.create(device, { width: 64, height: 64, fullRange: false }, { maxPixels: 64 * 64, forcePath })).rejects.toMatchObject({ reason: "no_subgroups" });
  });
});
