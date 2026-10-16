import "@testing-library/jest-dom";

// jsdom implements neither object-URL method. They live here rather than in
// each test because `lib/download.ts` revokes on a macrotask, which outlives a
// test's own `vi.stubGlobal` and would throw after the stub is restored.
if (typeof URL.createObjectURL !== "function") URL.createObjectURL = () => "blob:test";
// jsdom does not implement scrolling; the one-page account layout scrolls to a section.
if (typeof Element.prototype.scrollIntoView !== "function") Element.prototype.scrollIntoView = () => {};
if (typeof Element.prototype.scrollTo !== "function") Element.prototype.scrollTo = () => {};
if (typeof URL.revokeObjectURL !== "function") URL.revokeObjectURL = () => {};
