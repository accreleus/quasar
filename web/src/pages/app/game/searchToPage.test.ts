import { describe, expect, it } from "vitest";
import { paletteGameHref, searchOnGamePage } from "./searchToPage";

describe("search to the game page", () => {
  it("is inert under Classic layout", () => {
    expect(paletteGameHref({ id: "app-7" }, false, false)).toBeUndefined();
    expect(searchOnGamePage("/app/game/7", false)).toBe(false);
  });

  it("sends a player's game row to its page, and nothing else", () => {
    expect(paletteGameHref({ id: "app-7" }, false, true)).toBe("/app/game/7");
    expect(paletteGameHref({ id: "app-7" }, true, true)).toBeUndefined();
    expect(paletteGameHref({ id: "go-home" }, false, true)).toBeUndefined();
    expect(searchOnGamePage("/app/game/7", true)).toBe(true);
    expect(searchOnGamePage("/app/library", true)).toBe(false);
  });
});
