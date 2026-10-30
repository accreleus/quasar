import "@testing-library/jest-dom";
import { configure } from "@testing-library/react";
import { afterEach } from "vitest";

import { cancelReveal } from "./components/shell/settingsSearch/reveal";

// A settings-search reveal polls for up to 4 s after a palette pick or the
// layout switch, outliving a test that did not wait for it: the poll then hit a
// torn-down jsdom ("document is not defined", an unhandled error while every
// test passed). Every test ends it.
afterEach(cancelReveal);

// jsdom implements neither object-URL method. They live here rather than in
// each test because `lib/download.ts` revokes on a macrotask, which outlives a
// test's own `vi.stubGlobal` and would throw after the stub is restored.
if (typeof URL.createObjectURL !== "function") URL.createObjectURL = () => "blob:test";
// jsdom does not implement scrolling; the one-page account layout scrolls to a section.
if (typeof Element.prototype.scrollIntoView !== "function") Element.prototype.scrollIntoView = () => {};
if (typeof Element.prototype.scrollTo !== "function") Element.prototype.scrollTo = () => {};
if (typeof URL.revokeObjectURL !== "function") URL.revokeObjectURL = () => {};

// Routes and the command palette are lazy chunks. The first import of one in a
// worker transforms its whole module graph, which can outlast findBy's default
// 1 s while the full suite loads every core; a hit only waits longer on a real failure.
configure({ asyncUtilTimeout: 5000 });
