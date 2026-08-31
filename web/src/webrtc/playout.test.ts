import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import {
  PlayoutController,
  PLAYOUT_FLOOR_MS,
  resolvePlayoutMs,
  playoutOverride,
  resolveInitialPlayoutMs,
} from "./playout";

// A stub receiver that captures every applied playout target. The controller only ever
// touches `playoutDelayHint` / `jitterBufferTarget` on it (both best-effort, guarded).
function stubReceiver(): RTCRtpReceiver {
  return {} as RTCRtpReceiver;
}

const EVAL_MS = 5_000;

/**
 * Build a controller whose applied targets are recorded, plus a driver that feeds N
 * telemetry samples of a given health and advances one 5 s evaluation window.
 */
function harness(playout0Ms: number) {
  const applied: number[] = [];
  const ctl = new PlayoutController({
    receiver: stubReceiver(),
    playout0Ms,
    onChange: (ms) => applied.push(ms),
  });
  ctl.start();
  // start() applies playout0 once synchronously.
  return {
    ctl,
    applied,
    /** Feed one healthy window (σ well below the 12 ms healthy line) and evaluate. */
    healthyWindow() {
      ctl.sample({ sdMs: 2, freezeCount: 0 });
      vi.advanceTimersByTime(EVAL_MS);
    },
    /** Feed one degraded window (a freeze) and evaluate. */
    freezeWindow() {
      ctl.sample({ sdMs: 2, freezeCount: 1 });
      vi.advanceTimersByTime(EVAL_MS);
    },
    /** Feed one hysteresis-band window (12 < σ < 18, no freeze) and evaluate. */
    bandWindow() {
      ctl.sample({ sdMs: 15, freezeCount: 0 });
      vi.advanceTimersByTime(EVAL_MS);
    },
  };
}

