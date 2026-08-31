import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useSwapTransition } from "./useSwapTransition";

const getSession = vi.fn();
vi.mock("../../api/library", () => ({
  getSession: (...a: unknown[]) => getSession(...a),
}));

const session = (state: string, state_detail: string | null, app_id = "a1") => ({
  session: { id: "s1", app_id, state, state_detail, error_message: null },
});

beforeEach(() => {
  getSession.mockReset();
  // Plain fake timers, no shouldAdvanceTime: that flag ties the fake clock to
  // REAL elapsed wall time (a background nudge, to keep RTL's waitFor making
  // progress), which under heavy parallel test load let real delay push the
  // clock past a nearby boundary (e.g. the 3.5s auto-dismiss) that a manual
  // `advanceTimersByTimeAsync(2000)` call was never meant to reach. advanceUntil
  // below replaces every waitFor with a step loop driven purely by manual fake-
  // clock advances, so elapsed time is exactly what the test requests.
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

/** Deterministic replacement for RTL's `waitFor`: steps the fake clock by
 *  `stepMs` (the poll interval) until `predicate` is true, flushing the
 *  pending mocked promise on each step. Never touches the real clock, so it
 *  cannot overshoot a nearby timer boundary the way shouldAdvanceTime's
 *  real-time-linked auto-advance can. */
async function advanceUntil(predicate: () => boolean, budgetMs = 20_000, stepMs = 1_000) {
  for (let elapsed = 0; elapsed < budgetMs; elapsed += stepMs) {
    if (predicate()) return;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stepMs);
    });
  }
  if (!predicate()) throw new Error(`advanceUntil: condition not met within ${budgetMs}ms`);
}

