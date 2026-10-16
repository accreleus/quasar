// Whether a transparent SteamGridDB logo reads on the page's dark scrim.
// The hero's ink is pinned light in both themes, so the scrim is always dark;
// a black or very dark logo would vanish into it and the text title wins.

/** Mean relative luminance of the logo's opaque pixels, and how much of the box they fill. */
export function logoInk(rgba: ArrayLike<number>): { luminance: number; coverage: number } {
  let sum = 0;
  let n = 0;
  const px = rgba.length / 4;
  for (let i = 0; i < rgba.length; i += 4) {
    if (rgba[i + 3] < 128) continue;
    const lin = (v: number) => {
      const c = v / 255;
      return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    };
    sum += 0.2126 * lin(rgba[i]) + 0.7152 * lin(rgba[i + 1]) + 0.0722 * lin(rgba[i + 2]);
    n++;
  }
  return { luminance: n ? sum / n : 0, coverage: px ? n / px : 0 };
}

/** ~3:1 against the darkest scrim for the logo's average ink, and something actually drawn. */
export function logoReadable(rgba: ArrayLike<number>): boolean {
  const { luminance, coverage } = logoInk(rgba);
  return coverage >= 0.02 && luminance >= 0.12;
}

/** Samples a loaded, same-origin image. Unknown (a tainted canvas) counts as unreadable. */
export function imageLogoReadable(img: HTMLImageElement): boolean {
  try {
    const w = 96;
    const h = Math.max(1, Math.round((img.naturalHeight / img.naturalWidth) * w)) || 32;
    const c = document.createElement("canvas");
    c.width = w;
    c.height = h;
    const ctx = c.getContext("2d");
    if (!ctx) return false;
    ctx.drawImage(img, 0, 0, w, h);
    return logoReadable(ctx.getImageData(0, 0, w, h).data);
  } catch {
    return false;
  }
}
