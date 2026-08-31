/**
 * Elapsed time as a length, not as an instant (that is ./relativeTime.ts).
 *
 * Two shapes live here, deliberately:
 *   · `duration` / `durationBetween` — the console's everyday "how long has
 *     this been running" string ("8s", "47m", "1h 12m"). Minute-resolution
 *     past a minute, because a session's age to the second is noise.
 *   · `durationMs` — job runs, which are frequently sub-second and where the
 *     interesting digit is the tenth of a second.
 * They are not folded together: the second one renders a different fact at a
 * different resolution, and collapsing them would round a 500 ms run to "0s".
 */

/** Seconds -> "8s" / "47m" / "1h 12m". Negative or non-finite reads "0s".
 *  Each unit is rounded before the next one is chosen, so 59.6 seconds reads
 *  "1m" rather than the "60s" a round-after-choosing would print. */
export function duration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0s";
  const secs = Math.round(seconds);
  if (secs < 60) return `${secs}s`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m`;
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}

/** Two ISO instants -> `duration`. An open end means "until now" (a live
 *  session); no start at all means there is nothing to say, so: "". */
export function durationBetween(
  startedAt: string | null | undefined,
  endedAt: string | null | undefined,
  now: number = Date.now(),
): string {
  if (!startedAt) return "";
  const start = new Date(startedAt).getTime();
  const end = endedAt ? new Date(endedAt).getTime() : now;
  return duration((end - start) / 1000);
}

/** Milliseconds -> "500 ms" / "1.5 s" / "2m 30s". Null (not finished) -> "—". */
export function durationMs(ms: number | null): string {
  if (ms == null) return "—";
  if (ms < 1000) return `${ms} ms`;
  const secs = ms / 1000;
  if (secs < 60) return `${secs.toFixed(1)} s`;
  const mins = Math.floor(secs / 60);
  const rem = Math.round(secs % 60);
  return `${mins}m ${rem}s`;
}
