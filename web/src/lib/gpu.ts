/** Shared GPU display helpers (host-observability UI). */

/**
 * GPU `model` is a real marketing name (e.g. "NVIDIA GeForce RTX 5090") that
 * often repeats the vendor word shown alongside it in its own chip — strip a
 * leading vendor word so the primary label doesn't read "nvidia NVIDIA GeForce
 * RTX 5090".
 */
export function primaryGpuLabel(vendor: string, model: string): string {
  const trimmed = model.trim();
  const vendorWord = vendor.trim();
  if (vendorWord && trimmed.toLowerCase().startsWith(vendorWord.toLowerCase())) {
    return trimmed.slice(vendorWord.length).trim() || trimmed;
  }
  return trimmed;
}

export function mbToGb(mb: number): string {
  return (mb / 1024).toFixed(0);
}

// ── Live VRAM telemetry (#383) ──────────────────────────────────────────────
//
// Declared per-app VRAM was removed from admission; `GPUAvailability.vram_mb_reserved`
// (the old sum-of-declared-VRAM figure) now trends permanently to 0 for new sessions.
// The admin UI reads three additive, nullable fields instead: `vram_mb_used` /
// `vram_mb_free` / `vram_sampled_at`. Null means UNKNOWN (no sample yet, or an
// old agent) — never a fabricated zero. See
// docs/design/plans/2026-07-26-383-vram-admission-telemetry-spec.md §3.3/§4.1/§5.

/** Matches the control-plane's default `QUASAR_VRAM_STALENESS_SECS` (20s, §4.3). */
export const VRAM_STALE_MS = 20_000;

/** The subset of GPUAvailability the VRAM-telemetry helpers need. */
export interface VramTelemetrySource {
  vram_mb_used: number | null;
  vram_mb_total: number;
  vram_sampled_at: string | null;
}

export interface VramReading {
  /** False ⇒ render "—", never a 0%-filled bar (null/stale/implausible timestamp). */
  known: boolean;
  usedMb: number | null;
  totalMb: number;
  /** 0 when `!known` — callers must gate on `known`, not trust this as "0 used". */
  percent: number;
  /** Explains the unknown state; empty string when `known`. */
  hint: string;
}

/** Reads one GPU's live VRAM state, applying the freshness window. */
export function readVram(gpu: VramTelemetrySource, now: number = Date.now()): VramReading {
  const totalMb = gpu.vram_mb_total;
  if (gpu.vram_mb_used == null || gpu.vram_sampled_at == null) {
    return { known: false, usedMb: null, totalMb, percent: 0, hint: "No VRAM telemetry reported yet" };
  }
  const sampledAtMs = new Date(gpu.vram_sampled_at).getTime();
  const ageMs = now - sampledAtMs;
  if (!Number.isFinite(sampledAtMs) || ageMs > VRAM_STALE_MS) {
    const ageS = Number.isFinite(sampledAtMs) ? Math.max(0, Math.round(ageMs / 1000)) : null;
    return {
      known: false,
      usedMb: gpu.vram_mb_used,
      totalMb,
      percent: 0,
      hint: ageS != null ? `VRAM telemetry stale (last sample ${ageS}s ago)` : "VRAM telemetry stale",
    };
  }
  return {
    known: true,
    usedMb: gpu.vram_mb_used,
    totalMb,
    percent: totalMb === 0 ? 0 : Math.round((gpu.vram_mb_used / totalMb) * 100),
    hint: "",
  };
}

export interface VramAggregate {
  /** True when at least one GPU in the set contributed a fresh sample. */
  known: boolean;
  usedMb: number;
  totalMb: number;
  percent: number;
  /** GPUs excluded from the totals because their telemetry was unknown/stale. */
  excludedCount: number;
  gpuCount: number;
  /** Non-empty only when `excludedCount > 0` — surface this next to the bar. */
  hint: string;
}

/**
 * Sums live VRAM across a set of GPUs (one host, or the whole fleet), excluding
 * any GPU whose telemetry is unknown/stale from BOTH sides of the ratio — never
 * counting an unknown GPU's total as if 0 were used (spec §5: fleet aggregates
 * must exclude unknown GPUs, not treat them as zero).
 */
export function aggregateVram(gpus: VramTelemetrySource[], now: number = Date.now()): VramAggregate {
  let usedMb = 0;
  let totalMb = 0;
  let excludedCount = 0;
  for (const g of gpus) {
    const r = readVram(g, now);
    if (r.known) {
      usedMb += r.usedMb ?? 0;
      totalMb += r.totalMb;
    } else {
      excludedCount++;
    }
  }
  const known = gpus.length > 0 && excludedCount < gpus.length;
  return {
    known,
    usedMb,
    totalMb,
    percent: known && totalMb > 0 ? Math.round((usedMb / totalMb) * 100) : 0,
    excludedCount,
    gpuCount: gpus.length,
    hint:
      excludedCount > 0
        ? `${excludedCount} of ${gpus.length} GPU${gpus.length === 1 ? "" : "s"} excluded — no live telemetry`
        : "",
  };
}
