// Read by the launch path on the library page, which must not load the
// playout controller (playout.ts) to do it.

/**
 * Explicit `?playout=` override (ms, ≥0) or null. The controller's off-switch
 * (AS-05): with an override the {@link PlayoutController} must NOT run —
 * `?playout=` is the AS-04/T8 measurement instrument and stays absolute.
 */
let cachedOverride: number | null | undefined;
export function playoutOverride(search?: string): number | null {
  if (search === undefined) {
    if (cachedOverride === undefined) cachedOverride = parseOverride(location.search);
    return cachedOverride;
  }
  return parseOverride(search);
}

function parseOverride(search: string): number | null {
  const raw = new URLSearchParams(search).get("playout");
  if (raw == null) return null;
  const n = parseFloat(raw);
  if (!Number.isFinite(n) || n < 0) return null;
  return n;
}
