// AS10-06 — stream-health classification → banner, pure so it unit-tests alone.
// The client never auto-acts (never auto-stops or relaunches); the user chooses.

import type { HealthState, Session } from "../../api/types";
import type { SignalQuality } from "../../components/Signal";
// σ bands come from the one golden file (docs/session-trace/thresholds.json via
// webrtc/thresholds.ts) so the glyph and the playout loop can never disagree.
import {
  SIGNAL_PACKETS_LOST_FAIR,
  SIGNAL_PACKETS_LOST_POOR,
  SIGNAL_SD_EXCELLENT_MS,
  SIGNAL_SD_FAIR_MS,
  SIGNAL_SD_POOR_MS,
} from "../../webrtc/thresholds";

export type HealthBannerKind = "warning" | "critical";

/** Connection-glyph inputs (UI-09), all already computed by SessionTelemetry. */
export interface SignalInputs {
  /** Present-interval σ (ms) from the #108 RVFC loop; null before the first window. */
  presentSdMs: number | null;
  /** Achieved receive fps (getStats framesPerSecond). */
  fps: number;
  /** Packets lost in the last 1 s poll window. */
  packetsLost: number;
  /** Freezes in the last 1 s poll window. */
  freezeCount: number;
  /** Distinguishes warm-up (fps 0, never delivered — `good`) from a dead media
   *  path (fps 0 after delivering — `poor`). Defaults false. */
  hasDeliveredFrames?: boolean;
  /** Input DataChannel open — the gate SessionTelemetry starts/stops on. With
   *  no path every counter is zero and the healthy default would lie (the audit
   *  saw "Good" for 118 s on a dead session), so the reading is `poor` with the
   *  label "No signal" ({@link qualityLabelFor}). Defaults true. */
  mediaPathUp?: boolean;
  /** #484 §3.2: false until the app commits its first presented frame — the
   *  compositor's empty scene decodes perfectly, so caps the result at `"good"`,
   *  never `"excellent"`, over a black screen. Defaults true. */
  appPresented?: boolean;
}

/**
 * Four-bar glyph: present σ (#108) dominates, loss/freezes knock a rung off.
 *  - poor      → freeze, heavy loss, σ ≥ 18 ms (AS-04 "degraded"), or a
 *                previously-flowing stream now at 0 fps.
 *  - fair      → some loss, or σ ≥ 12 ms (AS-04 "watch").
 *  - excellent → σ < 6 ms and clean.
 *  - good      → everything else (healthy default before σ / first frame).
 */
export function signalQuality({
  presentSdMs,
  fps,
  packetsLost,
  freezeCount,
  hasDeliveredFrames = false,
  mediaPathUp = true,
  appPresented = true,
}: SignalInputs): SignalQuality {
  // First: with no media path every counter below is zero and would fall
  // through to the healthy default.
  if (!mediaPathUp) return "poor";

  if (freezeCount > 0 || packetsLost >= SIGNAL_PACKETS_LOST_POOR) return "poor";
  if (presentSdMs != null && presentSdMs >= SIGNAL_SD_POOR_MS) return "poor";

  // 0 fps after delivering frames is a dead media path; never-delivered is
  // warm-up and stays on the "good" default.
  if (fps === 0 && hasDeliveredFrames) return "poor";

  if (packetsLost >= SIGNAL_PACKETS_LOST_FAIR) return "fair";
  if (presentSdMs != null && presentSdMs >= SIGNAL_SD_FAIR_MS) return "fair";

  if (presentSdMs != null && presentSdMs < SIGNAL_SD_EXCELLENT_MS && fps > 0 && packetsLost === 0) {
    // pre-present, "smooth and clean" measures the compositor's empty scene
    return appPresented ? "excellent" : "good";
  }
  return "good";
}

/** Human label for the connection glyph, matching the design dock's q-label. */
export function signalLabel(q: SignalQuality): string {
  return q.charAt(0).toUpperCase() + q.slice(1);
}

/** With no media path "Poor" would claim a measurement that does not exist;
 *  the label must read "No signal" instead. */
export function qualityLabelFor(q: SignalQuality, mediaPathUp: boolean): string {
  return mediaPathUp ? signalLabel(q) : "No signal";
}

export interface HealthBanner {
  kind: HealthBannerKind;
  title: string;
  message: string;
  /** Critical banners offer Stop + Retry actions; warning banners are info-only. */
  actionable: boolean;
}

/**
 * Which banner (if any) to show. host_lost is SessionPage's, ignored here.
 * #484 §3.2: `appPresented=false` suppresses every non-critical banner (a
 * network/device verdict over a black screen is unfalsifiable), but
 * `unsustainable` is checked first and never suppressed.
 */
export function healthBanner(
  session: Pick<Session, "state" | "state_detail" | "health_state" | "health_reason">,
  appPresented = true,
): HealthBanner | null {
  const failedUnsustainable =
    session.state === "failed" &&
    (session.state_detail?.startsWith("unsustainable") ?? false);

  if (session.health_state === "unsustainable" || failedUnsustainable) {
    return {
      kind: "critical",
      title: "Stream quality is unsustainable on your network",
      message:
        session.health_reason ??
        "Your network can't sustain this stream profile. Stop and relaunch at a lower profile, or retry.",
      actionable: true,
    };
  }

  // everything below is a non-blocking warning
  if (!appPresented) return null;

  // AS10-11: device bottlenecks, not network. The server only sets client_*
  // states from a visible tab's telemetry, so hidden tabs need no special case.
  const clientWarn: Partial<Record<HealthState, { title: string; message: string }>> = {
    client_decode_degrading: {
      title: "Your device is struggling to decode this stream",
      message:
        "Your device can't decode the video fast enough. Try a lower resolution for smoother playback.",
    },
    client_presentation_degrading: {
      title: "Your device can't keep up with the frame rate",
      message:
        "Playback is choppy on this device even though the network is fine. Try a lower resolution or frame rate.",
    },
  };

  if (session.health_state && clientWarn[session.health_state]) {
    const c = clientWarn[session.health_state]!;
    return {
      kind: "warning",
      title: c.title,
      message: session.health_reason ?? c.message,
      actionable: false,
    };
  }

  const warn: Partial<Record<HealthState, string>> = {
    network_degrading: "Your network is struggling — stream quality has been reduced.",
    abr_at_floor:
      "Stream quality is at the lowest level for this profile. Consider a lower profile if it stays choppy.",
  };

  if (session.health_state && warn[session.health_state]) {
    return {
      kind: "warning",
      title: "Reduced stream quality",
      message: session.health_reason ?? (warn[session.health_state] as string),
      actionable: false,
    };
  }

  return null;
}
