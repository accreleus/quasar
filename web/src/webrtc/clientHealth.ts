import {
  DECODE_ABS_CEILING_MS,
  DECODE_BUDGET_FRACTION,
  FREEZE_DEGRADED_COUNT,
  PRESENT_P95_DEGRADED_MS,
  PRESENT_SD_DEGRADED_MS,
} from "./thresholds";

// Client-side stream-health classifier. The browser is the source of truth for
// non-network client bottlenecks (document.hidden, RVFC pacing); control-plane
// consumes the class, does not re-derive it (single classifier, see coordinator.go).
//
// Invariants: client health NEVER drives server ABR — surfacing only (banner,
// admin diagnostics, profile eligibility history). `backgrounded_or_hidden` is
// checked FIRST: a hidden tab stops RVFC (present_fps collapses, freezes spike),
// and misreading that as decode/presentation failure is the #1 false-positive risk.
//
// Pure: no DOM/timers/I-O, guarded by clientHealth.test.ts.

/** The client-health classes the browser reports. */
export type ClientHealth =
  | "smooth"
  | "decode_degrading"
  | "presentation_degrading"
  | "backgrounded_or_hidden"
  | "client_unsupported";

export interface ClientHealthResult {
  health: ClientHealth;
  /** Short machine-ish reason string; surfaced in admin diagnostics. */
  reason: string;
}

/**
 * Inputs to {@link classifyClientHealth}, a single telemetry snapshot. Every
 * field here is READ — if a rule needs another field, add it with the branch
 * that uses it, in the same commit; don't carry unused "context-only" fields.
 */
export interface ClientHealthInputs {
  /** Mean decode time per frame this window (ms), or null if not yet measured. */
  decodeMs: number | null;
  /** σ of frame-to-frame presentation intervals (ms), or null. */
  presentSdMs: number | null;
  /** p95 of presentation intervals (ms), or null. */
  presentP95Ms: number | null;
  /** Freezes counted this window (getStats freezeCount delta). */
  freezeCount: number;
  /** document.hidden at sample time. */
  isHidden: boolean;
  /** 1000 / profile fps — the per-frame decode/present budget (ms). 0 = unknown. */
  profileFrameBudgetMs: number;
  /**
   * A hard decode failure / unsupported-codec signal the caller already knows
   * (e.g. the stream never produced a decodable frame, getVideoPlaybackQuality
   * totalVideoFrames stuck at 0). When true the client is classified
   * `client_unsupported` regardless of pacing — except a hidden tab still wins.
   */
  decodeFailed?: boolean;
}

// Thresholds (docs/session-trace/thresholds.json via ./thresholds): presentation σ
// is set well above AS-05's own 18ms "degraded" line so transient controller
// re-inflate churn doesn't flap this health banner. Decode budget fraction (0.85)
// leaves jitter headroom; unknown profile budget (0) falls back to an absolute
// ceiling (a 60fps frame is 16.7ms; 25ms decode is too slow for any interactive profile).

/** Classify a telemetry snapshot into a client-health class. Order is load-bearing:
 *  hidden beats all, then decode failure, then decode saturation, then
 *  presentation judder/freezes, else smooth. */
export function classifyClientHealth(inp: ClientHealthInputs): ClientHealthResult {
  // Hidden tab wins over everything: RVFC stops firing, so present_fps/σ are
  // meaningless and freezes are expected, never a failure.
  if (inp.isHidden) {
    return { health: "backgrounded_or_hidden", reason: "tab hidden or backgrounded" };
  }

  if (inp.decodeFailed) {
    return {
      health: "client_unsupported",
      reason: "stream did not decode on this device (unsupported codec/profile)",
    };
  }

  const budget =
    inp.profileFrameBudgetMs > 0
      ? inp.profileFrameBudgetMs * DECODE_BUDGET_FRACTION
      : DECODE_ABS_CEILING_MS;
  if (inp.decodeMs != null && inp.decodeMs >= budget) {
    return {
      health: "decode_degrading",
      reason: `decode ${inp.decodeMs.toFixed(1)}ms near/over frame budget ${budget.toFixed(1)}ms`,
    };
  }

  if (inp.freezeCount >= FREEZE_DEGRADED_COUNT) {
    return {
      health: "presentation_degrading",
      reason: `${inp.freezeCount} freezes in window`,
    };
  }
  if (inp.presentSdMs != null && inp.presentSdMs >= PRESENT_SD_DEGRADED_MS) {
    return {
      health: "presentation_degrading",
      reason: `present σ ${inp.presentSdMs.toFixed(1)}ms over ${PRESENT_SD_DEGRADED_MS}ms`,
    };
  }
  if (inp.presentP95Ms != null && inp.presentP95Ms >= PRESENT_P95_DEGRADED_MS) {
    return {
      health: "presentation_degrading",
      reason: `present p95 ${inp.presentP95Ms.toFixed(1)}ms over ${PRESENT_P95_DEGRADED_MS}ms`,
    };
  }

  return { health: "smooth", reason: "" };
}