describe("PlayoutController — IL-1 verdict-aware fast descent", () => {
  beforeEach(() => {
    // Fake `performance.now` too (not faked by default): the controller reads it for its
    // hold deadlines, so it must advance in lockstep with the interval clock or the
    // hold-timing assertions below reason about a real wall-clock `now` and a fake timer.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "performance"] });
    // Console noise from the state-transition trail would clutter the run.
    vi.spyOn(console, "info").mockImplementation(() => {});
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("descends to the floor within ~15 s of evaluations once a healthy streak establishes fast descent", () => {
    // Start at the 720p30 tier's playout₀ (100 ms). With the fast regime (−15 ms after a
    // 3-window healthy streak) the buffer must reach the 50 ms floor quickly.
    const h = harness(100);
    // 3 healthy windows to arm fast descent (15 s), then keep going. Track when we hit floor.
    let windows = 0;
    while (h.ctl.currentMs() > PLAYOUT_FLOOR_MS && windows < 20) {
      h.healthyWindow();
      windows++;
    }
    expect(h.ctl.currentMs()).toBe(PLAYOUT_FLOOR_MS);
    // Slow (−10) for the first 2 windows: 100→90→80, then fast (−15): 80→65→50 (floor,
    // 2026-08-18: floor is now 50 so the descent stops exactly here — 4 windows = 20 s).
    // 100,90,80 (slow) then 65,50 (fast, floored).
    expect(h.applied).toEqual([100, 90, 80, 65, 50]);
  });

  it("collapses the post-up-step hold to ~10 s once healthy (vs the base 30 s), then resumes descent", () => {
    // Get the buffer to the floor, then force an up-step and time how fast it re-descends.
    const h = harness(50); // starts at floor already (2026-08-18: floor is 50)
    // A freeze inflates ×1.5 → 75 and arms the FULL 30 s hold + resets the streak.
    h.freezeWindow();
    expect(h.ctl.currentMs()).toBe(75);

    // Now feed healthy windows. Under the BASE regime the 30 s hold would block descent
    // for 6 windows. IL-1: after 3 healthy windows (fast armed) the hold is retroactively
    // clamped to +10 s, so descent resumes far sooner.
    // window 1 (t≈+5s):  streak1, slow, hold active (30 s) → HOLD (still 75)
    h.healthyWindow();
    expect(h.ctl.currentMs()).toBe(75);
    // window 2 (t≈+10s): streak2, slow, hold active → HOLD
    h.healthyWindow();
    expect(h.ctl.currentMs()).toBe(75);
    // window 3 (t≈+15s): streak3 → FAST; hold clamped to now+10s. 15 s have elapsed since
    // the up-step (> 10 s), so the clamp puts holdUntil in the past → descent resumes: −15 → 60.
    h.healthyWindow();
    expect(h.ctl.currentMs()).toBe(60);
    // window 4 (t≈+20s): still fast → −15 → 45, clamped to the 50 ms floor.
    h.healthyWindow();
    expect(h.ctl.currentMs()).toBe(PLAYOUT_FLOOR_MS);
    // Total elapsed to resume descent after the up-step: ~15 s, not the base 30 s.
  });

  it("a degradation mid-descent steps up ×1.5, re-arms the full 30 s hold, and resets the streak (asymmetry preserved)", () => {
    const h = harness(100);
    // Arm fast descent and drop a few steps.
    h.healthyWindow(); // 90
    h.healthyWindow(); // 80
    h.healthyWindow(); // 65 (fast)
    h.healthyWindow(); // 50 (fast)
    expect(h.ctl.currentMs()).toBe(50);

    // Degrade: immediate ×1.5 up-step (50 → 75) regardless of the healthy history.
    h.freezeWindow();
    expect(h.ctl.currentMs()).toBe(75);

    // The full 30 s hold is re-armed AND the streak reset. Descent must NOT resume off the
    // stale (pre-freeze) fast streak: the first two healthy windows (streak rebuilding 1→2,
    // still the conservative regime) hold — no fast shortcut without a FRESH 3-window streak.
    h.healthyWindow(); // streak1, slow, within 30 s hold → HOLD
    expect(h.ctl.currentMs()).toBe(75);
    h.healthyWindow(); // streak2, slow, within 30 s hold → HOLD
    expect(h.ctl.currentMs()).toBe(75);
    // Only on the 3rd fresh healthy window does fast re-arm and the (now ~10 s) hold clear.
    h.healthyWindow(); // streak3 → fast, hold re-based to up-step+10 s → descent −15 → 60
    expect(h.ctl.currentMs()).toBe(60);
  });

  it("hysteresis-band windows (12 < σ < 18) neither trim, inflate, nor advance the streak", () => {
    const h = harness(100);
    // Two band windows: no change, streak stays 0.
    h.bandWindow();
    h.bandWindow();
    expect(h.ctl.currentMs()).toBe(100);
    // A band window between healthy windows must not let the streak reach fast: it pauses
    // (does not reset) the streak, so we still need a full run of healthy windows.
    h.healthyWindow(); // streak1 → slow −10 → 90
    h.bandWindow(); // streak paused at 1, no change → 90
    h.healthyWindow(); // streak2 → slow −10 → 80
    expect(h.ctl.currentMs()).toBe(80);
    // Only the third *consecutive-since-band* healthy window arms fast.
    h.healthyWindow(); // streak3 → fast −15 → 65
    expect(h.ctl.currentMs()).toBe(65);
  });

  it("never descends below the floor and clamps the floor to playout₀ when it starts low", () => {
    const h = harness(90);
    for (let i = 0; i < 10; i++) h.healthyWindow();
    expect(h.ctl.currentMs()).toBe(PLAYOUT_FLOOR_MS);

    // A controller that starts BELOW the nominal floor keeps that lower start as its floor.
    const low = harness(20);
    for (let i = 0; i < 10; i++) low.healthyWindow();
    expect(low.ctl.currentMs()).toBe(20); // floor clamped to playout₀=20, never re-inflated
  });

  it("caps re-inflation at 150 ms under sustained degradation", () => {
    const h = harness(120);
    // Repeated freezes: 120 → 150 (cap), never above.
    h.freezeWindow(); // 120*1.5=180 → capped 150
    expect(h.ctl.currentMs()).toBe(150);
    h.freezeWindow(); // stays at cap
    expect(h.ctl.currentMs()).toBe(150);
  });

  it("holds (does not guess) when a window has neither σ samples nor freezes", () => {
    const h = harness(100);
    // No sample() call this window (e.g. hidden tab → RVFC stopped).
    vi.advanceTimersByTime(EVAL_MS);
    expect(h.ctl.currentMs()).toBe(100);
    // ...and the empty window must not advance the healthy streak.
    h.healthyWindow(); // streak1 → slow → 90
    h.healthyWindow(); // streak2 → slow → 80
    h.healthyWindow(); // streak3 → fast → 65
    expect(h.ctl.currentMs()).toBe(65);
  });
});

describe("playout override / resolution (unchanged by IL-1)", () => {
  it("?playout= override disables the controller path entirely (still an absolute value)", () => {
    expect(playoutOverride("?playout=30")).toBe(30);
    expect(playoutOverride("?playout=0")).toBe(0);
    expect(playoutOverride("")).toBeNull();
    expect(playoutOverride("?foo=1")).toBeNull();
    expect(playoutOverride("?playout=-5")).toBeNull();
    expect(playoutOverride("?playout=nope")).toBeNull();
  });

  it("initial playout resolves override > tier playout₀ > default", () => {
    expect(resolveInitialPlayoutMs(50, "?playout=30")).toBe(30); // override wins
    expect(resolveInitialPlayoutMs(50, "")).toBe(50); // tier playout₀
    expect(resolveInitialPlayoutMs(undefined, "")).toBe(100); // default
  });

  it("resolvePlayoutMs falls back to the default on absent/invalid", () => {
    expect(resolvePlayoutMs("?playout=30")).toBe(30);
    expect(resolvePlayoutMs("")).toBe(100);
    expect(resolvePlayoutMs("?playout=-1")).toBe(100);
  });
});
