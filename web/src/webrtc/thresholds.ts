/**
 * TS copy of the golden threshold file `docs/session-trace/thresholds.json`;
 * `thresholds.test.ts` fails if they disagree. Go twin:
 * `control-plane/internal/session/verdict_thresholds.go`.
 */

/** Golden-file version; the Verdict reports the same string as `thresholds_version`. */
export const THRESHOLDS_VERSION = "2026-08-23.4";

// ── client-health classification (clientHealth.ts) ───────────────────────────

/** Present-interval σ (ms) at or above which the CLIENT is blamed, not the buffer. */
export const PRESENT_SD_DEGRADED_MS = 28;
/** Present-interval p95 (ms) — a sustained long-frame tail. */
export const PRESENT_P95_DEGRADED_MS = 45;
/** Freezes in a window: one is a blip, this many is a pattern. */
export const FREEZE_DEGRADED_COUNT = 2;
/** Fraction of the per-frame budget at which decode is already the limit. */
export const DECODE_BUDGET_FRACTION = 0.85;
/** Fallback decode ceiling (ms) when the profile frame budget is unknown. */
export const DECODE_ABS_CEILING_MS = 25;

// ── present cadence (presentCadence.ts) ──────────────────────────────────────
//
// Web-only: the control plane stores/serves the derived keys but doesn't
// recompute cadence, so the Go drift test doesn't assert these four.

/** Intervals needed in a window before any present_* summary is produced. */
export const PRESENT_MIN_SAMPLES = 5;
/** Half-width of the band around 2x the median that counts as a doubled frame. */
export const PRESENT_DOUBLED_BAND = 0.2;
/** Multiple of the median above which an interval is a stall, not the beat. */
export const PRESENT_LONG_FRAME_FACTOR = 2.5;
/** Largest doubled share still explainable as the source-fps == display-Hz beat. */
export const PRESENT_BEAT_DOUBLED_MAX = 0.25;

// ── receiver playout controller (playout.ts) ─────────────────────────────────

/** Default receiver playout target (ms) — the #108 measured smoothness knee. */
export const DEFAULT_PLAYOUT_MS = 100;
/** Smallest playout the controller descends to (ms) — the T2-measured knee. */
export const PLAYOUT_FLOOR_MS = 50;
/** Hard ceiling on the buffer (ms). */
export const PLAYOUT_CAP_MS = 150;
/** Conservative down-step per healthy 5 s window (ms). */
export const STEP_DOWN_MS = 10;
/** Multiplier applied to the buffer on a degraded window. */
export const STEP_UP_FACTOR = 1.5;
/** Default dwell after a step-up before descending again (ms). */
export const HOLD_MS = 30_000;
/** IL-1 shortened dwell once the path is confirmed locally healthy (ms). */
export const HOLD_FAST_MS = 10_000;
/** IL-1 accelerated down-step (ms). */
export const STEP_DOWN_FAST_MS = 15;
/** σ (ms) at or below which the controller may trim the buffer. */
export const HEALTHY_SD_MS = 12;
/** σ (ms) at or above which the controller re-inflates. */
export const DEGRADED_SD_MS = 18;
/** Consecutive healthy evaluations before fast descent is trusted. */
export const HEALTHY_STREAK_FOR_FAST = 3;
/** Controller evaluation cadence (ms). */
export const EVAL_INTERVAL_MS = 5_000;

// ── connection glyph (pages/app/streamHealth.ts) ─────────────────────────────
//
// SIGNAL_SD_POOR_MS/FAIR_MS deliberately mirror DEGRADED_SD_MS/HEALTHY_SD_MS
// (glyph and playout controller read the same present-interval σ). Go drift
// test asserts the pair.

/** σ (ms) at or above which the glyph reads `poor`. */
export const SIGNAL_SD_POOR_MS = 18;
/** σ (ms) at or above which the glyph reads `fair`. */
export const SIGNAL_SD_FAIR_MS = 12;
/** σ (ms) below which — and only when otherwise clean — the glyph reads `excellent`. */
export const SIGNAL_SD_EXCELLENT_MS = 6;
/** Packets lost in one poll window at or above which the glyph reads `poor`. */
export const SIGNAL_PACKETS_LOST_POOR = 15;
/** Packets lost in one poll window at or above which the glyph reads `fair`. */
export const SIGNAL_PACKETS_LOST_FAIR = 3;

// ── clock-offset reporting (telemetry.ts) ────────────────────────────────────
//
// Re-posts on this cadence when the estimate moves: a one-time latched post
// let clock drift silently invalidate aligned series for the rest of the
// session. `clockOffsetMs()` returns the same estimate it posts.

/** How often the client may re-post its clock-offset estimate while connected (s). */
export const CLOCK_REPOST_INTERVAL_S = 60;
/** Only re-post when the estimate moved more than this from the last posted value (ms). */
export const CLOCK_REPOST_DELTA_MS = 5;
