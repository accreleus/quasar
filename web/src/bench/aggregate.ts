import { percentile } from "../lib/stats";

// Bench-mode aggregation — pure, dependency-free (testable without canvas/
// video/WebRTC): one BenchFrame per displayed frame → the per-second
// `bench.window` payload.

/** Marker event_flags bit 0 — the app's input-echo pulse is active. */
export const FLAG_INPUT_ECHO = 0x01;
/** Marker event_flags bit 1 — scene changed on this frame. */
export const FLAG_SCENE_CHANGE = 0x02;
/** Marker event_flags bit 2 — render/surface resolution changed on this frame. */
export const FLAG_RESOLUTION_CHANGE = 0x04;
/** Marker event_flags bit 3 — a labelled mark pulse is active. */
export const FLAG_MARK = 0x08;

/** One displayed frame, after marker decode. Also the ring-buffer record. */
export interface BenchFrame {
  /** Browser present time, unix epoch ms (RVFC expectedDisplayTime when available). */
  present_ms: number;
  /** True when the marker decoded with a valid CRC. */
  decoded: boolean;
  /** Decoder confidence 0..1 (0 when not decoded). */
  confidence: number;
  frame_index?: number;
  /** App-side CLOCK_REALTIME unix ms baked into the frame's pixels. */
  host_time_ms?: number;
  scene_id?: number;
  load_level?: number;
  event_flags?: number;
  render_w?: number;
  render_h?: number;
  /** present_ms + clock offset − host_time_ms (ms). Undefined when not decoded. */
  g2g_ms?: number;
  /** The clock offset used for g2g_ms; null when unmeasured (g2g is then raw). */
  offset_ms: number | null;

  // RVFC stage metadata (stages.ts), raw on the high-resolution clock so stage
  // differences carry no timeOrigin/offset error; frameStages lifts in one
  // place. All optional: a UA that omits a field must yield null stages,
  // never a wrong number.
  /** `receiveTime` — the frame's last packet arrived (ms, high-res clock). */
  receive_ms?: number;
  /** `presentationTime` — the UA submitted the frame for composition (ms). */
  presentation_ms?: number;
  /** `expectedDisplayTime` — when the UA expects it visible (ms). */
  expected_display_ms?: number;
  /** `processingDuration` CONVERTED TO MS (the spec gives it in seconds). */
  processing_ms?: number;
  /** `rtpTimestamp` — the frame's RTP timestamp, for correlating with the wire. */
  rtp_timestamp?: number;
  /** `presentedFrames` — the UA's own count of frames submitted for composition. */
  presented_frames?: number;
}

/** `missing` is not called "dropped" — see classifyIndexDelta. */
export interface IndexDeltaCounts {
  missing: number;
  duplicated: number;
  reordered: number;
}

/**
 * Classify one frame-index step (prev = previous decoded index; null = first
 * frame, counts as nothing). delta 1 → ok, 0 → duplicated, >1 → delta−1
 * missing, <0 → reordered.
 *
 * The gap count is `missing`, never "dropped": the loss could be anywhere from
 * app to presentation, and "dropped" invites booking it as pipeline loss.
 * Attribution needs the app's own presented range — bench_app_samples.py
 * carries `frame_index_min/max` for the downstream difference.
 */
export function classifyIndexDelta(
  prev: number | null,
  current: number,
): IndexDeltaCounts {
  const zero: IndexDeltaCounts = { missing: 0, duplicated: 0, reordered: 0 };
  if (prev === null || !Number.isFinite(prev) || !Number.isFinite(current)) return zero;
  const delta = current - prev;
  if (delta === 1) return zero;
  if (delta === 0) return { ...zero, duplicated: 1 };
  if (delta > 1) return { ...zero, missing: delta - 1 };
  return { ...zero, reordered: 1 };
}

// Re-exported: bench modules and their tests import percentile from this file.
export { percentile };

