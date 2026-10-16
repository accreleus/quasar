import { describe, expect, it } from "vitest";
import { searchOnGamePage } from "./searchToPage";

describe("search on the game page", () => {
  it("is inert under Classic layout", () => {
    expect(searchOnGamePage("/app/game/7", false)).toBe(false);
  });

  it("shows the field on a game page only", () => {
    expect(searchOnGamePage("/app/game/7", true)).toBe(true);
    expect(searchOnGamePage("/app/library", true)).toBe(false);
  });
});
