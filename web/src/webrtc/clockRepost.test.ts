import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { shouldPostClock, type ClockEstimate, type PostedClock } from "./clockOffset";
import { CLOCK_REPOST_DELTA_MS, CLOCK_REPOST_INTERVAL_S } from "./thresholds";

// The clock offset is no longer a field the control plane merely reports — it is
// APPLIED, to put browser points on the host clock. A stale offset therefore
// skews every aligned series for the rest of the session, which is why the client
// re-posts instead of latching after one post.

const est = (offsetMs: number): ClockEstimate => ({
  clientOffsetMs: offsetMs,
  uncertaintyMs: 4,
});
const posted = (offsetMs: number | null, atMs: number): PostedClock => ({ offsetMs, atMs });

const decide = (e: ClockEstimate | null, p: PostedClock, nowMs: number) =>
  shouldPostClock(e, p, nowMs, CLOCK_REPOST_INTERVAL_S, CLOCK_REPOST_DELTA_MS);

describe("shouldPostClock", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it("never posts without a stable estimate — absence is the honest unmeasured", () => {
    expect(decide(null, posted(null, 0), 10_000)).toBe(false);
    expect(decide(null, posted(-3, 0), 10_000_000)).toBe(false);
  });

  it("posts the first stable estimate immediately", () => {
    expect(decide(est(-3.2), posted(null, 0), 12_345)).toBe(true);
  });

  it("waits out the re-post interval however far the offset moved", () => {
    vi.setSystemTime(new Date(1_000_000));
    const p = posted(-3.2, Date.now());
    vi.advanceTimersByTime((CLOCK_REPOST_INTERVAL_S - 1) * 1000);
    expect(decide(est(500), p, Date.now())).toBe(false);
  });

  it("re-posts after the interval once the offset has actually moved", () => {
    vi.setSystemTime(new Date(1_000_000));
    const p = posted(-3.2, Date.now());
    vi.advanceTimersByTime(CLOCK_REPOST_INTERVAL_S * 1000 + 1);

    // Inside the estimator's own noise: churning measured_at would make a fresh
    // clock look refreshed without the offset being any better.
    expect(decide(est(-3.2 + CLOCK_REPOST_DELTA_MS), p, Date.now())).toBe(false);
    // Beyond it: the server's copy is now wrong by more than the noise floor.
    expect(decide(est(-3.2 + CLOCK_REPOST_DELTA_MS + 0.1), p, Date.now())).toBe(true);
    // Drift in either direction.
    expect(decide(est(-3.2 - CLOCK_REPOST_DELTA_MS - 0.1), p, Date.now())).toBe(true);
  });

  it("keeps re-checking every cadence once the interval has elapsed", () => {
    vi.setSystemTime(new Date(1_000_000));
    const p = posted(0, Date.now());
    vi.advanceTimersByTime(CLOCK_REPOST_INTERVAL_S * 1000 + 1);
    // A quiet clock stays quiet — no post, and (crucially) posted.atMs is NOT
    // advanced by a skipped check, so the next drift is caught on the next tick
    // rather than a minute later.
    expect(decide(est(0.5), p, Date.now())).toBe(false);
    vi.advanceTimersByTime(5_000);
    expect(decide(est(40), p, Date.now())).toBe(true);
  });
});
