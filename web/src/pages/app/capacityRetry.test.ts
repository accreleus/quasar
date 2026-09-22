import { describe, expect, it } from "vitest";
import {
  decideCapacityRetry,
  DEFAULT_RETRY_DELAY_MS,
  MAX_CAPACITY_RETRY_WAIT_MS,
  MAX_NO_HOST_RETRY_WAIT_MS,
  MIN_RETRY_DELAY_MS,
  NO_HOST_RETRY_DELAY_MS,
  waitingToastCopy,
} from "./capacityRetry";

describe("waitingToastCopy", () => {
  it("names the hand-picked codec: the wait is for a GPU that can encode it (#304)", () => {
    expect(waitingToastCopy("slot", "Portal", "av1")).toEqual({
      title: "Waiting for a GPU that can encode AV1…",
      body: "Portal will launch as soon as one is free.",
    });
    expect(waitingToastCopy("host", "Portal", "h265")).toEqual({
      title: "Waiting for a GPU that can encode HEVC to come online…",
      body: "Portal will launch as soon as one is ready.",
    });
  });

  it("keeps the slot and host copy for a launch without a codec", () => {
    expect(waitingToastCopy("slot", "Portal")).toEqual({
      title: "Waiting for a slot to free up…",
      body: "Portal will launch as soon as one is free.",
    });
    expect(waitingToastCopy("host", "Portal", undefined)).toEqual({
      title: "Waiting for a host to come online…",
      body: "Portal will launch as soon as a host is ready.",
    });
  });
});

describe("decideCapacityRetry", () => {
  it("honours the server's Retry-After over the default delay", () => {
    expect(decideCapacityRetry({ elapsedMs: 0, retryAfterSeconds: 2 })).toEqual({
      kind: "retry",
      delayMs: 2000,
    });
  });

  it("falls back to the default delay when the server sent no Retry-After", () => {
    expect(decideCapacityRetry({ elapsedMs: 0, retryAfterSeconds: undefined })).toEqual({
      kind: "retry",
      delayMs: DEFAULT_RETRY_DELAY_MS,
    });
  });

  it("floors Retry-After: 0 at MIN_RETRY_DELAY_MS instead of retrying immediately", () => {
    // A hot loop against a still-full host helps nobody — 0 is a valid
    // server answer, but it is not license to hammer the launch endpoint.
    expect(decideCapacityRetry({ elapsedMs: 0, retryAfterSeconds: 0 })).toEqual({
      kind: "retry",
      delayMs: MIN_RETRY_DELAY_MS,
    });
  });

  it("keeps retrying while the next wait still fits inside the cap", () => {
    // 55s elapsed + a 5s default delay lands exactly on the 60s cap.
    expect(decideCapacityRetry({ elapsedMs: 55_000, retryAfterSeconds: undefined })).toEqual({
      kind: "retry",
      delayMs: DEFAULT_RETRY_DELAY_MS,
    });
  });

  it("clamps a delay that would overshoot the cap to the remaining budget instead of giving up", () => {
    // Only 4s of budget left; the default 5s delay is clamped down to it
    // rather than refusing to try again while time remains.
    expect(decideCapacityRetry({ elapsedMs: 56_000, retryAfterSeconds: undefined })).toEqual({
      kind: "retry",
      delayMs: 4_000,
    });
  });

  it("gives up once even the minimum delay would push elapsed time past the cap", () => {
    // 400ms of budget left; MIN_RETRY_DELAY_MS's floor would overshoot it.
    expect(decideCapacityRetry({ elapsedMs: 59_600, retryAfterSeconds: undefined })).toEqual({
      kind: "give-up",
    });
  });

  it("gives up immediately when already at the cap", () => {
    expect(
      decideCapacityRetry({ elapsedMs: MAX_CAPACITY_RETRY_WAIT_MS, retryAfterSeconds: 1 }),
    ).toEqual({ kind: "give-up" });
  });

  it("gives up against a caller-supplied cap once the floor would overshoot it", () => {
    // 400ms left of a 5s cap; even the 1s floor overshoots it.
    expect(decideCapacityRetry({ elapsedMs: 4_600, retryAfterSeconds: 5 }, 5_000)).toEqual({
      kind: "give-up",
    });
  });

  it("retries with the clamped remainder against a caller-supplied cap when the floor still fits", () => {
    // 1s left of a 5s cap — exactly MIN_RETRY_DELAY_MS, so it still retries.
    expect(decideCapacityRetry({ elapsedMs: 4_000, retryAfterSeconds: 5 }, 5_000)).toEqual({
      kind: "retry",
      delayMs: 1_000,
    });
  });

  it("clamps an oversized server Retry-After to the remaining budget rather than disabling retry", () => {
    // The server says 90s; only the whole 60s budget is available, so the
    // client spends all of it on one more attempt instead of giving up with
    // the server's own signal that a slot is coming.
    expect(decideCapacityRetry({ elapsedMs: 0, retryAfterSeconds: 90 })).toEqual({
      kind: "retry",
      delayMs: MAX_CAPACITY_RETRY_WAIT_MS,
    });
  });

  // no_host_available's own budget/delay, used via the caller-supplied
  // maxWaitMs/defaultDelayMs params (useLaunch.ts wires these when the error
  // code is no_host_available).
  describe("no_host_available budget", () => {
    it("uses NO_HOST_RETRY_DELAY_MS as the default delay, not capacity_exhausted's 5s", () => {
      expect(
        decideCapacityRetry(
          { elapsedMs: 0, retryAfterSeconds: undefined },
          MAX_NO_HOST_RETRY_WAIT_MS,
          NO_HOST_RETRY_DELAY_MS,
        ),
      ).toEqual({ kind: "retry", delayMs: NO_HOST_RETRY_DELAY_MS });
    });

    it("gives up at the 20s no-host cap rather than capacity_exhausted's 60s", () => {
      expect(
        decideCapacityRetry(
          { elapsedMs: MAX_NO_HOST_RETRY_WAIT_MS, retryAfterSeconds: undefined },
          MAX_NO_HOST_RETRY_WAIT_MS,
          NO_HOST_RETRY_DELAY_MS,
        ),
      ).toEqual({ kind: "give-up" });
    });

    it("still retries capacity_exhausted's full 60s budget when no override is passed", () => {
      expect(
        decideCapacityRetry({ elapsedMs: 59_000, retryAfterSeconds: undefined }),
      ).toEqual({ kind: "retry", delayMs: 1_000 });
    });

    it("judges the same elapsed time against each code's own budget", () => {
      // useLaunch.ts carries one elapsedMs across a no_host_available <->
      // capacity_exhausted flip rather than resetting it; this only checks
      // that decideCapacityRetry, given that same elapsedMs, applies whichever
      // budget/delay the caller passes for the current code.
      const elapsed = 18_000; // most of the 20s no-host budget already spent
      const noHostDecision = decideCapacityRetry(
        { elapsedMs: elapsed, retryAfterSeconds: undefined },
        MAX_NO_HOST_RETRY_WAIT_MS,
        NO_HOST_RETRY_DELAY_MS,
      );
      expect(noHostDecision).toEqual({ kind: "retry", delayMs: NO_HOST_RETRY_DELAY_MS });

      // Same elapsedMs, now judged against capacity_exhausted's larger 60s
      // budget/5s default delay — the 18s already spent is not given back.
      const capacityDecision = decideCapacityRetry({
        elapsedMs: elapsed,
        retryAfterSeconds: undefined,
      });
      expect(capacityDecision).toEqual({ kind: "retry", delayMs: DEFAULT_RETRY_DELAY_MS });
    });
  });
});
