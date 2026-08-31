/**
 * Collapses requestVideoFrameCallback ticks that carry the SAME video frame into
 * one presentation sample (#263). On a display whose refresh rate doesn't evenly
 * divide the source fps (observed: ~98 Hz display, flawless 60 fps source), RVFC
 * fires at the display's vsync rate, re-invoking for already-presented frames —
 * a 60 fps stream measured ~43-49 fps raw. Summary-statistic fixes (median-over-
 * mean in presentCadence.ts) can't recover this: the raw samples are polluted,
 * not just the summary. Fix: count a sample only when
 * `VideoFrameCallbackMetadata.mediaTime` (the frame's own PTS) differs from the
 * last one seen. A real frame drop still yields a longer interval between two
 * distinct mediaTimes, so drop detection is unaffected. Pure/dependency-free;
 * the unit test is the whole contract.
 */

/** Running state threaded through successive feedPresentedFrame() calls. */
export interface FrameDedupState {
  /** mediaTime (seconds) of the last DISTINCT frame observed, or null before the first. */
  lastMediaTime: number | null;
  /** performance.now() (ms) at which the last DISTINCT frame was observed, or null before the first. */
  lastNow: number | null;
}

/** Fresh state for the start of a session or a new drain window. */
export const INITIAL_FRAME_DEDUP_STATE: FrameDedupState = {
  lastMediaTime: null,
  lastNow: null,
};

export interface FrameDedupResult {
  /** null when this callback produced no new sample (first frame, or a duplicate). */
  intervalMs: number | null;
  /** State to pass into the next feedPresentedFrame() call. */
  nextState: FrameDedupState;
}

/**
 * Fold one requestVideoFrameCallback tick into the running dedup state.
 * `mediaTime` null/undefined (no VideoFrameCallbackMetadata, rAF fallback) treats
 * every callback as distinct.
 */
export function feedPresentedFrame(
  state: FrameDedupState,
  mediaTime: number | null | undefined,
  nowMs: number,
): FrameDedupResult {
  const hasMediaTime = typeof mediaTime === "number" && Number.isFinite(mediaTime);

  if (hasMediaTime && state.lastMediaTime != null && mediaTime === state.lastMediaTime) {
    // Not a sample; leave state untouched so the next distinct frame's interval
    // spans the full elapsed gap, not just the tail (#263).
    return { intervalMs: null, nextState: state };
  }

  const intervalMs = state.lastNow != null ? nowMs - state.lastNow : null;
  return {
    intervalMs,
    nextState: {
      lastMediaTime: hasMediaTime ? (mediaTime as number) : state.lastMediaTime,
      lastNow: nowMs,
    },
  };
}
