import { describe, expect, it } from "vitest";
import { PyroWaveDecoder, chooseDecoderPath, type DecoderPath } from "./decoder";

const header = { width: 64, height: 64, fullRange: false };

describe("chooseDecoderPath", () => {
  it.each([
    [["subgroups", "shader-f16"], "fast"],
    [["subgroups"], "subgroups-fp32"], // Chrome on NVIDIA: no shader-f16
    [["shader-f16"], "fallback"],
    [[], "fallback"],
  ])("%j -> %s", (features, path) => {
    expect(chooseDecoderPath(new Set(features))).toBe(path);
  });

  it.each([
    [32, 32, "fast"],
    [4, 4, "fast"],
    [128, 128, "fast"],
    [undefined, undefined, "fast"], // not reported: trusted
    [32, 64, "fallback"], // RDNA: wave32 or wave64
    [8, 32, "fallback"],
    [256, 256, "fallback"],
    [48, 48, "fallback"],
    [0, 0, "fallback"],
  ] as const)("subgroups %s-%s -> %s", (subgroupMinSize, subgroupMaxSize, path) => {
    expect(chooseDecoderPath(new Set(["subgroups", "shader-f16"]), { subgroupMinSize, subgroupMaxSize })).toBe(path);
  });
});

describe("PyroWaveDecoder.create", () => {
  const fake = (extra: object = {}) =>
    ({
      features: new Set<string>(["shader-f16"]),
      lost: new Promise(() => {}),
      limits: { maxTextureDimension2D: 8192, maxStorageBufferBindingSize: 1 << 27, maxBufferSize: 1 << 28 },
      ...extra,
    }) as unknown as GPUDevice;

  it.each([
    ["fast", "subgroups"],
    ["subgroups-fp32", "subgroups"],
  ] as const)("refuses a forced %s path on a device without %s, naming it", async (forcePath, feature) => {
    const e = PyroWaveDecoder.create(fake(), header, { maxPixels: 64 * 64, forcePath });
    await expect(e).rejects.toMatchObject({ reason: "unsupported", message: expect.stringContaining(feature) });
  });

  it("refuses a forced fast path on a device without shader-f16, naming it", async () => {
    const e = PyroWaveDecoder.create(fake({ features: new Set(["subgroups"]) }), header, { maxPixels: 64 * 64, forcePath: "fast" });
    await expect(e).rejects.toMatchObject({ reason: "unsupported", message: expect.stringContaining("shader-f16") });
  });

  it.each(["turbo", "toString"])("refuses an unknown forced path %j with a PyroWaveError", async (forcePath) => {
    const e = PyroWaveDecoder.create(fake(), header, { maxPixels: 64 * 64, forcePath: forcePath as DecoderPath });
    await expect(e).rejects.toMatchObject({ name: "PyroWaveError", reason: "unsupported" });
  });

  it("a pipeline that fails to build is a gpu_error, and is not cached", async () => {
    let calls = 0;
    const device = fake({
      createShaderModule: () => ({ getCompilationInfo: async () => ({ messages: [] }) }),
      createComputePipelineAsync: async () => {
        calls++;
        throw new DOMException("the layout needs 7 storage buffers, the device allows 4", "OperationError");
      },
      createRenderPipelineAsync: async () => ({}),
    });
    const create = () => PyroWaveDecoder.create(device, header, { maxPixels: 64 * 64 });
    await expect(create()).rejects.toMatchObject({ reason: "gpu_error", message: expect.stringContaining("7 storage buffers") });
    const first = calls;
    await expect(create()).rejects.toMatchObject({ reason: "gpu_error" });
    expect(calls).toBeGreaterThan(first);
  });
});
