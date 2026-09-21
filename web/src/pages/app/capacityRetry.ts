/**
 * #494: `503 capacity_exhausted` right after launch is transient — the encode-slot
 * reservation holds through `stopping` (#489 overlap prevention), so a relaunch
 * right after a peer's DELETE bounces once and clears within ~15s. Retry with
 * backoff instead of erroring on the first bounce.
 *
 * `decideCapacityRetry` is the pure decision: given elapsed wait and the server's
 * `Retry-After` (client.ts's `ApiError.retryAfterSeconds`), returns retry-with-delay
 * or give-up. No I/O/timers — caller (AppHomeNext's launchApp) owns the `setTimeout`.
 */

/** Retry-After to assume when the server sent none (matches the control
 *  plane's own default — handler.go's `capacityExhaustedRetryAfterSeconds`). */
export const DEFAULT_RETRY_DELAY_MS = 5_000;

/** Total time a launch may sit retrying capacity_exhausted before giving up (#494). */
export const MAX_CAPACITY_RETRY_WAIT_MS = 60_000;

/** Total time a launch may sit retrying `no_host_available` (#288): the control
 *  plane's handshake cap is 15s (a reconnecting agent's first `capacity` report
 *  lands inside that window), so 20s covers the cap plus one extra retry. */
export const MAX_NO_HOST_RETRY_WAIT_MS = 20_000;

/** Floor between attempts — a `Retry-After: 0` or near-zero remainder must never
 *  become a busy loop against a host that is still full. */
export const MIN_RETRY_DELAY_MS = 1_000;

/** Default delay for `no_host_available` (no `Retry-After` on this code): the
 *  window is usually under 3s, so retry sooner than capacity_exhausted's 5s
 *  default — floored at {@link MIN_RETRY_DELAY_MS}. */
export const NO_HOST_RETRY_DELAY_MS = MIN_RETRY_DELAY_MS;

export interface CapacityRetryState {
  /** Elapsed since the FIRST capacity_exhausted response, not the most recent —
   *  the budget covers the whole wait, not a per-attempt window. */
  elapsedMs: number;
  /** Server's `Retry-After` in seconds. `undefined` (no header) falls back to
   *  {@link DEFAULT_RETRY_DELAY_MS}; `0` (retry immediately) does not. */
  retryAfterSeconds?: number;
}

export type CapacityRetryDecision =
  | { kind: "retry"; delayMs: number }
  | { kind: "give-up" };

/**
 * Decide whether another launch attempt is worth making. The requested delay is
 * the server's Retry-After when sent, else {@link DEFAULT_RETRY_DELAY_MS}, then
 * clamped to the remaining budget (an oversized Retry-After spends the rest of
 * the budget on one more attempt rather than giving up early) and floored at
 * {@link MIN_RETRY_DELAY_MS} (never busy-loop a still-full host). Gives up only
 * once even the floor would push elapsed time past `maxWaitMs`.
 */
export function decideCapacityRetry(
  state: CapacityRetryState,
  maxWaitMs: number = MAX_CAPACITY_RETRY_WAIT_MS,
  defaultDelayMs: number = DEFAULT_RETRY_DELAY_MS,
): CapacityRetryDecision {
  const remainingMs = maxWaitMs - state.elapsedMs;
  if (remainingMs <= 0) {
    return { kind: "give-up" };
  }

  const requestedMs =
    state.retryAfterSeconds !== undefined && state.retryAfterSeconds >= 0
      ? state.retryAfterSeconds * 1000
      : defaultDelayMs;

  const delayMs = Math.max(Math.min(requestedMs, remainingMs), MIN_RETRY_DELAY_MS);

  if (state.elapsedMs + delayMs > maxWaitMs) {
    return { kind: "give-up" };
  }
  return { kind: "retry", delayMs };
}
