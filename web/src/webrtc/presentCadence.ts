/**
 * Reports the DISTRIBUTION of presentation intervals, not one scalar. When source
 * fps equals display Hz the two clocks beat: an occasional missed vsync doubles
 * one interval, dragging `1000/mean(intervals)` down hard (observed: 120fps
 * session, encode 7.5ms, present σ 1.9ms, "fps (shown)" read 88-108) while the
 * median never moves. Median alone can't distinguish "beat" from "stall" either
 * (both drag mean to ~107), hence also tracking doubledFraction/longFrames/
 * inherentBeat. Pure/dependency-free; unit test is the whole contract.
 */

import { mean as meanOf, median as medianOf, percentile, stddev } from "../lib/stats";
import {
  PRESENT_BEAT_DOUBLED_MAX,
  PRESENT_DOUBLED_BAND,
  PRESENT_LONG_FRAME_FACTOR,
  PRESENT_MIN_SAMPLES,
} from "./thresholds";

/** A window of RVFC frame-to-frame presentation intervals, summarised. */
export interface PresentCadence {
  /** Intervals in the window. Reported even when it is too small to summarise. */
  n: number;
  /** Median interval (ms) — the estimator that survives a doubled frame. */
  medianMs: number | null;
  /** Mean interval (ms) — kept because it is what `present_fps` has always been. */
  meanMs: number | null;
  p95Ms: number | null;
  maxMs: number | null;
  /** Population σ of the intervals (ms) — the #108 headline judder metric. */
  sdMs: number | null;
  /** 1000 / medianMs — the fps a viewer actually perceives. */
  fpsFromMedian: number | null;
  /** 1000 / meanMs — the legacy `present_fps`, kept for continuity. */
  fpsFromMean: number | null;
  /** Share of intervals within ±PRESENT_DOUBLED_BAND of exactly 2× the median. */
  doubledFraction: number | null;
  /** Intervals longer than PRESENT_LONG_FRAME_FACTOR × median — real stalls. */
  longFrames: number | null;
  /** medianMs − 1000/displayHz. Null when the display Hz is unknown. */
  driftMs: number | null;
  /**
   * True only when source fps and display Hz agree (within 1), doubled share is
   * inside PRESENT_BEAT_DOUBLED_MAX, and no interval is a stall. False whenever
   * unknown (display Hz/source fps/samples) — never defaults true on missing data.
   */
  inherentBeat: boolean;
}

const EMPTY: Omit<PresentCadence, "n"> = {
  medianMs: null,
  meanMs: null,
  p95Ms: null,
  maxMs: null,
  sdMs: null,
  fpsFromMedian: null,
  fpsFromMean: null,
  doubledFraction: null,
  longFrames: null,
  driftMs: null,
  inherentBeat: false,
};

/**
 * Below PRESENT_MIN_SAMPLES every summary is null (must not produce a confident
 * number from a barely-filled window); `n` still reports the truth.
 */
export function summarizePresentWindow(
  intervalsMs: readonly number[],
  displayHz: number | null,
  sourceFps: number | null,
): PresentCadence {
  const n = intervalsMs.length;
  if (n < PRESENT_MIN_SAMPLES) return { n, ...EMPTY };

  const medianMs = medianOf(intervalsMs);
  const meanMs = meanOf(intervalsMs);
  if (medianMs == null || meanMs == null || medianMs <= 0) return { n, ...EMPTY };

  let maxMs = intervalsMs[0]!;
  let doubled = 0;
  let longFrames = 0;
  const doubledTarget = 2 * medianMs;
  const doubledLo = doubledTarget * (1 - PRESENT_DOUBLED_BAND);
  const doubledHi = doubledTarget * (1 + PRESENT_DOUBLED_BAND);
  const longCut = PRESENT_LONG_FRAME_FACTOR * medianMs;
  for (const iv of intervalsMs) {
    if (iv > maxMs) maxMs = iv;
    if (iv >= doubledLo && iv <= doubledHi) doubled++;
    if (iv > longCut) longFrames++;
  }
  const doubledFraction = doubled / n;

  const driftMs =
    displayHz != null && displayHz > 0 ? medianMs - 1000 / displayHz : null;

  // ±1 absorbs a 59.94-vs-60 mismatch without admitting 60-on-120.
  const ratesMatch =
    sourceFps != null && displayHz != null && Math.abs(sourceFps - displayHz) <= 1;
  const inherentBeat =
    ratesMatch && doubledFraction <= PRESENT_BEAT_DOUBLED_MAX && longFrames === 0;

  return {
    n,
    medianMs,
    meanMs,
    p95Ms: percentile(intervalsMs, 95),
    maxMs,
    sdMs: stddev(intervalsMs),
    fpsFromMedian: 1000 / medianMs,
    fpsFromMean: meanMs > 0 ? 1000 / meanMs : null,
    doubledFraction,
    longFrames,
    driftMs,
    inherentBeat,
  };
}
