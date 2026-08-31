// Multi-codec spec §6.1 — pure decode-failure detector, extracted from
// SessionPage for testability (mirrors sessionSummary.ts: SessionPage owns
// only a ref + wiring to SessionTelemetry.setDecodeFailed, decisions live here).
//
// Anchored on FIRST BYTE RECEIVED, not track arrival: onTrack fires before
// media flows, so anchoring there can false-positive a slow-but-healthy
// session (bytes can legitimately arrive >2s after track add).
//
// Requires bytes GROWING across >=2 consecutive polls while framesDecoded
// stays 0: a stream that stalls after a few bytes is a network problem, not a
// codec problem (AS10-03).

/** Grace window (ms) after the first byte before a stall can be flagged. */
const GRACE_MS = 2000;
/** Consecutive growing-bytes-zero-frames polls required before latching. */
const REQUIRED_STREAK = 2;

export interface DecodeFailureDetectorState {
  /** performance.now() at the first poll with bytesReceivedTotal > 0; null until then. */
  firstByteAtMs: number | null;
  /** Previous poll's cumulative bytesReceivedTotal, to detect growth. */
  prevBytesReceivedTotal: number | null;
  /** Consecutive polls where bytes grew AND framesDecodedTotal was still 0. */
  growingZeroFrameStreak: number;
  /** Sticky — once true, further steps are no-ops (mirrors SessionTelemetry.setDecodeFailed). */
  failed: boolean;
}

export function initialDecodeFailureDetectorState(): DecodeFailureDetectorState {
  return {
    firstByteAtMs: null,
    prevBytesReceivedTotal: null,
    growingZeroFrameStreak: 0,
    failed: false,
  };
}

export interface DecodeFailureSample {
  /** TelemetrySnapshot.framesDecodedTotal for this poll. */
  framesDecodedTotal: number;
  /** TelemetrySnapshot.bytesReceivedTotal for this poll. */
  bytesReceivedTotal: number;
  /** performance.now() at this poll. */
  nowMs: number;
}

export interface DecodeFailureStepResult {
  /** The next state — never mutates the input state. */
  state: DecodeFailureDetectorState;
  /** True only on the poll that flips failed from false to true (the caller's
   *  cue to call SessionTelemetry.setDecodeFailed(true) exactly once). */
  justFailed: boolean;
}

/** Advances the detector by one telemetry poll. Pure — no DOM, no timers. */
export function stepDecodeFailureDetector(
  state: DecodeFailureDetectorState,
  sample: DecodeFailureSample,
): DecodeFailureStepResult {
  if (state.failed) return { state, justFailed: false };

  const firstByteAtMs =
    state.firstByteAtMs == null && sample.bytesReceivedTotal > 0
      ? sample.nowMs
      : state.firstByteAtMs;

  const bytesGrew =
    state.prevBytesReceivedTotal != null && sample.bytesReceivedTotal > state.prevBytesReceivedTotal;

  // Reset when decoding starts, or bytes don't grow this window (a stall, not
  // a codec problem).
  const growingZeroFrameStreak =
    sample.framesDecodedTotal === 0 && bytesGrew ? state.growingZeroFrameStreak + 1 : 0;

  const justFailed =
    firstByteAtMs != null &&
    sample.nowMs - firstByteAtMs > GRACE_MS &&
    growingZeroFrameStreak >= REQUIRED_STREAK;

  return {
    state: {
      firstByteAtMs,
      prevBytesReceivedTotal: sample.bytesReceivedTotal,
      growingZeroFrameStreak,
      failed: justFailed,
    },
    justFailed,
  };
}
