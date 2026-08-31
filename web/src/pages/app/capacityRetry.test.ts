import { describe, expect, it } from "vitest";
import {
  decideCapacityRetry,
  DEFAULT_RETRY_DELAY_MS,
  MAX_CAPACITY_RETRY_WAIT_MS,
  MIN_RETRY_DELAY_MS,
} from "./capacityRetry";

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
});
