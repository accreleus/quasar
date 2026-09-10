// vitest 5 moved the custom-matcher extension point: `Assertion` became
// `Assertion<R, T>` and now picks its extra matchers up from `Matchers<R, T>`.
// @testing-library/jest-dom 7.0.1 still ships the vitest-4 shape — its
// `declare module 'vitest' { interface Assertion<T = any> }` no longer merges,
// so every `toBeInTheDocument()` typechecks as a missing property while working
// perfectly at runtime (setup registers the matchers through `globals: true`).
//
// Augment the interface vitest actually reads. Remove this file when jest-dom
// ships a vitest-5 augmentation of its own.
import type { TestingLibraryMatchers } from "@testing-library/jest-dom/matchers";

declare module "vitest" {
  interface Matchers<
    R extends void | Promise<void> = void | Promise<void>,
    T = unknown,
  > extends TestingLibraryMatchers<T, R> {}
}
