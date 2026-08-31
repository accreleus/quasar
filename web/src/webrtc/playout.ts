// Numbers live in the one golden threshold file (docs/session-trace/thresholds.json)
// via ./thresholds; they are not restated here.
import * as T from "./thresholds";
// true median: an even-length σ window averages the middle pair, not leans low
import { median } from "../lib/stats";

// Receiver playout-buffer control (#108): a near-zero jitter buffer micro-stutters
// at presentation (invisible to getStats loss/freeze). Single source of truth for
// the playout target — session.ts applies it to the receiver, telemetry.ts records
// it as `playout_target_ms` so stored σ correlates with its buffer.

/** Measured smoothness knee (#108); re-exported because callers import it here. */
export const DEFAULT_PLAYOUT_MS = T.DEFAULT_PLAYOUT_MS;

/**
 * `?playout=` override or the default; `?playout=0` disables buffering, negative or
 * unparseable falls back. The no-arg result is cached (called every stats tick, the
 * URL never changes mid-session); an explicit `search` bypasses the cache.
 */
let cachedDefaultPlayout: number | undefined;
export function resolvePlayoutMs(search?: string): number {
  if (search === undefined) {
    cachedDefaultPlayout ??= parsePlayout(location.search);
    return cachedDefaultPlayout;
  }
  return parsePlayout(search);
}

function parsePlayout(search: string): number {
  const raw = new URLSearchParams(search).get("playout");
  if (raw == null) return DEFAULT_PLAYOUT_MS;
  const n = parseFloat(raw);
  if (!Number.isFinite(n) || n < 0) return DEFAULT_PLAYOUT_MS;
  return n;
}

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

/** Starting target: `?playout=` override, else the tier's `playout0_ms` (AS-02),
 *  else the default. */
export function resolveInitialPlayoutMs(playout0Ms?: number, search?: string): number {
  const ov = playoutOverride(search);
  if (ov != null) return ov;
  if (playout0Ms != null && Number.isFinite(playout0Ms) && playout0Ms >= 0) return playout0Ms;
  return DEFAULT_PLAYOUT_MS;
}

/**
 * Sets both knobs: `jitterBufferTarget` (ms) and `playoutDelayHint` (seconds).
 * Neither is in the TS DOM types and either may be absent, so each set is guarded
 * best-effort — a failure to tune never breaks the session.
 */
export function applyPlayout(receiver: RTCRtpReceiver, ms: number): void {
  const r = receiver as RTCRtpReceiver & {
    playoutDelayHint?: number;
    jitterBufferTarget?: number | null;
  };
  try {
    r.playoutDelayHint = ms / 1000; // seconds
  } catch {
    /* unsupported — best effort */
  }
  try {
    if ("jitterBufferTarget" in r) r.jitterBufferTarget = ms; // ms
  } catch {
    /* unsupported — best effort */
  }
}

// ── AS-05: adaptive playout controller ───────────────────────────────────────
//
// Starts at the tier's playout₀, trims headroom while smooth, re-inflates the
// moment presentation degrades. Threshold derivations: the AS-04 sweep
// (docs/completed/adaptive-streaming/AS-04-playout-sweep.md) and the T2 knee
// report (docs/reports/2026-08-18-overnight-optimisation/t2-playout-knee.md,
// which moved the floor 30→50 ms); values live in
// docs/session-trace/thresholds.json. The degradation arm is what earns its keep
// — AS-04 showed static playout₀ alone does not rescue σ under loss.
//
// Independent of the AS-03 ABR governor: that moves bitrate (server, GCC), this
// moves the receiver buffer (client, present σ); they share no state, and the
// asymmetric step + ≥30 s hold keeps this loop slower than ABR's retarget
// cadence so it never reacts to an ABR step's σ wiggle. 5 s windows.

/** T2-measured knee — smallest playout the controller descends to (ms). */
export const PLAYOUT_FLOOR_MS = T.PLAYOUT_FLOOR_MS;
const STEP_DOWN_MS = T.STEP_DOWN_MS;
const STEP_UP_FACTOR = T.STEP_UP_FACTOR;
const PLAYOUT_CAP_MS = T.PLAYOUT_CAP_MS;
/** Dwell after a step-up before descending again (ms). */
const HOLD_MS = T.HOLD_MS;
/** σ ≤ this: smooth enough to trim (ms). */
const HEALTHY_SD_MS = T.HEALTHY_SD_MS;
/** σ ≥ this, or any freeze: re-inflate (ms). */
const DEGRADED_SD_MS = T.DEGRADED_SD_MS;
const EVAL_INTERVAL_MS = T.EVAL_INTERVAL_MS;

// ── IL-1: verdict-aware fast descent on a locally-healthy path ────────────────
//
// Derivation: docs/research/input-latency-analysis.md (IL-0: on a clean LAN felt
// input latency is entirely the playout buffer; `jitterBufferTarget` is advisory,
// Chrome undercuts it on a clean path, so descent is cheap insurance). After
// HEALTHY_STREAK_FOR_FAST healthy evaluations the hold shortens and the down-step
// grows; a single degraded window steps up ×STEP_UP_FACTOR, re-arms the full
// hold, and resets the streak — the fast-up / gated-down asymmetry that earns
// the controller its keep under real loss is preserved.

