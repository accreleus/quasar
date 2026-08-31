// Type declarations for the vendored quasar-benchapp marker decoder
// (./marker_decode.js — see that file's header for provenance).
//
// Field names mirror quasar-benchgame docs/marker-spec.md exactly; they are the
// wire shape of the marker payload, so they stay snake_case.

/** Cached marker location, fed back on the next call to skip the full search. */
export interface MarkerLocation {
  x: number;
  y: number;
  cellSize: number;
}

export interface MarkerDecodeOptions {
  /** Location hint from a previous successful decode (fast path). */
  location?: MarkerLocation;
  /** When false, a failed hint returns immediately instead of re-searching. */
  searchOnHintFailure?: boolean;
  /** Coarse-search candidate cap (default 36). */
  maxCandidates?: number;
}

/** A frame in which the marker was found and its CRC verified. */
export interface MarkerDecodeSuccess {
  decoded: true;
  confidence: number;
  marker_x: number;
  marker_y: number;
  cell_size: number;
  /** Monotonic per-frame counter from the app (u32). */
  frame_index: number;
  /** CLOCK_REALTIME unix ms, sampled by the app before render (u48). */
  host_time_ms: number;
  scene_id: number;
  load_level: number;
  /** bit0 input echo, bit1 scene change, bit2 resolution change, bit3 mark. */
  event_flags: number;
  render_w: number;
  render_h: number;
  crc16: number;
  crc_valid: true;
  heartbeat: boolean;
  input_pulse: boolean;
}

/** The marker was not found, or was found but failed CRC. */
export interface MarkerDecodeFailure {
  decoded: false;
  confidence: number;
  error: string;
}

export type MarkerDecodeResult = MarkerDecodeSuccess | MarkerDecodeFailure;

/** ImageData-like input: the decoder only reads {width, height, data}. */
export interface MarkerImageData {
  width: number;
  height: number;
  data: Uint8ClampedArray | Uint8Array;
}

export function decodeMarker(
  imageData: MarkerImageData,
  options?: MarkerDecodeOptions,
): MarkerDecodeResult;

export function crc16Ccitt(bytes: ArrayLike<number>, length?: number): number;

export const constants: {
  GRID_W: number;
  GRID_H: number;
  MAIN_W: number;
  SYNC: number[];
};
