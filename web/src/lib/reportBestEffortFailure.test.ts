import { afterEach, describe, expect, it, vi } from "vitest";
import { reportBestEffortFailure } from "./reportBestEffortFailure";

describe("reportBestEffortFailure", () => {
  afterEach(() => {
    // `restoreAllMocks` only restores `vi.spyOn` spies since vitest 3; clear call
    // history too, or a plain `vi.fn()`'s counters survive into the next test.
    vi.clearAllMocks();
    vi.restoreAllMocks();
  });

  it("calls console.debug for silent-debug level", () => {
    const spy = vi.spyOn(console, "debug").mockImplementation(() => {});
    const err = new Error("test");
    reportBestEffortFailure("silent-debug", "my context", err);
    expect(spy).toHaveBeenCalledOnce();
    expect(spy.mock.calls[0][0]).toContain("my context");
    expect(spy.mock.calls[0][1]).toBe(err);
  });

  it("calls console.warn for console-warn level", () => {
    const spy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const err = new Error("test");
    reportBestEffortFailure("console-warn", "my context", err);
    expect(spy).toHaveBeenCalledOnce();
    expect(spy.mock.calls[0][0]).toContain("my context");
    expect(spy.mock.calls[0][1]).toBe(err);
  });

  it("calls console.error for user-visible level", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const err = new Error("test");
    reportBestEffortFailure("user-visible", "my context", err);
    expect(spy).toHaveBeenCalledOnce();
    expect(spy.mock.calls[0][0]).toContain("my context");
    expect(spy.mock.calls[0][1]).toBe(err);
  });
});
