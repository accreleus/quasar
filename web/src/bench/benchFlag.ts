// Bench mode is off unless explicitly asked for — per-frame pixel readback
// costs real CPU and a GPU sync, so nothing may run for an ordinary session;
// checked once before the controller is constructed. Enabled by `?bench=1` or
// localStorage["quasar.bench"]="1"; `?bench=0` beats the sticky flag.

/** localStorage key for the sticky bench flag. */
export const BENCH_STORAGE_KEY = "quasar.bench";

function truthy(raw: string | null): boolean | null {
  if (raw == null) return null;
  if (raw === "1" || raw === "true") return true;
  if (raw === "0" || raw === "false") return false;
  return null;
}

/**
 * Is bench mode on for this page?
 *
 * `search` and `stored` are injectable so this is testable without touching
 * `location` or `localStorage`; production callers pass nothing.
 */
export function isBenchModeEnabled(search?: string, stored?: string | null): boolean {
  const query = search ?? (typeof location !== "undefined" ? location.search : "");
  const fromUrl = truthy(new URLSearchParams(query).get("bench"));
  if (fromUrl !== null) return fromUrl;

  let raw: string | null;
  if (stored !== undefined) {
    raw = stored;
  } else {
    try {
      raw = typeof localStorage !== "undefined" ? localStorage.getItem(BENCH_STORAGE_KEY) : null;
    } catch {
      raw = null; // storage can throw in a partitioned/blocked context
    }
  }
  return truthy(raw) === true;
}
