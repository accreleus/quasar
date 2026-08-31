import "@testing-library/jest-dom";

// jsdom implements neither object-URL method. They live here rather than in
// each test because `lib/download.ts` revokes on a macrotask, which outlives a
// test's own `vi.stubGlobal` and would throw after the stub is restored.
if (typeof URL.createObjectURL !== "function") URL.createObjectURL = () => "blob:test";
if (typeof URL.revokeObjectURL !== "function") URL.revokeObjectURL = () => {};
