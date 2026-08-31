// Bench mode — per-stage decomposition of g2g_ms, measured in the page because
// the browser internals (jitter-buffer wait, Chrome's post-decode render queue)
// are reachable nowhere else; the agent-side probe closed the host half
// (docs/reports/2026-08-18-latency-probe/REPORT.md: 5.4 ms, 5.8% of budget;
// prior attribution gap: 2026-08-18-overnight-optimisation/t1-drops-latency.md §2).
//
// The decomposition is an identity, not a model. RVFC gives receiveTime /
// presentationTime / expectedDisplayTime on one clock, and present_ms is
// timeOrigin + expectedDisplayTime, so
//     g2g = host_to_receive + receive_to_present + present_to_display
// sums back with no residual by construction — `stagesReconcile()` asserts it.
//
// Stage notes that are not visible in the code:
//   - host_to_receive is the only cross-machine stage, hence the only one
//     needing the ST-05 clock offset.
//   - decode is RVFC processingDuration — the spec gives SECONDS; frameStages
//     takes it pre-converted so the unit mistake can only be made in one place.
//   - present_to_display in headless Chrome is a prediction on a virtual clock
//     (docs/testing-bench-mode.md), self-consistent but not what a user sees.
//   - render_queue ≈ wait_and_queue − jitter_buffer is derived, not measured,
//     and labelled as such in the emitted keys.

import type { BenchFrame } from "./aggregate";
import { percentile } from "./aggregate";

/**
 * The `inbound-rtp` fields this instrument reads, as cumulative totals straight
 * off getStats. Time-valued fields are SECONDS here (the WebRTC-stats unit) and
 * are converted exactly once, in `diffInbound`.
 *
 * Every field is optional: browsers ship these at different times, and a missing
 * counter must produce a null metric rather than a plausible wrong number.
 */
export interface InboundCounters {
  /** Cumulative seconds frames spent in the jitter buffer. */
  jitterBufferDelay?: number;
  /** Frames the jitter buffer has emitted — the divisor for the two delays. */
  jitterBufferEmittedCount?: number;
  /** Cumulative seconds of the buffer's own TARGET delay (what it was aiming for). */
  jitterBufferTargetDelay?: number;
  /** Cumulative seconds spent assembling frames that spanned several packets. */
  totalAssemblyTime?: number;
  framesAssembledFromMultiplePackets?: number;
  /** Cumulative seconds from first packet received to frame decoded. */
  totalProcessingDelay?: number;
  /** Cumulative seconds spent inside the decoder. */
  totalDecodeTime?: number;
  /** Cumulative seconds between consecutive decoded frames. */
  totalInterFrameDelay?: number;
  framesDecoded?: number;
  framesDropped?: number;
}

/** Per-window means derived from two `InboundCounters` reads. */
export interface InboundDelta {
  /** Mean jitter-buffer wait per emitted frame (ms). */
  jitterBufferMs: number | null;
  /** Mean jitter-buffer TARGET per emitted frame (ms) — what it was aiming for. */
  jitterBufferTargetMs: number | null;
  /** Mean assembly time per multi-packet frame (ms). */
  assemblyMs: number | null;
  /** Mean first-packet-to-decoded delay per decoded frame (ms). */
  processingMs: number | null;
  /** Mean decoder time per decoded frame (ms) — the getStats view of `decode`. */
  decodeMs: number | null;
  /** Mean interval between decoded frames (ms). */
  interFrameMs: number | null;
  /** Frames decoded in this window. */
  framesDecoded: number | null;
  /** Frames dropped in this window (presentation-side; see the #108 rule). */
  framesDropped: number | null;
}

/** Pull the video `inbound-rtp` entry out of a getStats report. */
export function parseInboundRtp(report: RTCStatsReport | null | undefined): InboundCounters | null {
  if (!report || typeof report.forEach !== "function") return null;
  let found: InboundCounters | null = null;
  report.forEach((raw: unknown) => {
    if (found) return;
    const s = raw as Record<string, unknown>;
    if (s?.["type"] !== "inbound-rtp") return;
    // `kind` is the modern field; `mediaType` is the legacy spelling. An audio
    // inbound-rtp carries several of the same key names, so guessing wrong here
    // would report the AUDIO jitter buffer as the video one — a plausible wrong
    // number, which is the failure mode this codebase treats as worse than none.
    const kind = (s["kind"] ?? s["mediaType"]) as string | undefined;
    if (kind !== "video") return;
    const num = (k: string): number | undefined => {
      const v = s[k];
      return typeof v === "number" && Number.isFinite(v) ? v : undefined;
    };
    found = {
      jitterBufferDelay: num("jitterBufferDelay"),
      jitterBufferEmittedCount: num("jitterBufferEmittedCount"),
      jitterBufferTargetDelay: num("jitterBufferTargetDelay"),
      totalAssemblyTime: num("totalAssemblyTime"),
      framesAssembledFromMultiplePackets: num("framesAssembledFromMultiplePackets"),
      totalProcessingDelay: num("totalProcessingDelay"),
      totalDecodeTime: num("totalDecodeTime"),
      totalInterFrameDelay: num("totalInterFrameDelay"),
      framesDecoded: num("framesDecoded"),
      framesDropped: num("framesDropped"),
    };
  });
  return found;
}

