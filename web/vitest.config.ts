import { configDefaults, defineConfig } from "vitest/config";

// Shared web test configuration — jsdom environment for React + DOM tests.
export default defineConfig({
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    exclude: [...configDefaults.exclude, "src/**/*.webgpu.test.ts"], // vitest.webgpu.config.ts
    setupFiles: ["src/test-setup.ts"],
  },
});
