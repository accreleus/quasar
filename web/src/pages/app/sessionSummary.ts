export interface SessionSummary {
  appName: string;
  durationSeconds: number;
  fpsP50: number | null;
  fpsP95: number | null;
  latencyP50Ms: number | null;
  latencyP95Ms: number | null;
  endReason: string;
  recommendation: string;
}

/**
 * Nearest-rank percentile where `p` is a FRACTION (0.95), not a percentage.
 * `lib/stats`'s `percentile` takes a percentage over the same formula; the two
 * are not interchangeable and must not be folded together without converting
 * every call site.
 */
export function percentileFraction(values: readonly number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(p * sorted.length) - 1))];
}

export function recommendation(fpsP50: number | null, targetFps: number | null): string {
  if (fpsP50 != null && targetFps != null && fpsP50 < targetFps * 0.9) {
    return "Try the next lower frame-rate profile for steadier presentation.";
  }
  return "This profile was sustained; use it again for the next session.";
}
