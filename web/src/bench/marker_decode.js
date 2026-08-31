/* VENDORED — quasar-benchapp reference marker decoder.
 *
 * Source: the quasar-benchgame repository, tools/marker_decode.js
 * Source commit: 9329b1d ("feat(window): --surface WxH and decorations off")
 * Spec: quasar-benchgame docs/marker-spec.md (marker format version 1)
 *
 * The SPA's Content-Security-Policy blocks `page.addScriptTag` and in-page
 * `eval` (see docs/design/research/2026-08-18-benchapp-bringup.md §6), so the
 * decoder cannot be injected by a harness at runtime — it has to be BUNDLED.
 * Hence this vendored copy.
 *
 * The decoder body below is byte-identical to the source apart from:
 *   - the UMD wrapper replaced by ESM `export` (last two lines),
 *   - the two-space indent of the IIFE body removed.
 * Do not "improve" it: it must stay bit-compatible with the Rust encoder and
 * the Python decoder. Re-vendor from source instead.
 *
 * Types: ./marker_decode.d.ts
 */

const GRID_W = 30, GRID_H = 19, MAIN_W = 22;
const SYNC = [1, 0, 1, 1, 0, 0, 1, 0, 1, 1, 1, 0, 0, 0, 1, 0, 1, 0, 0, 1];
const PATTERNS = [
  [0, 0, 0, 0], [0, 1, 1, 0], [1, 0, 0, 1], [1, 1, 1, 1]
];

function crc16Ccitt(bytes, length) {
  let crc = 0xffff;
  const count = length === undefined ? bytes.length : length;
  for (let i = 0; i < count; i++) {
    crc ^= bytes[i] << 8;
    for (let bit = 0; bit < 8; bit++) {
      crc = (crc & 0x8000) ? (((crc << 1) ^ 0x1021) & 0xffff) : ((crc << 1) & 0xffff);
    }
  }
  return crc;
}

function makeLuma(imageData) {
  if (!imageData || !Number.isInteger(imageData.width) || !Number.isInteger(imageData.height) || !imageData.data) {
    throw new TypeError("decodeMarker expects an ImageData-like {width, height, data} object");
  }
  const width = imageData.width, height = imageData.height, rgba = imageData.data;
  if (rgba.length < width * height * 4) throw new RangeError("ImageData buffer is too short");
  const luma = new Uint8Array(width * height);
  for (let p = 0, q = 0; q < luma.length; p += 4, q++) {
    luma[q] = (77 * rgba[p] + 150 * rgba[p + 1] + 29 * rgba[p + 2] + 128) >> 8;
  }
  return { width, height, luma };
}

function makeIntegral(frame) {
  const { width, height, luma } = frame;
  const stride = width + 1;
  const integral = new Float64Array(stride * (height + 1));
  for (let y = 0; y < height; y++) {
    let row = 0;
    const src = y * width, dst = (y + 1) * stride, previous = y * stride;
    for (let x = 0; x < width; x++) {
      row += luma[src + x];
      integral[dst + x + 1] = integral[previous + x + 1] + row;
    }
  }
  frame.integral = integral;
  frame.integralStride = stride;
  return frame;
}

function rectMean(frame, xa, ya, xb, yb) {
  const s = frame.integralStride, a = frame.integral;
  const sum = a[yb * s + xb] - a[ya * s + xb] - a[yb * s + xa] + a[ya * s + xa];
  return sum / ((xb - xa) * (yb - ya));
}

function scaleCandidates(width, height) {
  const maximum = Math.min(width / MAIN_W, height / GRID_H);
  const result = [];
  if (maximum < 1.5) return result;
  const smallLimit = Math.min(48, Math.floor(maximum));
  for (let value = 2; value <= smallLimit; value++) {
    result.push(value);
    if (value + 0.5 <= maximum) result.push(value + 0.5);
  }
  for (let value = 50; value <= maximum + 0.5; value *= 1.09) result.push(Math.min(value, maximum));
  result.push(maximum);
  result.sort((a, b) => a - b);
  return result.filter((value, index) => index === 0 || Math.abs(value - result[index - 1]) > 0.05);
}

