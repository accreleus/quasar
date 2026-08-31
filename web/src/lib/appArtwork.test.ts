import { describe, expect, it } from "vitest";
import { artFor } from "./appArtwork";

const app = (cover: string | null, hero: string | null) => ({
  cover_url: cover,
  hero_url: hero,
});

describe("artFor", () => {
  // The whole point of caching two crops: each surface must ask for ITS crop
  // first, or the hero renders a stretched thumbnail.
  it("prefers the tile crop for a tile and the hero crop for a hero", () => {
    const both = app("/v1/artwork/aaa.jpg", "/v1/artwork/bbb.jpg");
    expect(artFor(both, "tile")).toBe("/v1/artwork/aaa.jpg");
    expect(artFor(both, "hero")).toBe("/v1/artwork/bbb.jpg");
  });

  // A title may have a grid and no hero (or the reverse). Showing the other
  // crop beats a gradient for an app that demonstrably has art.
  it("falls back across crops when only one is present", () => {
    expect(artFor(app("/tile.jpg", null), "hero")).toBe("/tile.jpg");
    expect(artFor(app(null, "/hero.jpg"), "tile")).toBe("/hero.jpg");
  });

  // No artwork is the SHIPPED DEFAULT and renders the gradient tile — null is
  // the signal for that, and must never be an empty string or a broken src.
  it("returns null when the app has no artwork at all", () => {
    expect(artFor(app(null, null), "tile")).toBeNull();
    expect(artFor(app(null, null), "hero")).toBeNull();
  });

  // The API models "absent" as null, but an undefined field (an older control
  // plane, a partially-typed fixture) must degrade to the gradient too, never
  // to `undefined` reaching an <img src>.
  it("treats undefined like null", () => {
    expect(artFor({ cover_url: undefined, hero_url: undefined } as never, "tile")).toBeNull();
  });
});
