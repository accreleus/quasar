/**
 * Byte counts, in the shape the console has always used: Binary scaling
 * (1024 per step) with plain labels (KB, MB, GB, TB).
 *
 * That pairing is deliberate. Everything this renders arrives as the agent's
 * `*_mb` figures, which are MiB (`node-agent/src/capacity.rs`), and an
 * operator checks them against `df`, `free` and `nvidia-smi` — all of which
 * divide by 1024 and print "G". Writing "GiB" would be more correct and would
 * match nothing on the operator's screen. `lib/gpu.ts`'s `mbToGb` already made
 * this choice; this module matches it so no two numbers in the console
 * disagree about what a gigabyte is.
 */

const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"] as const;
const STEP = 1024;

/** How the number will actually be printed — one decimal below 100, none at or
 *  above it. The unit ladder walks on this value rather than the raw one, so a
 *  figure that rounds up to a full step is promoted instead of printed as
 *  "1024 MB". */
function displayValue(v: number): number {
  return v < 100 ? Math.round(v * 10) / 10 : Math.round(v);
}

/**
 * `bytes(12.4e9)` -> "11.5 GB", `bytes(1024 ** 2 * 512)` -> "512 MB",
 * `bytes(0)` -> "0 B".
 *
 * A non-finite input reads "unknown": a formatter that cannot tell how big
 * something is must say so rather than draw a confident zero.
 */
export function bytes(n: number): string {
  if (!Number.isFinite(n)) return "unknown";
  const sign = n < 0 ? "-" : "";
  let value = Math.abs(n);
  let unit = 0;
  while (unit < UNITS.length - 1 && displayValue(value) >= STEP) {
    value /= STEP;
    unit += 1;
  }
  const shown = displayValue(value);
  const text = shown < 100 ? trimZeroDecimal(shown.toFixed(1)) : String(shown);
  return `${sign}${text} ${UNITS[unit]}`;
}

/** The control plane reports storage and VRAM in `*_mb`, meaning MiB. One
 *  conversion, here, so no page has to remember that. */
export function bytesFromMb(mb: number): string {
  return bytes(mb * STEP * STEP);
}

function trimZeroDecimal(text: string): string {
  return text.endsWith(".0") ? text.slice(0, -2) : text;
}