function retain(best, item, limit) {
  if (item.score <= 0.7) return;
  if (best.length < limit) {
    best.push(item);
    best.sort((a, b) => b.score - a.score);
  } else if (item.score > best[best.length - 1].score) {
    best[best.length - 1] = item;
    best.sort((a, b) => b.score - a.score);
  }
}

function coarseCandidates(frame, limit) {
  const { width, height, luma } = frame;
  const best = [];
  for (const cell of scaleCandidates(width, height)) {
    const step = Math.max(1, Math.floor(cell * 0.48));
    const xmax = Math.floor(width - MAIN_W * cell), ymax = Math.floor(height - GRID_H * cell);
    if (xmax < 0 || ymax < 0) continue;
    for (let y = 0; y <= ymax; y += step) {
      const sy = Math.min(height - 1, Math.round(y + 1.5 * cell));
      for (let x = 0; x <= xmax; x += step) {
        let whiteSum = 0, blackSum = 0;
        const values = new Array(20);
        for (let i = 0; i < 20; i++) {
          const sx = Math.min(width - 1, Math.round(x + (i + 1.5) * cell));
          const value = luma[sy * width + sx];
          values[i] = value;
          if (SYNC[i]) whiteSum += value; else blackSum += value;
        }
        const whiteMean = whiteSum / 10, blackMean = blackSum / 10;
        if (whiteMean <= blackMean) continue;
        let whiteVar = 0, blackVar = 0;
        for (let i = 0; i < 20; i++) {
          const delta = values[i] - (SYNC[i] ? whiteMean : blackMean);
          if (SYNC[i]) whiteVar += delta * delta; else blackVar += delta * delta;
        }
        const within = Math.sqrt(whiteVar / 10) + Math.sqrt(blackVar / 10) + 12;
        retain(best, { score: (whiteMean - blackMean) / within, x, y, cell }, limit);
      }
    }
  }
  return best;
}

function sampleCells(frame, x0, y0, cell) {
  const { width, height } = frame;
  if (x0 < -0.25 || y0 < -0.25 || x0 + GRID_W * cell > width + 0.25 || y0 + GRID_H * cell > height + 0.25) return null;
  const cells = new Float32Array(GRID_W * GRID_H);
  const radius = Math.max(0.45, cell * 0.23);
  for (let row = 0; row < GRID_H; row++) {
    const cy = y0 + (row + 0.5) * cell;
    const ya = Math.max(0, Math.ceil(cy - radius));
    const yb = Math.min(height, Math.floor(cy + radius) + 1);
    for (let col = 0; col < GRID_W; col++) {
      const cx = x0 + (col + 0.5) * cell;
      const xa = Math.max(0, Math.ceil(cx - radius));
      const xb = Math.min(width, Math.floor(cx + radius) + 1);
      if (xa >= xb || ya >= yb) return null;
      cells[row * GRID_W + col] = rectMean(frame, xa, ya, xb, yb);
    }
  }
  return cells;
}

// Fast path for video: once a full decode has returned a location, only read small
// patches at the 570 cell centers.  This avoids allocating luma/integral
// buffers for the entire video frame on every call.
function sampleCellsImageData(imageData, x0, y0, cell) {
  const width = imageData.width, height = imageData.height, rgba = imageData.data;
  if (!Number.isFinite(x0) || !Number.isFinite(y0) || !Number.isFinite(cell) || cell < 1.25 ||
      x0 < -0.25 || y0 < -0.25 || x0 + GRID_W * cell > width + 0.25 || y0 + GRID_H * cell > height + 0.25) return null;
  const cells = new Float32Array(GRID_W * GRID_H);
  const radius = Math.max(0.45, cell * 0.18);
  for (let row = 0; row < GRID_H; row++) {
    const cy = y0 + (row + 0.5) * cell;
    const ya = Math.max(0, Math.ceil(cy - radius)), yb = Math.min(height, Math.floor(cy + radius) + 1);
    for (let col = 0; col < GRID_W; col++) {
      const cx = x0 + (col + 0.5) * cell;
      const xa = Math.max(0, Math.ceil(cx - radius)), xb = Math.min(width, Math.floor(cx + radius) + 1);
      if (xa >= xb || ya >= yb) return null;
      let sum = 0, count = 0;
      for (let y = ya; y < yb; y++) for (let x = xa; x < xb; x++) {
        const p = (y * width + x) * 4;
        sum += (77 * rgba[p] + 150 * rgba[p + 1] + 29 * rgba[p + 2] + 128) >> 8;
        count++;
      }
      cells[row * GRID_W + col] = sum / count;
    }
  }
  return cells;
}

