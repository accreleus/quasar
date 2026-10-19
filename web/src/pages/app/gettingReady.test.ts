import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { GETTING_READY_MS, GETTING_READY_PHRASES, useBootLine } from "./gettingReady";

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("useBootLine", () => {
  it("shows Steam's figure only when it is above 0", () => {
    const { result } = renderHook(() => useBootLine(true, 42));
    expect(result.current).toEqual({ text: "Processing shaders — 42%", decorative: false });
  });

  it.each([0, null])("shows a phrase, not the figure, when it is %s", (pct) => {
    const { result } = renderHook(() => useBootLine(true, pct));
    expect(GETTING_READY_PHRASES).toContain(result.current.text);
    expect(result.current.decorative).toBe(true);
  });

  it("shows nothing when the app is not booting", () => {
    const { result } = renderHook(() => useBootLine(false, 42));
    expect(result.current).toEqual({ text: "", decorative: false });
  });

  it("picks a new phrase every 3 s, never the same twice running", () => {
    const { result } = renderHook(() => useBootLine(true, null));
    const seen = [result.current.text];
    for (let i = 0; i < 30; i++) {
      act(() => {
        vi.advanceTimersByTime(GETTING_READY_MS);
      });
      seen.push(result.current.text);
    }
    expect(seen.every((p) => GETTING_READY_PHRASES.includes(p as never))).toBe(true);
    for (let i = 1; i < seen.length; i++) expect(seen[i]).not.toBe(seen[i - 1]);
    expect(new Set(seen).size).toBeGreaterThan(2);
  });

  it("does not change before the 3 s are up, and stops rotating when the boot ends", () => {
    const { result, rerender } = renderHook(({ b }) => useBootLine(b, null), { initialProps: { b: true } });
    const first = result.current.text;
    act(() => {
      vi.advanceTimersByTime(GETTING_READY_MS - 1);
    });
    expect(result.current.text).toBe(first);
    rerender({ b: false });
    expect(result.current.text).toBe("");
    expect(vi.getTimerCount()).toBe(0);
  });

  it("keeps every phrase short", () => {
    expect(GETTING_READY_PHRASES).toHaveLength(20);
    for (const p of GETTING_READY_PHRASES) expect(p.length).toBeLessThanOrEqual(32);
  });
});
