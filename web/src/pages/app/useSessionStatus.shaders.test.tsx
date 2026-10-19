// The launch poll's reading of `launch_progress`: a known step while the app is
// booting is surfaced; an unknown step, a null, or any other detail is not.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useSessionStatus } from "./useSessionStatus";

const getSession = vi.hoisted(() => vi.fn());
vi.mock("../../api/library", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/library")>()),
  getSession,
}));

const session = (detail: string, launch_progress: unknown) => ({
  session: { id: "s1", state: "running", state_detail: detail, host_id: "h1", launch_progress },
});

async function read(res: unknown) {
  getSession.mockResolvedValue(res);
  const { result } = renderHook(() => useSessionStatus("tok", "s1", false, { current: false }));
  await act(async () => {});
  return result.current.shaderPercent;
}

beforeEach(() => {
  getSession.mockReset();
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe("useSessionStatus — shaderPercent", () => {
  it("returns to null when Steam finishes its shaders while the app is still booting", async () => {
    let now = session("app booting", { step: "shaders", percent: 42 });
    getSession.mockImplementation(async () => now);
    const { result } = renderHook(() => useSessionStatus("tok", "s1", false, { current: false }));
    await act(async () => {});
    expect(result.current.shaderPercent).toBe(42);
    now = session("app booting", null);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(result.current.shaderPercent).toBeNull();
  });

  it("surfaces the shaders step while the app boots", async () => {
    expect(await read(session("app booting", { step: "shaders", percent: 42 }))).toBe(42);
  });
  it("ignores an unknown step", async () => {
    expect(await read(session("app booting", { step: "download", percent: 42 }))).toBeNull();
  });
  it("is null when the read carries none", async () => {
    expect(await read(session("app booting", null))).toBeNull();
  });
  it("ignores progress once the detail has left app booting", async () => {
    expect(await read(session("app presented", { step: "shaders", percent: 90 }))).toBeNull();
  });
});