function median(values) {
  values.sort((a, b) => a - b);
  const n = values.length;
  return n & 1 ? values[n >> 1] : (values[n / 2 - 1] + values[n / 2]) / 2;
}

function syncQuality(cells) {
  const whites = [], blacks = [];
  for (let i = 0; i < 20; i++) (SYNC[i] ? whites : blacks).push(cells[GRID_W + i + 1]);
  const white = median(whites), black = median(blacks), separation = white - black;
  if (separation <= 1) return { quality: -1, black, white };
  let syncError = 0;
  for (let i = 0; i < 20; i++) {
    const value = Math.max(0, Math.min(1, (cells[GRID_W + i + 1] - black) / separation));
    syncError += Math.abs(value - SYNC[i]);
  }
  syncError /= 20;
  let borderError = 0, borderCount = 0;
  function addBorder(value) {
    borderError += Math.max(0, Math.min(1, (white - value) / separation));
    borderCount++;
  }
  for (let x = 0; x < MAIN_W; x++) addBorder(cells[x]);
  for (let y = 0; y < GRID_H; y++) {
    addBorder(cells[y * GRID_W]);
    addBorder(cells[y * GRID_W + MAIN_W - 1]);
  }
  return {
    quality: Math.min(1, separation / 150) * (1 - syncError) - 0.12 * borderError / borderCount,
    black, white
  };
}

function refine(frame, candidate) {
  let best = null;
  const radii = [candidate.cell * 0.62, candidate.cell * 0.25, candidate.cell * 0.09, 0.35];
  for (const radius of radii) {
    const bx = best ? best.x : candidate.x, by = best ? best.y : candidate.y;
    const bc = best ? best.cell : candidate.cell;
    let trialBest = best;
    for (const ds of [-radius / 20, 0, radius / 20]) {
      const cell = bc + ds;
      if (cell < 1.25) continue;
      for (const dy of [-radius, 0, radius]) for (const dx of [-radius, 0, radius]) {
        const cells = sampleCells(frame, bx + dx, by + dy, cell);
        if (!cells) continue;
        const q = syncQuality(cells);
        const item = { score: q.quality, x: bx + dx, y: by + dy, cell, cells, black: q.black, white: q.white };
        if (!trialBest || item.score > trialBest.score) trialBest = item;
      }
    }
    if (!trialBest) return null;
    best = trialBest;
  }
  return best;
}

function normalized(value, black, span) {
  return Math.max(0, Math.min(1, (value - black) / span));
}

