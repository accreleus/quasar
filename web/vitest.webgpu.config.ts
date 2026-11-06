import { defineConfig } from "vitest/config";
import { playwright } from "@vitest/browser-playwright";

// WebGPU tests (*.webgpu.test.ts) in headless Chromium on SwiftShader, which exposes `subgroups`
// but not `shader-f16`. Run with `make test-webgpu`: the browser comes from the Playwright image
// whose version matches the pinned `playwright` devDependency. PYROWAVE_GPU=native uses the host's
// GPU through Vulkan instead (the container needs the GPU's device nodes and Vulkan driver).
const gpuArgs =
  process.env.PYROWAVE_GPU === "native"
    ? ["--use-vulkan=native", "--use-angle=vulkan", "--ignore-gpu-blocklist", "--enable-webgpu-developer-features"]
    : ["--use-vulkan=swiftshader", "--use-webgpu-adapter=swiftshader", "--enable-unsafe-swiftshader"];

export default defineConfig({
  // The golden fixtures live in testdata/ at the repo root.
  server: { fs: { allow: [".."] } },
  test: {
    include: ["src/**/*.webgpu.test.ts"],
    testTimeout: 60_000,
    browser: {
      enabled: true,
      headless: true,
      provider: playwright({
        launchOptions: {
          args: ["--enable-unsafe-webgpu", "--enable-features=Vulkan", "--disable-vulkan-surface", ...gpuArgs],
        },
      }),
      instances: [{ browser: "chromium" }],
    },
  },
});
