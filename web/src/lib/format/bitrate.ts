/**
 * A stream's bitrate, always in Mb/s with the unit on the number.
 *
 * The console reads bitrate off `session_metrics` in kbps and used to print it
 * three ways on three screens ("8.0 Mb", a bare "8.0", "8.0 Mb/s"). One
 * function so a table cell, a KPI meta line and a detail fact cannot disagree.
 * Column headers stay the bare noun ("Bitrate"); the unit rides on the cell.
 */

/** `bitrate(8000)` -> "8.0 Mb/s". An absent or non-finite sample is the
 *  console's no-value glyph, never a confident zero. */
export function bitrate(kbps: number | null | undefined): string {
  if (kbps == null || !Number.isFinite(kbps)) return "—";
  return `${(kbps / 1000).toFixed(1)} Mb/s`;
}