/** Mean of a cumulative-total / cumulative-count pair over one window, in ms. */
function meanMs(
  prevTotal: number | undefined, curTotal: number | undefined,
  prevCount: number | undefined, curCount: number | undefined,
): number | null {
  if (prevTotal === undefined || curTotal === undefined) return null;
  if (prevCount === undefined || curCount === undefined) return null;
  const frames = curCount - prevCount;
  // A window that emitted no frames has no mean. Returning 0 would read as
  // "zero jitter-buffer wait", which is the opposite of "we cannot say".
  if (!(frames > 0)) return null;
  const seconds = curTotal - prevTotal;
  if (!Number.isFinite(seconds)) return null;
  return (seconds / frames) * 1000;
}

function countDelta(prev: number | undefined, cur: number | undefined): number | null {
  if (prev === undefined || cur === undefined) return null;
  const d = cur - prev;
  return d >= 0 ? d : null;
}

/**
 * Window means, never lifetime means — a lifetime mean carries the start-up
 * transient forever (bench docs: a 60 s settle at 5.11% drags a 0.80% steady
 * state up to 1.71% for the whole hold).
 */
export function diffInbound(
  prev: InboundCounters | null | undefined,
  cur: InboundCounters | null | undefined,
): InboundDelta | null {
  if (!prev || !cur) return null;
  return {
    jitterBufferMs: meanMs(
      prev.jitterBufferDelay, cur.jitterBufferDelay,
      prev.jitterBufferEmittedCount, cur.jitterBufferEmittedCount),
    jitterBufferTargetMs: meanMs(
      prev.jitterBufferTargetDelay, cur.jitterBufferTargetDelay,
      prev.jitterBufferEmittedCount, cur.jitterBufferEmittedCount),
    assemblyMs: meanMs(
      prev.totalAssemblyTime, cur.totalAssemblyTime,
      prev.framesAssembledFromMultiplePackets, cur.framesAssembledFromMultiplePackets),
    processingMs: meanMs(
      prev.totalProcessingDelay, cur.totalProcessingDelay,
      prev.framesDecoded, cur.framesDecoded),
    decodeMs: meanMs(
      prev.totalDecodeTime, cur.totalDecodeTime,
      prev.framesDecoded, cur.framesDecoded),
    interFrameMs: meanMs(
      prev.totalInterFrameDelay, cur.totalInterFrameDelay,
      prev.framesDecoded, cur.framesDecoded),
    framesDecoded: countDelta(prev.framesDecoded, cur.framesDecoded),
    framesDropped: countDelta(prev.framesDropped, cur.framesDropped),
  };
}

/** One displayed frame's stage split. Null where the UA supplied no metadata. */
export interface FrameStages {
  /**
   * App submit → last packet of this frame received in the browser (ms).
   * The only cross-machine stage, so the only one carrying clock-offset error;
   * null when the frame did not decode (no `host_time_ms` to subtract).
   */
  hostToReceiveMs: number | null;
  /** Last packet received → UA submitted the frame for composition (ms). */
  receiveToPresentMs: number | null;
  /** Decoder submit → decoded frame ready (ms), from RVFC `processingDuration`. */
  decodeMs: number | null;
  /** `receiveToPresent − decode`: assembly + jitter buffer + render queue (ms). */
  waitAndQueueMs: number | null;
  /** UA composition submit → expected display (ms). Vsync/display quantisation. */
  presentToDisplayMs: number | null;
}

const NO_STAGES: FrameStages = {
  hostToReceiveMs: null,
  receiveToPresentMs: null,
  decodeMs: null,
  waitAndQueueMs: null,
  presentToDisplayMs: null,
};

function finite(v: number | undefined | null): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/**
 * Split one frame into stages. `timeOriginMs` lifts `receive_ms` into the
 * epoch domain of `host_time_ms`; the other stages are within-clock
 * differences, immune to both the time origin and the ST-05 offset.
 */
export function frameStages(frame: BenchFrame, timeOriginMs: number): FrameStages {
  const receive = finite(frame.receive_ms);
  const presentation = finite(frame.presentation_ms);
  const display = finite(frame.expected_display_ms);
  const decode = finite(frame.processing_ms);
  if (receive === null && presentation === null && display === null) return NO_STAGES;

  const hostTime = finite(frame.host_time_ms);
  const offset = frame.offset_ms;
  // Deliberately requires a measured offset. Substituting zero would turn a raw
  // difference between two machines' clocks into something that reads as a
  // latency — the same rule `g2g_ms` follows via `offset_unknown`.
  const hostToReceive =
    receive !== null && hostTime !== null && offset !== null
      ? timeOriginMs + receive + offset - hostTime
      : null;

  const receiveToPresent =
    receive !== null && presentation !== null ? presentation - receive : null;
  const presentToDisplay =
    presentation !== null && display !== null ? display - presentation : null;
  const waitAndQueue =
    receiveToPresent !== null && decode !== null ? receiveToPresent - decode : null;

  return {
    hostToReceiveMs: hostToReceive,
    receiveToPresentMs: receiveToPresent,
    decodeMs: decode,
    waitAndQueueMs: waitAndQueue,
    presentToDisplayMs: presentToDisplay,
  };
}