function decodeSampled(item) {
  const { cells, black, white } = item, span = white - black;
  if (span < 18) return null;
  const bytes = new Uint8Array(19);
  let confidenceSum = 0;
  for (let index = 0; index < 76; index++) {
    const sx = index % 10, sy = Math.floor(index / 10);
    const x = 1 + sx * 2, y = 2 + sy * 2;
    const observed = [
      normalized(cells[y * GRID_W + x], black, span),
      normalized(cells[y * GRID_W + x + 1], black, span),
      normalized(cells[(y + 1) * GRID_W + x], black, span),
      normalized(cells[(y + 1) * GRID_W + x + 1], black, span)
    ];
    const distances = [];
    for (let symbol = 0; symbol < 4; symbol++) {
      let distance = 0;
      for (let p = 0; p < 4; p++) {
        const delta = PATTERNS[symbol][p] - observed[p];
        distance += delta * delta;
      }
      distances.push({ symbol, distance: distance / 4 });
    }
    distances.sort((a, b) => a.distance - b.distance);
    const symbol = distances[0].symbol;
    confidenceSum += Math.max(0, Math.min(1, (distances[1].distance - distances[0].distance) / 0.5));
    for (let k = 0; k < 2; k++) {
      const bit = index * 2 + k;
      bytes[Math.floor(bit / 8)] |= ((symbol >> (1 - k)) & 1) << (7 - (bit % 8));
    }
  }
  const expectedCrc = (bytes[17] << 8) | bytes[18];
  if (crc16Ccitt(bytes, 17) !== expectedCrc) return null;
  const u16 = offset => bytes[offset] * 256 + bytes[offset + 1];
  const u32 = offset => (bytes[offset] * 0x1000000 + bytes[offset + 1] * 0x10000 + bytes[offset + 2] * 0x100 + bytes[offset + 3]) >>> 0;
  let hostTime = 0;
  for (let i = 4; i < 10; i++) hostTime = hostTime * 256 + bytes[i];
  let heartbeat = 0, pulse = 0;
  for (let y = 2; y < 6; y++) for (let x = 24; x < 28; x++) heartbeat += normalized(cells[y * GRID_W + x], black, span);
  for (let y = 9; y < 13; y++) for (let x = 24; x < 28; x++) pulse += normalized(cells[y * GRID_W + x], black, span);
  return {
    frame_index: u32(0), host_time_ms: hostTime, scene_id: bytes[10], load_level: bytes[11],
    event_flags: bytes[12], render_w: u16(13), render_h: u16(15), crc16: expectedCrc,
    crc_valid: true, heartbeat: heartbeat / 16 >= 0.5, input_pulse: pulse / 16 >= 0.5,
    symbolConfidence: confidenceSum / 76
  };
}

function decodeMarker(imageData, options) {
  options = options || {};
  if (options.location) {
    const p = options.location;
    const cell = Number(p.cellSize !== undefined ? p.cellSize : p.cell_size);
    const item = { x: Number(p.x), y: Number(p.y), cell };
    item.cells = sampleCellsImageData(imageData, item.x, item.y, item.cell);
    if (item.cells) {
      const q = syncQuality(item.cells);
      item.score = q.quality; item.black = q.black; item.white = q.white;
      const decoded = decodeSampled(item);
      if (decoded) return formatResult(item, decoded);
    }
    if (options.searchOnHintFailure === false) {
      return { decoded: false, confidence: 0, error: "cached marker location did not decode" };
    }
  }
  const frame = makeIntegral(makeLuma(imageData));
  const candidates = coarseCandidates(frame, options.maxCandidates || 36);
  const refined = candidates.map(candidate => refine(frame, candidate)).filter(Boolean);
  refined.sort((a, b) => b.score - a.score);
  for (const item of refined) {
    const decoded = decodeSampled(item);
    if (!decoded) continue;
    return formatResult(item, decoded);
  }
  return { decoded: false, confidence: 0, error: "marker not found or CRC mismatch" };
}

function formatResult(item, decoded) {
  const confidence = Math.max(0, Math.min(1, item.score)) * 0.45 + decoded.symbolConfidence * 0.55;
  delete decoded.symbolConfidence;
  return Object.assign({
    decoded: true, confidence: Math.round(confidence * 1e6) / 1e6,
    marker_x: Math.round(item.x * 1000) / 1000,
    marker_y: Math.round(item.y * 1000) / 1000,
    cell_size: Math.round(item.cell * 1000) / 1000
  }, decoded);
}

export { decodeMarker, crc16Ccitt };
export const constants = { GRID_W, GRID_H, MAIN_W, SYNC: SYNC.slice() };
