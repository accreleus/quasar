// Library hero band palette sampler (hero-band spec B1), the algorithm ported
// from design_mockups/library-expanded-hero.html's `sampleAndSet`: pure
// `computePalette` over raw pixels + a thin canvas wrapper (`samplePalette`).
// The tunables below carry the sampled geometry/thresholds.
//
// Every getImageData read is wrapped and falls back to the brand default (a
// misbehaving artwork provider, zero-size image, or no 2D context can still
// throw despite same-origin art). A no-art app uses the default directly,
// without invoking the sampler — see LibraryDetail.

/** CSS custom-property values, not colours. The two separators differ on
 * purpose: `rgba(var(--scrim-rgb), a)` needs a comma triple, while
 * `rgb(var(--accent-rgb))` uses the space syntax home.css's fallback uses. */
export interface HeroPalette {
  /** "r,g,b" — the 80%-mixed scrim colour. */
  scrimRgb: string;
  /** "r g b" — the most saturated accent pixel. */
  accentRgb: string;
  /** true when the art reads dark enough that hero text should be light. */
  darkArt: boolean;
}

/** Brand violet (106,69,245) over the app's dark ink-2 (#11111a) — used
 * whenever no art is sampled (missing art) or sampling fails (tainted
 * canvas, zero-size image, no 2D context). */
export const DEFAULT_HERO_PALETTE: HeroPalette = {
  scrimRgb: "17,17,26",
  accentRgb: "106 69 245",
  darkArt: true,
};

const SAMPLE_WIDTH = 64;
const SAMPLE_HEIGHT = 36;
/** Only the left 55% is sampled — that's where the band's text sits. */
const SAMPLE_FRACTION = 0.55;
/** Below this average luminance (out of 255), art counts as "dark". */
const DARK_LUMINANCE_THRESHOLD = 140;
/** A candidate accent pixel must clear this brightness floor — otherwise
 * near-black noise pixels can score as "highly saturated". */
const ACCENT_BRIGHTNESS_FLOOR = 70;
/** How far the average colour is mixed toward the scrim's pole (black/white). */
const SCRIM_MIX = 0.8;

/**
 * Pure palette derivation over a flat RGBA pixel buffer (as returned by
 * `CanvasRenderingContext2D.getImageData(...).data`). Unit-testable with
 * synthetic arrays — no DOM/canvas needed.
 */
export function computePalette(data: Uint8ClampedArray): HeroPalette {
  let rSum = 0;
  let gSum = 0;
  let bSum = 0;
  let n = 0;
  let accentR = 106;
  let accentG = 69;
  let accentB = 245;
  let bestScore = -1;

  for (let i = 0; i < data.length; i += 4) {
    const r = data[i];
    const g = data[i + 1];
    const b = data[i + 2];
    rSum += r;
    gSum += g;
    bSum += b;
    n++;

    const mx = Math.max(r, g, b);
    const mn = Math.min(r, g, b);
    const sat = mx === 0 ? 0 : (mx - mn) / mx;
    const score = sat * mx;
    if (score > bestScore && mx > ACCENT_BRIGHTNESS_FLOOR) {
      bestScore = score;
      accentR = r;
      accentG = g;
      accentB = b;
    }
  }

  if (n === 0) return DEFAULT_HERO_PALETTE;

  const r = rSum / n;
  const g = gSum / n;
  const b = bSum / n;
  const luminance = 0.2126 * r + 0.7152 * g + 0.0722 * b;
  const darkArt = luminance < DARK_LUMINANCE_THRESHOLD;

  const mix = (channel: number, pole: number) =>
    Math.round(channel * (1 - SCRIM_MIX) + pole * SCRIM_MIX);
  const pole = darkArt ? 0 : 255;
  const scrim = [mix(r, pole), mix(g, pole), mix(b, pole)];

  return {
    scrimRgb: scrim.join(","),
    accentRgb: `${accentR} ${accentG} ${accentB}`,
    darkArt,
  };
}

/**
 * Draws `img` into a small offscreen canvas and derives its palette.
 * `img` must already be loaded (`complete === true` / naturalWidth > 0) —
 * callers sample on the `<img>` load event or after checking `.complete`.
 * Never throws: any failure (tainted canvas, zero-size image, no 2D
 * context) is caught and the brand-default palette is returned instead.
 */
export function samplePalette(img: HTMLImageElement): HeroPalette {
  try {
    if (img.naturalWidth === 0 || img.naturalHeight === 0) return DEFAULT_HERO_PALETTE;
    const canvas = document.createElement("canvas");
    canvas.width = SAMPLE_WIDTH;
    canvas.height = SAMPLE_HEIGHT;
    const ctx = canvas.getContext("2d", { willReadFrequently: true });
    if (!ctx) return DEFAULT_HERO_PALETTE;
    ctx.drawImage(img, 0, 0, SAMPLE_WIDTH, SAMPLE_HEIGHT);
    const sampleWidth = Math.floor(SAMPLE_WIDTH * SAMPLE_FRACTION);
    if (sampleWidth <= 0) return DEFAULT_HERO_PALETTE;
    const { data } = ctx.getImageData(0, 0, sampleWidth, SAMPLE_HEIGHT);
    return computePalette(data);
  } catch {
    return DEFAULT_HERO_PALETTE;
  }
}
