import { describe, expect, it, vi } from "vitest";
import { computePalette, DEFAULT_HERO_PALETTE, samplePalette } from "./heroPalette";

/** Builds a flat RGBA buffer from a list of [r,g,b] pixels (alpha is always 255). */
function pixels(rgbs: Array<[number, number, number]>): Uint8ClampedArray {
  const data = new Uint8ClampedArray(rgbs.length * 4);
  rgbs.forEach(([r, g, b], i) => {
    data[i * 4] = r;
    data[i * 4 + 1] = g;
    data[i * 4 + 2] = b;
    data[i * 4 + 3] = 255;
  });
  return data;
}

describe("computePalette — luminance branch", () => {
  it("classifies a near-black art sample as dark (darkArt=true)", () => {
    const p = computePalette(pixels([[10, 10, 10], [12, 8, 14], [9, 11, 13]]));
    expect(p.darkArt).toBe(true);
  });

  it("classifies a near-white art sample as bright (darkArt=false)", () => {
    const p = computePalette(pixels([[240, 240, 240], [230, 235, 238], [250, 248, 245]]));
    expect(p.darkArt).toBe(false);
  });

  it("mixes the scrim toward black for dark art", () => {
    // Average colour (50,50,50), mixed 80% toward black (pole 0):
    // round(50*0.2) = 10 per channel.
    const p = computePalette(pixels([[50, 50, 50], [50, 50, 50]]));
    expect(p.scrimRgb).toBe("10,10,10");
  });

  it("mixes the scrim toward white for bright art", () => {
    // Average colour (200,200,200), mixed 80% toward white (pole 255):
    // round(200*0.2 + 255*0.8) = round(244) = 244 per channel.
    const p = computePalette(pixels([[200, 200, 200], [200, 200, 200]]));
    expect(p.scrimRgb).toBe("244,244,244");
  });

  it("sits exactly at the threshold boundary as dark (lum < 140 is strict)", () => {
    // Luminance of (140,140,140) via Rec.709 weights sums to exactly 140,
    // which is NOT < 140 -> bright.
    const bright = computePalette(pixels([[140, 140, 140]]));
    expect(bright.darkArt).toBe(false);
    // One below crosses into dark.
    const dark = computePalette(pixels([[139, 139, 139]]));
    expect(dark.darkArt).toBe(true);
  });
});

describe("computePalette — accent pick", () => {
  it("picks the most saturated pixel clearing the brightness floor", () => {
    // A dim, fully-saturated red (mx=80) scores 80*1=80; a bright, weakly
    // saturated pixel scores lower — saturation dominates as long as the
    // brightness floor (>70) is cleared.
    const p = computePalette(
      pixels([
        [20, 20, 20], // background filler, low saturation
        [80, 0, 0], // sat=1, mx=80 -> score 80
        [200, 190, 180], // sat is small, mx=200 -> score is much lower
      ]),
    );
    expect(p.accentRgb).toBe("80 0 0");
  });

  it("ignores a highly-saturated pixel that doesn't clear the brightness floor", () => {
    // (70,0,0) has mx=70, which fails `mx > 70` (strict) and must be skipped
    // in favour of a pixel that clears it.
    const p = computePalette(
      pixels([
        [70, 0, 0], // sat=1, mx=70 -> floor NOT cleared, skipped
        [100, 60, 60], // sat = 0.4, mx=100 -> score 40, clears the floor
      ]),
    );
    expect(p.accentRgb).toBe("100 60 60");
  });

  it("falls back to the brand accent when nothing clears the brightness floor", () => {
    const p = computePalette(pixels([[50, 50, 50], [30, 20, 10]]));
    expect(p.accentRgb).toBe(DEFAULT_HERO_PALETTE.accentRgb);
  });
});

describe("computePalette — degenerate input", () => {
  it("returns the default palette for an empty buffer", () => {
    expect(computePalette(new Uint8ClampedArray(0))).toEqual(DEFAULT_HERO_PALETTE);
  });
});

describe("samplePalette — canvas wrapper failure modes", () => {
  it("returns the default palette for a zero-size image (no canvas needed)", () => {
    const img = { naturalWidth: 0, naturalHeight: 0 } as HTMLImageElement;
    expect(samplePalette(img)).toEqual(DEFAULT_HERO_PALETTE);
  });

  it("returns the default palette when getImageData throws (tainted canvas)", () => {
    const img = { naturalWidth: 100, naturalHeight: 50 } as HTMLImageElement;
    // Stub document.createElement("canvas") so the 2D context it returns
    // throws on getImageData — simulates a CORS-tainted canvas without
    // pulling in a canvas-mocking dependency (jsdom has no real canvas).
    const original = document.createElement.bind(document);
    const spy = vi.spyOn(document, "createElement").mockImplementation((tag: string) => {
      if (tag !== "canvas") return original(tag);
      const canvas = original("canvas") as HTMLCanvasElement;
      canvas.getContext = (() => ({
        drawImage: () => {},
        getImageData: () => {
          throw new DOMException("tainted", "SecurityError");
        },
      })) as unknown as HTMLCanvasElement["getContext"];
      return canvas;
    });
    try {
      expect(samplePalette(img)).toEqual(DEFAULT_HERO_PALETTE);
    } finally {
      spy.mockRestore();
    }
  });

  it("returns the default palette when the canvas has no 2D context", () => {
    const img = { naturalWidth: 100, naturalHeight: 50 } as HTMLImageElement;
    const original = document.createElement.bind(document);
    const spy = vi.spyOn(document, "createElement").mockImplementation((tag: string) => {
      if (tag !== "canvas") return original(tag);
      const canvas = original("canvas") as HTMLCanvasElement;
      canvas.getContext = (() => null) as unknown as HTMLCanvasElement["getContext"];
      return canvas;
    });
    try {
      expect(samplePalette(img)).toEqual(DEFAULT_HERO_PALETTE);
    } finally {
      spy.mockRestore();
    }
  });
});
