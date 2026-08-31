import { afterEach, describe, expect, it, vi } from "vitest";
import { RecoveryController, type RecoveryState } from "./recovery";

afterEach(() => vi.useRealTimers());

describe("RecoveryController", () => {
  it("characterizes connect, degradation, retry, and recovery", () => {
    vi.useFakeTimers();
    const states: RecoveryState[] = [];
    const retry = vi.fn();
    const recovery = new RecoveryController({ onRetry: retry, onState: (state) => states.push(state) });

    recovery.connected();
    recovery.interrupted();
    vi.advanceTimersByTime(0);
    expect(retry).toHaveBeenCalledWith(1);
    recovery.connected();

    expect(states.map(({ phase }) => phase)).toEqual([
      "connecting", "connected", "degraded", "reconnecting", "recovered",
    ]);
  });

  it("fails after bounded backoff retries", () => {
    vi.useFakeTimers();
    const states: RecoveryState[] = [];
    const retry = vi.fn();
    const recovery = new RecoveryController({
      retryDelaysMs: [0, 20, 50],
      onRetry: retry,
      onState: (state) => states.push(state),
    });

    recovery.interrupted();
    vi.runAllTimers();

    expect(retry).toHaveBeenCalledTimes(3);
    expect(states.at(-1)).toMatchObject({ phase: "failed", attempt: 3 });
  });

  // Chrome's native setTimeout/clearTimeout throw "TypeError: Illegal
  // invocation" when invoked with any receiver other than the global object.
  // vi.useFakeTimers() installs plain JS mocks with no receiver check, which is
  // exactly how the detached-default bug (this.setTimer = setTimeout; later
  // this.setTimer(...)) passed every test while crashing on first use in a real
  // browser — recovery never ran in production. Mimic the browser's strictness.
  it("default timers survive Chrome's receiver check (Illegal invocation regression)", () => {
    const g = globalThis as Record<string, unknown>;
    const realSet = globalThis.setTimeout;
    const realClear = globalThis.clearTimeout;
    const strictThis = (fnName: string, real: (...a: never[]) => unknown) =>
      function (this: unknown, ...args: never[]) {
        if (this !== undefined && this !== globalThis) {
          throw new TypeError(`Illegal invocation (${fnName})`);
        }
        return real(...args);
      };
    g.setTimeout = strictThis("setTimeout", realSet as never);
    g.clearTimeout = strictThis("clearTimeout", realClear as never);
    try {
      const retry = vi.fn();
      const recovery = new RecoveryController({ onRetry: retry, onState: () => {} });
      expect(() => recovery.interrupted()).not.toThrow();
      expect(() => recovery.close()).not.toThrow();
    } finally {
      g.setTimeout = realSet;
      g.clearTimeout = realClear;
    }
  });

  // #526 — a takeover is terminal but is NOT `failed`, because `failed` is the
  // phase sessionRuntime mints a replacement token from. Minting after a
  // takeover re-attaches and displaces the tab that displaced this one, whose
  // recovery displaces it back: the ping-pong.
  it("superseded is terminal, distinct from failed, and never retries", () => {
    vi.useFakeTimers();
    const retry = vi.fn();
    const states: RecoveryState[] = [];
    const recovery = new RecoveryController({ onRetry: retry, onState: (state) => states.push(state) });

    recovery.superseded("This session was opened in another tab or window");
    vi.runAllTimers();

    expect(retry).not.toHaveBeenCalled();
    expect(states.at(-1)).toMatchObject({
      phase: "superseded",
      message: "This session was opened in another tab or window",
    });
    expect(states.map(({ phase }) => phase)).not.toContain("failed");
  });

  // The ICE failure that FOLLOWS a takeover (the host is offering to the new
  // peer now, so this one's media path dies seconds later) must not restart the
  // escalation from the other end.
  it("superseded latches: a later interruption cannot re-escalate", () => {
    vi.useFakeTimers();
    const retry = vi.fn();
    const states: RecoveryState[] = [];
    const recovery = new RecoveryController({ onRetry: retry, onState: (state) => states.push(state) });

    recovery.superseded("taken over");
    recovery.interrupted("ICE failed — checking whether the path can recover");
    vi.runAllTimers();

    expect(retry).not.toHaveBeenCalled();
    expect(states.at(-1)).toMatchObject({ phase: "superseded" });
  });

  // A pending retry must be cancelled, not left to fire onto a session that is
  // now owned elsewhere.
  it("superseded clears a pending retry", () => {
    vi.useFakeTimers();
    const retry = vi.fn();
    const recovery = new RecoveryController({
      retryDelaysMs: [50],
      onRetry: retry,
      onState: () => {},
    });

    recovery.interrupted();
    recovery.superseded("taken over");
    vi.runAllTimers();

    expect(retry).not.toHaveBeenCalled();
  });

  it("cancels pending recovery", () => {
    vi.useFakeTimers();
    const retry = vi.fn();
    const states: RecoveryState[] = [];
    const recovery = new RecoveryController({ onRetry: retry, onState: (state) => states.push(state) });

    recovery.interrupted();
    recovery.cancel();
    vi.runAllTimers();

    expect(retry).not.toHaveBeenCalled();
    expect(states.at(-1)?.message).toBe("Recovery cancelled");
  });
});
