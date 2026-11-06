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

  it("refuses a device without subgroups until the fallback decoder exists", async () => {
    const device = { features: new Set<string>() } as unknown as GPUDevice;
    await expect(PyroWaveDecoder.create(device, { width: 64, height: 64, fullRange: false })).rejects.toThrow(/#1055/);
  });
});
