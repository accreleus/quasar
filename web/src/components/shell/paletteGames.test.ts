import { describe, expect, it } from "vitest";
import { searchPaletteFuzzy } from "./paletteGames";
import type { PaletteSources } from "./paletteSearch";

const sources = (isAdmin: boolean): PaletteSources => ({
  hosts: [],
  sessions: [],
  users: [],
  isAdmin,
  apps: [
    { id: "c", name: "Cities: Skylines", kind: "game" },
    { id: "p", name: "Portal 2", kind: "game" },
  ],
});

describe("searchPaletteFuzzy", () => {
  it("leads with ranked games for a typo, routing a player into the library", () => {
    const [games] = searchPaletteFuzzy("ciies sky", sources(false), null);
    expect(games.title).toBe("Games");
    expect(games.items.map((i) => [i.label, i.to])).toEqual([["Cities: Skylines", "/app/library?q=Cities%3A%20Skylines"]]);
  });

  it("sends an admin's hit to the app editor", () => {
    const [games] = searchPaletteFuzzy("portl", sources(true), null);
    expect(games.items[0].to).toBe("/admin/library/apps/p");
  });

  it("keeps the shipped answer for everything else, and for no match", () => {
    expect(searchPaletteFuzzy("", sources(false), null)[0].title).toBe("Actions");
    expect(searchPaletteFuzzy("zzzzzz", sources(false), null)).toEqual([{ title: "No matches", items: [] }]);
  });
});

describe("searchPaletteFuzzy, admin vocabulary", () => {
  it("keeps calling them Apps for an admin", () => {
    expect(searchPaletteFuzzy("portl", sources(true), null)[0].title).toBe("Apps");
  });
});
