import { defineConfig } from "vitest/config";

// Shared web test configuration — jsdom environment for React + DOM tests.
export default defineConfig({
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    setupFiles: ["src/test-setup.ts"],
  },
});
