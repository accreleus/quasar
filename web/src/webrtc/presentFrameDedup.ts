/**
 * Presentation samples from requestVideoFrameCallback (#263, #552).
 *
 * TWO sources, and they must never be mixed:
 *
 * 1. RVFC frame metadata (feedPresentedFrames): count presented frames from the
 *    UA's own `presentedFrames` counter and take intervals from
 *    `expectedDisplayTime` (else `presentationTime`) — times the display showed
 *    distinct frames. A callback that coalesced N presentations contributes one
 *    per-frame interval (elapsed / N) PER presented frame, so a skipped callback
 *    is not one long frame; the page's callback cadence no longer moves the
 *    cadence metrics. LIMIT: the N frames inside one coalesced callback share
 *    that averaged interval, so a long frame covered by one callback cannot be
 *    seen (it is averaged across the frames that callback counted).
 * 2. Callback cadence (feedPresentedFrame): the requestAnimationFrame fallback,
 *    and RVFC metadata without presentedFrames, have no display clock. Samples
 *    come from when the callback runs; telemetry labels them as callback cadence
 *    (TelemetrySnapshot.presentSource), never as display pacing.
 *
 * #263's mediaTime dedup survives for source 2 only: on a display whose refresh
 * rate doesn't evenly divide the source fps (observed: ~98 Hz display, flawless
 * 60 fps source), RVFC fires at the display's vsync rate, re-invoking for
 * already-presented frames. Source 1 detects those as a presentedFrames delta of
 * 0. Pure/dependency-free; the unit tests are the whole contract.
 */

/** Running state threaded through successive feedPresentedFrame() calls. */
export interface FrameDedupState {
  /** Key of the last DISTINCT frame observed (presentedFrameKey: mediaTime in seconds, or an RTP
   *  timestamp; compared for equality only), or null before the first. */
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
 * `mediaTime` is the frame key from presentedFrameKey(). null/undefined (no
 * VideoFrameCallbackMetadata, rAF fallback) treats every callback as distinct.
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

/** Running state threaded through successive RVFC-metadata presentation samples. */
export interface PresentFrameMetadataState {
  /** The UA's `presentedFrames` at the last counted callback, or null before the first. */
  lastPresentedFrames: number | null;
  /** Display time of the last counted frame (ms, RVFC metadata clock), or null before the first. */
  lastDisplayMs: number | null;
}

/** Fresh state for the start of a session or a new cadence run. */
export const INITIAL_PRESENT_FRAME_METADATA_STATE: PresentFrameMetadataState = {
  lastPresentedFrames: null,
  lastDisplayMs: null,
};

export interface PresentFrameMetadataResult {
  /** Presentations the UA counted since the last counted callback (presentedFrames delta); 0 for a duplicate tick. */
  frames: number;
  /** Display-to-display interval, averaged per presented frame (elapsed / frames). The caller must add it ONCE PER presented frame (`frames` times), not once per callback. Null when there is nothing to measure from. */
  intervalMs: number | null;
  /** State to pass into the next feedPresentedFrames() call. */
  nextState: PresentFrameMetadataState;
}

/**
 * The instant a displayed frame is shown on the RVFC metadata clock (#552):
 * `expectedDisplayTime` (when the UA expects it visible) when the UA supplies it,
 * else `presentationTime` (when it was submitted for composition). Both are
 * DOMHighResTimeStamps on the page's own clock, so differences between them are
 * display intervals with no performance.now() sampling error. Null when the UA
 * gave neither — no display time, so no display-paced sample.
 */
export function presentedDisplayMs(
  metadata: Partial<Pick<VideoFrameCallbackMetadata, "expectedDisplayTime" | "presentationTime">> | undefined,
): number | null {
  const expected = metadata?.expectedDisplayTime;
  if (typeof expected === "number" && Number.isFinite(expected)) return expected;
  const presentation = metadata?.presentationTime;
  if (typeof presentation === "number" && Number.isFinite(presentation)) return presentation;
  return null;
}

/**
 * Fold one RVFC callback into the running metadata state (#552). `frames` is the
 * UA's own presented-frame delta: a callback the UA coalesced over several
 * presentations counts all of them, and its interval is divided by that count —
 * the display cadence per frame, not one callback-to-callback span read as a
 * long frame. The caller adds that interval once PER presented frame, so a
 * coalesced callback contributes `frames` samples and a per-frame weighting.
 * A delta of 0 is a tick for an already-presented frame and yields no sample
 * (state untouched, so the next new frame's interval still spans the real gap);
 * a negative delta means the UA's counter restarted, and reseeds.
 */
export function feedPresentedFrames(
  state: PresentFrameMetadataState,
  presentedFrames: number,
  displayMs: number,
): PresentFrameMetadataResult {
  if (state.lastPresentedFrames == null) {
    return {
      frames: 0,
      intervalMs: null,
      nextState: { lastPresentedFrames: presentedFrames, lastDisplayMs: displayMs },
    };
  }
  const delta = presentedFrames - state.lastPresentedFrames;
  if (delta < 0) {
    // The UA's counter restarted (a new video element or callback run): reseed
    // rather than suppressing every future sample as "duplicate".
    return {
      frames: 0,
      intervalMs: null,
      nextState: { lastPresentedFrames: presentedFrames, lastDisplayMs: displayMs },
    };
  }
  if (delta === 0) return { frames: 0, intervalMs: null, nextState: state };

  const elapsed = state.lastDisplayMs != null ? displayMs - state.lastDisplayMs : null;
  return {
    frames: delta,
    intervalMs: elapsed != null && elapsed > 0 ? elapsed / delta : null,
    nextState: { lastPresentedFrames: presentedFrames, lastDisplayMs: displayMs },
  };
}

/**
 * The frame identity for feedPresentedFrame. The spec lets `mediaTime` be 0 for WebRTC
 * and Firefox always reports 0, which would drop every frame as a duplicate; the RTP
 * timestamp is per frame there. Any non-zero `mediaTime` is used as before. A callback
 * with `mediaTime` 0 and no RTP timestamp keys as 0 (the pre-#686 behaviour).
 */
export function presentedFrameKey(
  metadata: Pick<VideoFrameCallbackMetadata, "mediaTime" | "rtpTimestamp"> | undefined,
): number | undefined {
  if (metadata?.mediaTime === 0 && typeof metadata.rtpTimestamp === "number") {
    return metadata.rtpTimestamp;
  }
  return metadata?.mediaTime;
}
