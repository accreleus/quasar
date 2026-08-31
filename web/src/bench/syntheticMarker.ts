// Synthetic marker-frame generator — TEST SUPPORT ONLY.
//
// Ported from quasar-benchgame `tools/test_marker_decode.js` (commit 9329b1d),
// which is the encoder-side reference the Rust renderer is checked against. It
// is deliberately an INDEPENDENT implementation of the layout in
// docs/marker-spec.md: encoding with the same code we decode with would prove
// nothing, so this writes the cells straight from the spec tables.
//
// Not imported by any production module.

import { crc16Ccitt, type MarkerImageData } from "./marker_decode.js";

const GRID_W = 30;
const GRID_H = 19;
const SYNC = [1, 0, 1, 1, 0, 0, 1, 0, 1, 1, 1, 0, 0, 0, 1, 0, 1, 0, 0, 1];
const PATTERNS = [
  [0, 0, 0, 0],
  [0, 1, 1, 0],
  [1, 0, 0, 1],
  [1, 1, 1, 1],
];

export interface MarkerFields {
  frame_index: number;
  host_time_ms: number;
  scene_id: number;
  load_level: number;
  event_flags: number;
  render_w: number;
  render_h: number;
}

/** The 19-byte payload: 17 data bytes then the big-endian CRC-16/CCITT-FALSE. */
export function encodePayload(f: MarkerFields): Uint8Array {
  const bytes = new Uint8Array(19);
  const put16 = (o: number, v: number) => {
    bytes[o] = (v >>> 8) & 0xff;
    bytes[o + 1] = v & 0xff;
  };
  bytes[0] = (f.frame_index >>> 24) & 0xff;
  bytes[1] = (f.frame_index >>> 16) & 0xff;
  bytes[2] = (f.frame_index >>> 8) & 0xff;
  bytes[3] = f.frame_index & 0xff;
  let host = f.host_time_ms;
  for (let i = 9; i >= 4; i--) {
    bytes[i] = host % 256;
    host = Math.floor(host / 256);
  }
  bytes[10] = f.scene_id;
  bytes[11] = f.load_level;
  bytes[12] = f.event_flags;
  put16(13, f.render_w);
  put16(15, f.render_h);
  put16(17, crc16Ccitt(bytes, 17));
  return bytes;
}

/** The 30×19 cell canvas (0 = black, 255 = white). */
export function markerCells(bytes: Uint8Array): Uint8Array {
  const cells = new Uint8Array(GRID_W * GRID_H);
  cells.fill(255);
  for (let y = 1; y < 18; y++) for (let x = 1; x < 21; x++) cells[y * GRID_W + x] = 0;
  for (let x = 0; x < 20; x++) cells[GRID_W + x + 1] = SYNC[x]! * 255;
  for (let i = 0; i < 76; i++) {
    let symbol = 0;
    for (let k = 0; k < 2; k++) {
      const bit = i * 2 + k;
      symbol |= ((bytes[Math.floor(bit / 8)]! >>> (7 - (bit % 8))) & 1) << (1 - k);
    }
    const x = 1 + (i % 10) * 2;
    const y = 2 + Math.floor(i / 10) * 2;
    const pattern = PATTERNS[symbol]!;
    cells[y * GRID_W + x] = pattern[0]! * 255;
    cells[y * GRID_W + x + 1] = pattern[1]! * 255;
    cells[(y + 1) * GRID_W + x] = pattern[2]! * 255;
    cells[(y + 1) * GRID_W + x + 1] = pattern[3]! * 255;
  }
  const heartbeat = bytes[3]! & 1 ? 0 : 255;
  const pulse = bytes[12]! & 1 ? 255 : 0;
  for (let y = 2; y < 6; y++) for (let x = 24; x < 28; x++) cells[y * GRID_W + x] = heartbeat;
  for (let y = 9; y < 13; y++) for (let x = 24; x < 28; x++) cells[y * GRID_W + x] = pulse;
  return cells;
}

/** Composite the marker onto a checkerboard "scene" at (ox, oy), cell px per cell. */
export function syntheticFrame(
  fields: MarkerFields,
  opts: { width: number; height: number; ox: number; oy: number; cell: number },
): MarkerImageData {
  const { width, height, ox, oy, cell } = opts;
  const data = new Uint8ClampedArray(width * height * 4);
  for (let y = 0; y < height; y++) {
    for (let x = 0; x < width; x++) {
      const p = (y * width + x) * 4;
      const checker = ((x >>> 4) ^ (y >>> 4)) & 1;
      data[p] = 42 + checker * 24;
      data[p + 1] = 84;
      data[p + 2] = 123 - checker * 17;
      data[p + 3] = 255;
    }
  }
  const cells = markerCells(encodePayload(fields));
  for (let my = 0; my < GRID_H; my++) {
    for (let mx = 0; mx < GRID_W; mx++) {
      const value = cells[my * GRID_W + mx]!;
      for (let dy = 0; dy < cell; dy++) {
        for (let dx = 0; dx < cell; dx++) {
          const x = ox + mx * cell + dx;
          const y = oy + my * cell + dy;
          if (x < 0 || y < 0 || x >= width || y >= height) continue;
          const p = (y * width + x) * 4;
          data[p] = value;
          data[p + 1] = value;
          data[p + 2] = value;
          data[p + 3] = 255;
        }
      }
    }
  }
  return { width, height, data };
}
