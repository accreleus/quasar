import { describe, expect, it } from "vitest";
import {
  initialDecodeFailureDetectorState,
  stepDecodeFailureDetector,
  type DecodeFailureDetectorState,
} from "./decodeFailureDetector";

/** Runs a sequence of polls through the detector, returning the final state
 * and whether any step in the sequence flagged justFailed. */
function run(
  steps: Array<{ framesDecodedTotal: number; bytesReceivedTotal: number; nowMs: number }>,
): { state: DecodeFailureDetectorState; anyFailed: boolean } {
  let state = initialDecodeFailureDetectorState();
  let anyFailed = false;
  for (const sample of steps) {
    const r = stepDecodeFailureDetector(state, sample);
    state = r.state;
    anyFailed = anyFailed || r.justFailed;
  }
  return { state, anyFailed };
}

describe("stepDecodeFailureDetector", () => {
  it("never flags before any bytes have arrived", () => {
    const { state, anyFailed } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 0, nowMs: 0 },
      { framesDecodedTotal: 0, bytesReceivedTotal: 0, nowMs: 5000 },
    ]);
    expect(anyFailed).toBe(false);
    expect(state.failed).toBe(false);
  });

  // Regression for defect (1): the grace window is anchored on FIRST BYTE
  // RECEIVED, not on track arrival. A slow-connecting-but-healthy session
  // whose first RTP arrives long after the track was added must not
  // immediately latch just because the wall clock already looks "late".
  it("does not flag on the very first poll bytes appear, even if that arrives late", () => {
    const { anyFailed, state } = run([
      // First bytes ever observed arrive at nowMs=5000 (e.g. track added at
      // t=0, first RTP didn't land until t=5000) — the OLD track-arrival-
      // anchored detector would have latched immediately here.
      { framesDecodedTotal: 0, bytesReceivedTotal: 5000, nowMs: 5000 },
    ]);
    expect(anyFailed).toBe(false);
    expect(state.failed).toBe(false);
  });

  // Slow-start-then-decode: bytes grow for a bit with zero frames decoded
  // (normal keyframe/jitter-buffer warmup), then decoding begins before the
  // streak or grace window is satisfied. Must never flag.
  it("slow-start-then-decode: growing bytes with zero frames briefly, then decode starts — no false positive", () => {
    const { anyFailed, state } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 1_000, nowMs: 0 }, // first byte
      { framesDecodedTotal: 0, bytesReceivedTotal: 3_000, nowMs: 1_000 }, // growing, streak=1
      { framesDecodedTotal: 5, bytesReceivedTotal: 6_000, nowMs: 2_000 }, // decode starts
      { framesDecodedTotal: 30, bytesReceivedTotal: 9_000, nowMs: 3_000 },
    ]);
    expect(anyFailed).toBe(false);
    expect(state.failed).toBe(false);
  });

  // Regression for defect (2): a stream that receives a few bytes then stalls
  // is a network problem, not a codec problem. Bytes must GROW across
  // consecutive polls (not merely have been received at some point) before
  // decodeFailed can latch.
  it("bytes-stalled: growth interrupted before two consecutive growing polls — no decodeFailed latch", () => {
    const { anyFailed, state } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 1_000, nowMs: 0 }, // first byte
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 1_000 }, // grew, streak=1
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 2_000 }, // STALL — streak resets to 0
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 3_000 }, // still stalled
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 10_000 }, // well past the grace window
    ]);
    expect(anyFailed).toBe(false);
    expect(state.failed).toBe(false);
  });

  it("a stall that later resumes growing needs a fresh 2-poll streak, not a resumed one", () => {
    const { anyFailed, state } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 1_000, nowMs: 0 },
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 1_000 }, // streak=1
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 2_000 }, // stall -> streak=0
      { framesDecodedTotal: 0, bytesReceivedTotal: 3_000, nowMs: 3_000 }, // resumes -> streak=1 (not 2)
    ]);
    expect(anyFailed).toBe(false);
    expect(state.failed).toBe(false);
  });

  // Positive case: the detector must still catch a genuine decode failure —
  // bytes keep growing every poll while framesDecoded never leaves 0.
  it("flags a genuine decode failure once bytes have grown for 2 consecutive polls past the grace window", () => {
    const { anyFailed, state } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 1_000, nowMs: 0 }, // first byte
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 1_000 }, // streak=1
      { framesDecodedTotal: 0, bytesReceivedTotal: 3_000, nowMs: 2_500 }, // streak=2, >2s since first byte
    ]);
    expect(anyFailed).toBe(true);
    expect(state.failed).toBe(true);
  });

  it("is sticky: once failed, later steps never re-flag or reset (and return the same state)", () => {
    const { state: failedState } = run([
      { framesDecodedTotal: 0, bytesReceivedTotal: 1_000, nowMs: 0 },
      { framesDecodedTotal: 0, bytesReceivedTotal: 2_000, nowMs: 1_000 },
      { framesDecodedTotal: 0, bytesReceivedTotal: 3_000, nowMs: 2_500 },
    ]);
    expect(failedState.failed).toBe(true);

    const next = stepDecodeFailureDetector(failedState, {
      framesDecodedTotal: 100,
      bytesReceivedTotal: 100,
      nowMs: 999_999,
    });
    expect(next.justFailed).toBe(false);
    expect(next.state).toBe(failedState);
  });
});