describe("useSwapTransition", () => {
  it("shows the transition the instant startSwap is called, before any poll resolves", () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const onCommitted = vi.fn();
    const onToast = vi.fn();
    const { result } = renderHook(() => useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast }));

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));

    expect(result.current.transition).toEqual({ phase: "switching", appName: "Purple App" });
    expect(onCommitted).not.toHaveBeenCalled();
  });

  it("does not disappear on the response alone — stays 'switching' while state_detail is still 'swapping'", async () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const onCommitted = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast: vi.fn() }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    // First tick fires immediately (before the 1s interval), and several more
    // on the interval — all still "swapping".
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });

    expect(result.current.transition).toEqual({ phase: "switching", appName: "Purple App" });
    expect(onCommitted).not.toHaveBeenCalled();
    expect(getSession.mock.calls.length).toBeGreaterThan(1);
  });

  it("clears and adopts the COMMITTED app_id once state_detail reaches 'swap complete'", async () => {
    getSession
      .mockResolvedValueOnce(session("running", "swapping"))
      .mockResolvedValueOnce(session("running", "swap complete", "a2"));
    const onCommitted = vi.fn();
    const onToast = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await advanceUntil(() => onCommitted.mock.calls.length > 0);
    expect(onCommitted).toHaveBeenCalledWith("a2", "Purple App");

    expect(result.current.transition).toBeNull();
    expect(onToast).toHaveBeenCalled();
  });

  it("surfaces the rollback reason on failure and never calls onCommitted", async () => {
    // The server always commits state_detail="swapping" before it can ever
    // commit a rollback detail (swapper.go sets it synchronously in the
    // request handler; only the later agent callback can write a terminal
    // detail) — a mock that returns the rollback text with no "swapping"
    // ever observed is not a state the real server can produce, and (correctly)
    // cannot resolve here either: see the "does not treat a terminal detail
    // as this swap's own outcome..." test below for why.
    getSession
      .mockResolvedValueOnce(session("running", "swapping"))
      .mockResolvedValueOnce(
        session("running", "swap failed; rolled back: new source container launch failed: boom"),
      );
    const onCommitted = vi.fn();
    const onToast = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    // First tick (immediate) observes "swapping" and arms; the interval tick
    // one second later delivers the rollback.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await advanceUntil(() => result.current.transition?.phase === "error");
    expect(result.current.transition).toEqual({
      phase: "error",
      appName: "Purple App",
      message: "new source container launch failed: boom",
    });
    expect(onCommitted).not.toHaveBeenCalled();
  });

  // The actual root cause of the "second swap in a session" defect: swapper.go
  // does NOT write state_detail="swapping" as the first thing it does (it runs
  // entitlement/reservation/home-in-use checks and resolveHome() first), so
  // GET /v1/sessions/{id} can — on EVERY swap, not just a fluke — still
  // return the PREVIOUS swap's terminal detail for a real window after a new
  // swap has genuinely been requested. A poll tick landing in that window
  // must not mistake old data for this swap's own answer.
  it("does not treat a terminal detail as this swap's own outcome until it has observed 'swapping' at least once", async () => {
    // Every response for this swap is a *stale* terminal detail (as if
    // state_detail is still sitting at "swap complete" from a swap that
    // finished before this one started) — never "swapping" at all.
    getSession.mockResolvedValue(session("running", "swap complete", "STALE_PREVIOUS_APP"));
    const onCommitted = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast: vi.fn() }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });

    // Never armed (never observed "swapping" for THIS swap) — must still be
    // waiting, not have committed the stale app id under the new app's name.
    expect(onCommitted).not.toHaveBeenCalled();
    expect(result.current.transition).toEqual({ phase: "switching", appName: "Purple App" });

    // Once the real "swapping" for this swap finally shows up, followed by
    // the real completion, it resolves normally.
    getSession.mockResolvedValue(session("running", "swapping"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    getSession.mockResolvedValue(session("running", "swap complete", "a2"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });

    await advanceUntil(() => onCommitted.mock.calls.length > 0);
    expect(onCommitted).toHaveBeenCalledWith("a2", "Purple App");
    expect(result.current.transition).toBeNull();
  });

  // In practice onSwapStart/startSwap always fires before a possible
  // onSwapRejected/rejectSwap (SessionQuickSwitch.swap() calls onSwapStart,
  // THEN awaits swapSession — onSwapRejected only fires from that await's
  // catch), so startSwap has always already kicked off its first poll tick by
  // the time rejectSwap can run. rejectSwap's job is to make sure that poll
  // has no further effect, not that it was never started.
  it("rejectSwap goes straight to error and stops any poll startSwap already kicked off", async () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const onCommitted = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast: vi.fn() }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    act(() => result.current.rejectSwap("That app's library lives on another host."));

    expect(result.current.transition).toEqual({
      phase: "error",
      appName: "Purple App",
      message: "That app's library lives on another host.",
    });

    const callsAtReject = getSession.mock.calls.length;
    // Advance past where the stray in-flight tick's interval WOULD have fired
    // again (1s) but short of the terminal auto-dismiss (3.5s, exercised by
    // its own test below) — isolates "no further polling" from "eventually
    // clears on its own".
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });
    // No further polling, and the terminal state set by rejectSwap was not
    // overwritten by a stray in-flight tick.
    expect(getSession.mock.calls.length).toBe(callsAtReject);
    expect(onCommitted).not.toHaveBeenCalled();
    expect(result.current.transition).toEqual({
      phase: "error",
      appName: "Purple App",
      message: "That app's library lives on another host.",
    });
  });

  it("times out rather than hanging forever when the server never answers", async () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const onCommitted = vi.fn();
    const onToast = vi.fn();
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted, onToast }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(75_001);
    });

    expect(result.current.transition).toEqual({ phase: "timeout", appName: "Purple App" });
    expect(onCommitted).not.toHaveBeenCalled();
    expect(onToast).toHaveBeenCalled();

    // No further polling once timed out.
    const callsAtTimeout = getSession.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(getSession.mock.calls.length).toBe(callsAtTimeout);
  });

  it("auto-dismisses a terminal transition after a readable pause", async () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const { result } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted: vi.fn(), onToast: vi.fn() }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    act(() => result.current.rejectSwap("nope"));
    expect(result.current.transition).not.toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_500);
    });
    expect(result.current.transition).toBeNull();
  });

  it("stops polling on unmount", async () => {
    getSession.mockResolvedValue(session("running", "swapping"));
    const { result, unmount } = renderHook(() =>
      useSwapTransition({ authToken: "t", sessionId: "s1", onCommitted: vi.fn(), onToast: vi.fn() }),
    );

    act(() => result.current.startSwap({ id: "a2", name: "Purple App" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const callsBeforeUnmount = getSession.mock.calls.length;
    unmount();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(getSession.mock.calls.length).toBe(callsBeforeUnmount);
  });
});
