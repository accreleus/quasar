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
