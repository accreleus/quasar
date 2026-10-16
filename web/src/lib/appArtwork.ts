// Which cover-artwork crop a given library surface should render (UI-P7).

import type { App } from "../api/types";

/** The two crops the artwork service caches per app. */
export type ArtSurface = "tile" | "hero";

/**
 * Pick the artwork URL for a surface. Two crops, not one image scaled — the
 * tile in the hero frame reads as a blown-up thumbnail — so each surface asks
 * for its crop first, cross-falling back to the other (`object-fit: cover`
 * trims, never distorts). Null from both is the gradient tile, a design, not
 * a broken state.
 */
export function artFor(app: Pick<App, "cover_url" | "hero_url">, surface: ArtSurface): string | null {
  const tile = app.cover_url ?? null;
  const hero = app.hero_url ?? null;
  return surface === "hero" ? hero ?? tile : tile ?? hero;
}

/** A width the artwork route can scale a cover to (control-api.md §GET /v1/artwork/{asset}). */
export type ArtWidth = 96 | 320 | 480 | 560;

// Only artwork-service URLs take `?w=`; a directly written cover_url can point anywhere.
const LOCAL_ART = /^\/v1\/artwork\/[0-9a-f]{64}\.(jpg|png|webp)$/;

/** `url` scaled for a tile `w` px wide, or unchanged when it is not local artwork. */
export function sizedArt(url: string, w: ArtWidth): string {
  return LOCAL_ART.test(url) ? `${url}?w=${w}` : url;
}

/** A `srcSet` of the tile widths, so the browser picks by device pixel ratio; undefined
 *  when the URL is not local artwork. */
export function artSrcSet(url: string): string | undefined {
  return LOCAL_ART.test(url) ? ([96, 320, 480, 560] as const).map((w) => `${sizedArt(url, w)} ${w}w`).join(", ") : undefined;
}
