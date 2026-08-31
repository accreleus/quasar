/**
 * What happens on every telemetry tick, as one addressable/testable unit — the
 * effects interface is the whole output, so a test pushes snapshots and asserts
 * call order.
 *
 * Order within a tick is load-bearing: fan out first (every tick must reach the
 * isolated consumers — #139), then summary, then playout, then media-stall, then
 * freeze tracing, then decode-failure, then client_unsupported last.
 */

import {
  initialDecodeFailureDetectorState,
  stepDecodeFailureDetector,
  type DecodeFailureDetectorState,
} from "./decodeFailureDetector";
import type { TelemetrySnapshot } from "../../webrtc/telemetry";

export interface TelemetrySinkEffects {
  /** Fan out to the isolated consumers: strip, drawer gamepad readout,
   *  DiagPanel, drawer-gated quality subscriber. */
  fanOut(snap: TelemetrySnapshot): void;
  playoutSample(sample: { sdMs: number | null; freezeCount: number }): void;
  /** Three consecutive zero-receive windows start recovery while the host path
   *  is still restartable — ICE consent failure can lag a full RTP outage by
   *  tens of seconds. */
  recoverMediaPath(): void;
  mediaPathFlowing(): void;
  /** ST-04 client.freeze_detected. `gapMs` must be the window's longest
   *  MEASURED presentation interval (or null) — never `1000 / freezeCount`,
   *  which fabricates a duration from the counter, not a clock. */
  emitFreezeDetected(gapMs: number | null, isHidden: boolean): void;
  /** At most once per telemetry instance. */
  setDecodeFailed(): void;
  /** Sticky at the page level: a hard decode failure never self-heals. */
  onClientUnsupported(): void;
  onDisplayRefreshHz(hz: number | null): void;
  /** Injected so a test needn't fake performance.now globally. */
  now(): number;
  /** Hidden-tab guard (spec §3.3): a hidden tab's freeze is expected, never
   *  blamed on the stream. */
  isHidden(): boolean;
}

export interface SessionSummaryAccumulator {
  /** Elapsed on the sink's own clock (`fx.now()` deltas only). A wall-clock
   *  start is deliberately not exposed: `fx.now()` is `performance.now()` in
   *  production, and `Date.now() - startedAt` across the two clocks reports the
   *  Unix epoch as the session duration. */
  elapsedMs: number;
  fps: readonly number[];
  latency: readonly number[];
}

export interface TelemetrySink {
  push(snap: TelemetrySnapshot): void;
  /** Percentiles are computed by the caller from these, in sessionSummary.ts. */
  summary(): SessionSummaryAccumulator;
}

export function createTelemetrySink(fx: TelemetrySinkEffects): TelemetrySink {
  const startedAt = fx.now();
  const fps: number[] = [];
  const latency: number[] = [];
  let prevFreezeCount = 0;
  let mediaStallWindows = 0;
  let decodeState: DecodeFailureDetectorState = initialDecodeFailureDetectorState();

  return {
    summary: () => ({ elapsedMs: fx.now() - startedAt, fps, latency }),

    push(snap) {
      fx.fanOut(snap);

      if (snap.fps > 0) fps.push(snap.fps);
      const sample = snap.g2gMs ?? snap.rttMs;
      if (sample != null && sample >= 0) latency.push(sample);

      fx.onDisplayRefreshHz(snap.displayRefreshHz);

      fx.playoutSample({ sdMs: snap.presentSdMs, freezeCount: snap.freezeCount });

      // `=== 3` fires recovery exactly once per stall; `>= 3` on the way out
      // reports flowing-again only for a stall that already triggered it.
      if (snap.bitrateKbps === 0 && snap.fps === 0) {
        mediaStallWindows += 1;
        if (mediaStallWindows === 3) fx.recoverMediaPath();
      } else if ((snap.bitrateKbps ?? 0) > 0 || snap.fps > 0) {
        if (mediaStallWindows >= 3) fx.mediaPathFlowing();
        mediaStallWindows = 0;
      }

      if (snap.freezeCount > 0 && snap.freezeCount !== prevFreezeCount) {
        fx.emitFreezeDetected(snap.presentCadence.maxMs, fx.isHidden());
      }
      prevFreezeCount = snap.freezeCount;

      // Multi-codec spec §6.1: latches on the poll that flips it, so
      // classifyClientHealth reports client_unsupported one tick later.
      const decodeStep = stepDecodeFailureDetector(decodeState, {
        framesDecodedTotal: snap.framesDecodedTotal,
        bytesReceivedTotal: snap.bytesReceivedTotal,
        nowMs: fx.now(),
      });
      decodeState = decodeStep.state;
      if (decodeStep.justFailed) fx.setDecodeFailed();

      if (snap.clientHealth === "client_unsupported") fx.onClientUnsupported();
    },
  };
}