/** Healthy evaluations (no freeze, σ ≤ HEALTHY_SD_MS) before fast descent —
 *  long enough to ride out an ABR retarget's σ wiggle. */
const HEALTHY_STREAK_FOR_FAST = T.HEALTHY_STREAK_FOR_FAST;
const HOLD_FAST_MS = T.HOLD_FAST_MS;
const STEP_DOWN_FAST_MS = T.STEP_DOWN_FAST_MS;

export interface PlayoutHealthSample {
  /** Present-interval σ for the window (ms), or null if too few frames. */
  sdMs: number | null;
  /** getStats freezeCount delta for the window. */
  freezeCount: number;
}

/** One instance per session, created only when there is no `?playout=` override.
 *  Feed it telemetry's RVFC σ/freeze samples; it re-targets on its own 5 s timer. */
export class PlayoutController {
  private readonly receiver: RTCRtpReceiver;
  private readonly floor: number;
  private readonly onChange?: (ms: number) => void;
  private current: number;
  private readonly sdWindow: number[] = [];
  private freezesInWindow = 0;
  private holdUntil = 0; // performance.now() ms; no descent before this
  private healthyStreak = 0; // consecutive healthy evaluations; gates IL-1 fast descent
  private timer: ReturnType<typeof setInterval> | null = null;

  constructor(opts: {
    receiver: RTCRtpReceiver;
    playout0Ms: number;
    floorMs?: number;
    onChange?: (ms: number) => void;
  }) {
    this.receiver = opts.receiver;
    // floor never exceeds the start
    this.floor = Math.min(opts.floorMs ?? PLAYOUT_FLOOR_MS, opts.playout0Ms);
    this.onChange = opts.onChange;
    this.current = opts.playout0Ms;
  }

  /** Recorded by telemetry as playout_target_ms. */
  currentMs(): number {
    return this.current;
  }

  /** Apply playout₀ and begin adapting. Idempotent. */
  start(): void {
    if (this.timer !== null) return;
    applyPlayout(this.receiver, this.current);
    this.onChange?.(this.current);
    this.timer = setInterval(() => this.evaluate(), EVAL_INTERVAL_MS);
  }

  /** Stop adapting. Safe to call multiple times. */
  stop(): void {
    if (this.timer !== null) clearInterval(this.timer);
    this.timer = null;
  }

  /** Feed one telemetry tick (≈1 Hz). Buffered; consumed every 5 s by {@link evaluate}. */
  sample(s: PlayoutHealthSample): void {
    if (s.sdMs != null && Number.isFinite(s.sdMs)) this.sdWindow.push(s.sdMs);
    if (s.freezeCount > 0) this.freezesInWindow += s.freezeCount;
  }

  private evaluate(): void {
    const froze = this.freezesInWindow > 0;
    const sd = median(this.sdWindow);
    this.sdWindow.length = 0;
    this.freezesInWindow = 0;

    // No frames this window (e.g. a hidden tab — RVFC stops): hold, don't guess.
    if (sd == null && !froze) return;

    const now = performance.now();
    const degraded = froze || (sd != null && sd >= DEGRADED_SD_MS);
    const healthy = !froze && sd != null && sd <= HEALTHY_SD_MS;

    if (degraded) {
      // Re-inflate in one step, reset the streak, re-arm the full hold — the
      // fast-up / gated-down asymmetry must survive every acceleration.
      this.healthyStreak = 0;
      this.holdUntil = now + HOLD_MS;
      const next = Math.min(PLAYOUT_CAP_MS, Math.round(this.current * STEP_UP_FACTOR));
      if (next !== this.current) this.set(next, froze ? "freeze" : `σ=${sd!.toFixed(1)}`);
    } else if (healthy) {
      this.healthyStreak++;
      const fast = this.healthyStreak >= HEALTHY_STREAK_FOR_FAST;
      const stepMs = fast ? STEP_DOWN_FAST_MS : STEP_DOWN_MS;
      // Once fast, retroactively re-base a hold armed under the conservative
      // regime (set at upStep + HOLD_MS) to upStep + HOLD_FAST_MS.
      if (fast) {
        const shortened = this.holdUntil - HOLD_MS + HOLD_FAST_MS;
        if (shortened < this.holdUntil) this.holdUntil = shortened;
      }
      if (now >= this.holdUntil && this.current > this.floor) {
        const next = Math.max(this.floor, this.current - stepMs);
        this.set(next, `σ=${sd!.toFixed(1)}`);
      }
    } else {
      // Hysteresis band: no change, and the streak pauses (neither advances nor
      // resets) so a wobbly path doesn't accrue toward fast descent.
    }
  }

  private set(ms: number, why: string): void {
    const from = this.current;
    this.current = ms;
    applyPlayout(this.receiver, ms);
    this.onChange?.(ms);
    // transition trail for the AS-05 evidence
    console.info(`[playout] ${from} → ${ms} ms (${ms > from ? "up" : "down"}, ${why})`);
  }
}