/**
 * Signed residual of g2g − Σ stages (ms), null when a term is missing. ~0 by
 * identity; non-zero means present_ms fell back to Date.now() (no
 * expectedDisplayTime) and that frame's stage table describes a different
 * quantity than its g2g. Exported so tests and dump() readers can check it.
 */
export function stagesReconcile(frame: BenchFrame, timeOriginMs: number): number | null {
  const g2g = finite(frame.g2g_ms);
  if (g2g === null) return null;
  const s = frameStages(frame, timeOriginMs);
  if (s.hostToReceiveMs === null || s.receiveToPresentMs === null || s.presentToDisplayMs === null) {
    return null;
  }
  return g2g - (s.hostToReceiveMs + s.receiveToPresentMs + s.presentToDisplayMs);
}

function pctKeys(
  out: Record<string, number | null>, name: string, values: number[],
): void {
  out[`stage_${name}_p50_ms`] = percentile(values, 50);
  out[`stage_${name}_p95_ms`] = percentile(values, 95);
}

/**
 * One window's frames + inbound delta → the flat `stage_*` keys on the
 * `bench.window` payload. Flat and numeric so bench_app_samples.py can pass
 * them through as `browser.stage_*` metrics; `bench.window` is
 * `additionalProperties: true` in openapi.yaml, so new keys touch no frozen
 * contract. Only `host_to_receive` needs a decoded marker; it reports its own `n`.
 */
export function summarizeStages(
  frames: BenchFrame[],
  timeOriginMs: number,
  inbound: InboundDelta | null,
): Record<string, number | null> {
  const hostToReceive: number[] = [];
  const receiveToPresent: number[] = [];
  const decode: number[] = [];
  const waitAndQueue: number[] = [];
  const presentToDisplay: number[] = [];
  const residuals: number[] = [];

  for (const f of frames) {
    const s = frameStages(f, timeOriginMs);
    if (s.hostToReceiveMs !== null) hostToReceive.push(s.hostToReceiveMs);
    if (s.receiveToPresentMs !== null) receiveToPresent.push(s.receiveToPresentMs);
    if (s.decodeMs !== null) decode.push(s.decodeMs);
    if (s.waitAndQueueMs !== null) waitAndQueue.push(s.waitAndQueueMs);
    if (s.presentToDisplayMs !== null) presentToDisplay.push(s.presentToDisplayMs);
    const r = stagesReconcile(f, timeOriginMs);
    if (r !== null) residuals.push(Math.abs(r));
  }

  const out: Record<string, number | null> = {};
  pctKeys(out, "host_to_receive", hostToReceive);
  pctKeys(out, "receive_to_present", receiveToPresent);
  pctKeys(out, "decode", decode);
  pctKeys(out, "wait_queue", waitAndQueue);
  pctKeys(out, "present_to_display", presentToDisplay);

  // Sample counts, so a reader can tell "0.0 ms" from "no frame carried it".
  // `receiveTime` in particular is optional metadata: a UA that never supplies
  // it leaves every receive-anchored stage null, and this is how that shows.
  out["stage_n"] = receiveToPresent.length;
  out["stage_host_to_receive_n"] = hostToReceive.length;
  out["stage_decode_n"] = decode.length;

  // Self-check, carried on the wire: p95 of |g2g − Σ stages|. Expected ~0.
  out["stage_reconcile_p95_ms"] = percentile(residuals, 95);

  // getStats-derived. Sampled by the controller immediately before the window
  // closes, so it describes the window that just ended — but it is a counter
  // DIFFERENCE over ~1 s, not a per-frame join, and is named `_mean_` to say so.
  out["stage_jb_mean_ms"] = inbound?.jitterBufferMs ?? null;
  out["stage_jb_target_mean_ms"] = inbound?.jitterBufferTargetMs ?? null;
  out["stage_assembly_mean_ms"] = inbound?.assemblyMs ?? null;
  out["stage_processing_mean_ms"] = inbound?.processingMs ?? null;
  out["stage_decode_stats_mean_ms"] = inbound?.decodeMs ?? null;
  out["stage_interframe_mean_ms"] = inbound?.interFrameMs ?? null;
  out["stage_frames_decoded"] = inbound?.framesDecoded ?? null;
  out["stage_frames_dropped"] = inbound?.framesDropped ?? null;

  // DERIVED, not measured: the part of `wait_queue` the jitter buffer does not
  // explain, i.e. Chrome's post-decode render queue. A NEGATIVE value is left
  // as-is rather than clamped — it means the per-frame path and the counter mean
  // disagree (different populations, or a window boundary straddling a rate
  // change), and hiding that behind a 0 would invent agreement.
  const waitP50 = out["stage_wait_queue_p50_ms"];
  const jb = out["stage_jb_mean_ms"];
  out["stage_render_queue_derived_p50_ms"] =
    waitP50 !== null && jb !== null ? waitP50 - jb : null;

  return out;
}
