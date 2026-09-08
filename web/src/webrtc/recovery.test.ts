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

  describe("#128 — signalling health is tracked apart from media health", () => {
    const build = () => {
      const retry = vi.fn();
      const states: RecoveryState[] = [];
      const recovery = new RecoveryController({
        onRetry: retry,
        onState: (state) => states.push(state),
      });
      return { retry, states, recovery };
    };

    it("holds the media retry ladder while signalling is down, and runs it after", () => {
      // onRetry sends restart_ice over the signalling socket, and wsSend drops
      // silently when that socket is closed. Running the ladder during an
      // outage spends all three attempts on nothing in 15 s and terminalises a
      // session the agent would have held for 120 s.
      vi.useFakeTimers();
      const { retry, recovery } = build();

      recovery.signalingLost("signaling closed (1006)");
      recovery.interrupted("media wobbled");
      vi.advanceTimersByTime(60_000);
      expect(retry).not.toHaveBeenCalled();

      recovery.signalingRestored();
      vi.advanceTimersByTime(1);
      expect(retry).toHaveBeenCalledTimes(1);
    });

    it("does not republish an unchanged signalling outage", () => {
      // Media telemetry calls connected() every tick; re-emitting a fresh state
      // object each time defeats the snapshot dedupe upstream.
      vi.useFakeTimers();
      const { states, recovery } = build();

      recovery.signalingLost("signaling closed (1006)");
      const after = states.length;
      recovery.connected();
      recovery.connected();
      recovery.connected();

      expect(states.length).toBe(after);
      expect(states.at(-1)?.phase).toBe("signaling-lost");
    });

    it("emits one signalling outage however many closes arrive", () => {
      vi.useFakeTimers();
      const { states, recovery } = build();

      recovery.signalingLost("first");
      recovery.signalingLost("second");

      expect(states.filter((s) => s.phase === "signaling-lost").length).toBe(1);
    });

    it("returns to connected once signalling is back and media never faltered", () => {
      vi.useFakeTimers();
      const { states, recovery } = build();

      recovery.signalingLost("signaling closed (1006)");
      recovery.signalingRestored();

      expect(states.at(-1)?.phase).toBe("connected");
    });

    it("reports a deferred ladder as NOT in flight, so the rebind sends one restart", () => {
      // The session sends restart_ice for a ladder with requests already on the
      // wire; a deferred ladder sends its own when signalingRestored() starts
      // it. Counting deferred here puts two ICE-restart offers on the wire
      // before either is answered, which the control plane cannot dedupe.
      vi.useFakeTimers();
      const { recovery } = build();

      recovery.signalingLost("down");
      recovery.interrupted("media wobbled");

      expect(recovery.mediaRetryInFlight()).toBe(false);
    });

    it("stands a mid-flight ladder down when signalling drops, and resumes it after", () => {
      vi.useFakeTimers();
      const { retry, recovery } = build();

      recovery.interrupted("media wobbled"); // ladder starts, attempt 1 fires at 0ms
      vi.advanceTimersByTime(1);
      expect(retry).toHaveBeenCalledTimes(1);

      recovery.signalingLost("down"); // socket gone mid-ladder
      vi.advanceTimersByTime(60_000);
      expect(retry).toHaveBeenCalledTimes(1); // no attempt spent on a closed socket

      // The resumed ladder keeps its place: attempt 2 waits the second rung's
      // 5 s, it does not restart from zero.
      recovery.signalingRestored();
      vi.advanceTimersByTime(1);
      expect(retry).toHaveBeenCalledTimes(1);
      vi.advanceTimersByTime(5_000);
      expect(retry).toHaveBeenCalledTimes(2);
    });

    it("drops a deferred ladder when media recovers on its own", () => {
      vi.useFakeTimers();
      const { retry, recovery } = build();

      recovery.signalingLost("down");
      recovery.interrupted("media wobbled");
      recovery.connected(); // media healed while signalling was still down
      recovery.signalingRestored();
      vi.advanceTimersByTime(60_000);

      expect(retry).not.toHaveBeenCalled();
    });

    it("stays terminal: a signalling loss after `failed` changes nothing", () => {
      vi.useFakeTimers();
      const { states, recovery } = build();

      recovery.terminal("media gone");
      recovery.signalingLost("signaling closed (1006)");

      expect(states.at(-1)?.phase).toBe("failed");
    });
  });
});
