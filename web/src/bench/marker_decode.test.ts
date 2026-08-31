// The vendored decoder must stay bit-compatible with the app's Rust encoder.
// These are the checks quasar-benchgame's own tools/test_marker_decode.js runs,
// re-run here against OUR copy so a bad re-vendor is caught in `npx vitest run`
// rather than live on a bench host.

import { describe, expect, it } from "vitest";
import { crc16Ccitt, decodeMarker } from "./marker_decode.js";
import { syntheticFrame, type MarkerFields } from "./syntheticMarker";

const FIELDS: MarkerFields = {
  frame_index: 0xf1234567,
  host_time_ms: 1800123456789,
  scene_id: 4,
  load_level: 10,
  event_flags: 7,
  render_w: 3840,
  render_h: 2160,
};

describe("vendored marker decoder", () => {
  it("reproduces the CRC-16/CCITT-FALSE check vector 0x29B1", () => {
    expect(crc16Ccitt(new TextEncoder().encode("123456789"))).toBe(0x29b1);
  });

  it("round-trips every payload field from a synthetic frame", () => {
    const image = syntheticFrame(FIELDS, { width: 960, height: 540, ox: 37, oy: 29, cell: 16 });
    const found = decodeMarker(image);
    expect(found.decoded).toBe(true);
    if (!found.decoded) return;
    expect(found.frame_index).toBe(FIELDS.frame_index);
    expect(found.host_time_ms).toBe(FIELDS.host_time_ms);
    expect(found.scene_id).toBe(FIELDS.scene_id);
    expect(found.load_level).toBe(FIELDS.load_level);
    expect(found.event_flags).toBe(FIELDS.event_flags);
    expect(found.render_w).toBe(FIELDS.render_w);
    expect(found.render_h).toBe(FIELDS.render_h);
    expect(found.crc_valid).toBe(true);
    // frame_index is odd → heartbeat cell black; event_flags bit0 set → pulse lit.
    expect(found.heartbeat).toBe(false);
    expect(found.input_pulse).toBe(true);
    expect(found.confidence).toBeGreaterThan(0.8);
  });

  it("decodes from a cached location without re-searching", () => {
    const image = syntheticFrame(FIELDS, { width: 960, height: 540, ox: 37, oy: 29, cell: 16 });
    const first = decodeMarker(image);
    expect(first.decoded).toBe(true);
    if (!first.decoded) return;
    const hinted = decodeMarker(image, {
      location: { x: first.marker_x, y: first.marker_y, cellSize: first.cell_size },
      searchOnHintFailure: false,
    });
    expect(hinted.decoded).toBe(true);
    if (!hinted.decoded) return;
    expect(hinted.frame_index).toBe(FIELDS.frame_index);
  });

  it("reports not-decoded on a frame with no marker", () => {
    const blank = {
      width: 320,
      height: 240,
      data: new Uint8ClampedArray(320 * 240 * 4).fill(64),
    };
    const found = decodeMarker(blank);
    expect(found.decoded).toBe(false);
  });

  it("rejects a payload whose CRC does not cover the data (corrupted cell)", () => {
    const image = syntheticFrame(FIELDS, { width: 960, height: 540, ox: 37, oy: 29, cell: 16 });
    // Invert one whole data cell: the symbol flips, the CRC no longer matches.
    const cell = 16;
    const cx = 37 + 1 * cell;
    const cy = 29 + 2 * cell;
    for (let y = cy; y < cy + cell; y++) {
      for (let x = cx; x < cx + cell; x++) {
        const p = (y * image.width + x) * 4;
        image.data[p] = 255 - image.data[p]!;
        image.data[p + 1] = 255 - image.data[p + 1]!;
        image.data[p + 2] = 255 - image.data[p + 2]!;
      }
    }
    expect(decodeMarker(image).decoded).toBe(false);
  });
});
