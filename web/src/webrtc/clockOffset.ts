// Client<->host clock-offset estimation from ping/pong RTT samples (telemetry.ts
// pings every 500ms):
//
//     rtt    = now - sendTc                 (client-clock round trip, ms)
//     offset = hostTs - (sendTc + now) / 2  (host-clock - client-clock, ms)
//
// `offset` sign follows the operational rule the diagnostic bundle reads: ADD it
// to a browser timestamp to get the host clock (contract-amendment.md §A.2,
// trace-format.md §4).
//
// Estimator: NTP min-RTT filter (RFC 5905 §10) — the lowest-RTT sample has the
// least one-way-asymmetry corruption, so its offset is the best estimate and
// minRtt/2 is the honest uncertainty bound (never false precision).
//
// Stability gate: v1 posts ONE offset per session once stable. Below
// MIN_CLOCK_SAMPLES nothing posts, so a channel that never carries pongs (prod
// node-agent doesn't answer pings) leaves the server's clock row ABSENT ->
// bundle reports `unmeasured`, never offset 0 (trace-format.md §4).

/** One ping/pong RTT/offset sample (telemetry.ts onPong). */
export interface ClockSample {
  rtt: number;
  /** host-clock - client-clock offset implied by this pong (ms). */
  offset: number;
}

/** A stable clock-offset estimate, ready to persist. */
export interface ClockEstimate {
  /** host-clock - client-clock offset (ms); add to a browser ts to get host clock. */
  clientOffsetMs: number;
  /** minRtt/2, the one-way-asymmetry error bound. */
  uncertaintyMs: number;
}

/** Floor against a single early-outlier RTT pinning the offset (~4s warm-up at 500ms cadence). */
export const MIN_CLOCK_SAMPLES = 8;

/**
 * NTP min-RTT filter over collected samples. Returns null when there are not yet
 * enough samples — caller must NOT post; absence is the honest "unmeasured" state,
 * never a synthesized offset 0.
 */
export function estimateClockOffset(samples: ClockSample[]): ClockEstimate | null {
  if (samples.length < MIN_CLOCK_SAMPLES) return null;

  // Seed `best` with the first FINITE sample: a leading NaN/Inf rtt would sit in
  // `best` forever since every `<` comparison against NaN is false.
  let best: ClockSample | null = null;
  for (const s of samples) {
    if (!Number.isFinite(s.rtt) || !Number.isFinite(s.offset)) continue;
    if (best === null || s.rtt < best.rtt) best = s;
  }
  if (best === null) return null;

  return {
    clientOffsetMs: best.offset,
    uncertaintyMs: best.rtt / 2,
  };
}

/**
 * What the client has already told the server about its clock. `offsetMs` is
 * null until the first successful post.
 */
export interface PostedClock {
  offsetMs: number | null;
  atMs: number;
}

/**
 * Whether the client should (re-)post its clock-offset estimate. Rules in order:
 * no stable estimate -> never post (absence is honest "unmeasured", not a
 * synthesized 0); nothing posted yet -> post; inside the re-post interval ->
 * wait; moved by no more than delta -> skip (inside estimator noise; re-posting
 * would just churn `measured_at`, which staleness reads from).
 */
export function shouldPostClock(
  estimate: ClockEstimate | null,
  posted: PostedClock,
  nowMs: number,
  intervalS: number,
  deltaMs: number,
): boolean {
  if (estimate === null) return false;
  if (posted.offsetMs === null) return true;
  if (nowMs - posted.atMs < intervalS * 1000) return false;
  return Math.abs(estimate.clientOffsetMs - posted.offsetMs) > deltaMs;
}