/** Window boundaries and out-of-band counters, supplied by the controller. */
export interface WindowBounds {
  /** Window open, browser epoch ms. Always present, frames or not. */
  t_start_ms: number;
  /** Window close, browser epoch ms. Always present, frames or not. */
  t_end_ms: number;
  /** Clock offset in effect at window close; null when unmeasured. */
  offset_ms: number | null;
  /** Callbacks in this window where the <video> yielded no readable image. */
  no_image: number;
  /** Input pulses abandoned in this window because no echo ever arrived. */
  i2p_missed: number;
}

/** The `bench.window` trace-event payload (free-form by contract). */
export interface BenchWindowPayload {
  [key: string]: unknown;

  // The window carries its own clock — the page stamps it, the fold uses it.
  // Downstream must never re-derive timestamps from launch_epoch + ordinal.
  /** Window open, browser epoch ms. */
  t_start_ms: number;
  /** Window close, browser epoch ms. */
  t_end_ms: number;
  /** t_end_ms expressed on the HOST clock; null when the offset is unmeasured. */
  t_end_host_ms: number | null;
  /** Last decoded frame's own `host_time_ms` — exact host clock from the
   *  pixels, no offset needed, so the preferred join key. Null if none decoded. */
  last_host_time_ms: number | null;

  /** Displayed frames with a readable image in this window (decoded or not). */
  n: number;
  /** Of those, how many decoded with a valid CRC. */
  decoded: number;
  /** Image but no valid marker. Not `crc_fail`: also counts frames where the
   *  marker was simply absent (app not painting yet, marker off-crop). */
  undecoded: number;
  /** RVFC callbacks where the <video> had no readable pixels; excluded from `n`. */
  no_image: number;

  g2g_p50_ms: number | null;
  g2g_p95_ms: number | null;
  g2g_max_ms: number | null;

  /** Frame indices that never reached the display. See `classifyIndexDelta`. */
  missing_indices: number;
  duplicated: number;
  reordered: number;

  /** Input-to-photon samples completed in this window (ms), pure client clock. */
  i2p_ms: number[];
  /** Pulses abandoned in this window because the echo never arrived. */
  i2p_missed: number;

  render_w: number | null;
  render_h: number | null;
  /** Clock offset in effect (ms); null when unmeasured. */
  offset_ms: number | null;
  /** True when no ST-05 offset was available — g2g is then a raw clock difference. */
  offset_unknown: boolean;

  // Plus the flat stage_* keys (stages.ts) when the controller was given a
  // stage source; individually null when the UA supplied no metadata.
}

/**
 * One window's frames → the trace-event payload. `deltas` is accumulated by
 * the caller (the previous frame may belong to the previous window). An empty
 * window yields `n: 0` — a freeze is windows, never a gap. render_w/h come
 * from the last decoded frame (that frame carries FLAG_RESOLUTION_CHANGE).
 */
export function summarizeWindow(
  frames: BenchFrame[],
  deltas: IndexDeltaCounts,
  i2pMs: number[],
  bounds: WindowBounds,
): BenchWindowPayload {
  const decodedFrames = frames.filter((f) => f.decoded);
  const g2g: number[] = [];
  for (const f of decodedFrames) if (f.g2g_ms != null) g2g.push(f.g2g_ms);
  const last = decodedFrames[decodedFrames.length - 1];
  const offset = bounds.offset_ms;
  return {
    t_start_ms: bounds.t_start_ms,
    t_end_ms: bounds.t_end_ms,
    t_end_host_ms: offset === null ? null : bounds.t_end_ms + offset,
    last_host_time_ms: last?.host_time_ms ?? null,

    n: frames.length,
    decoded: decodedFrames.length,
    undecoded: frames.length - decodedFrames.length,
    no_image: bounds.no_image,

    g2g_p50_ms: percentile(g2g, 50),
    g2g_p95_ms: percentile(g2g, 95),
    g2g_max_ms: g2g.length > 0 ? Math.max(...g2g) : null,

    missing_indices: deltas.missing,
    duplicated: deltas.duplicated,
    reordered: deltas.reordered,

    i2p_ms: [...i2pMs],
    i2p_missed: bounds.i2p_missed,

    render_w: last?.render_w ?? null,
    render_h: last?.render_h ?? null,
    offset_ms: offset,
    offset_unknown: offset === null,
  };
}
