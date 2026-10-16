import { describe, expect, it, vi } from "vitest";
import type { App } from "../../../api/types";
import type { ExtrasMap, LibraryExtras } from "./libraryExtras";
import { LETTERS, arrangeApps, genreOptions, letterOf, lettered, metaLine, spyLetter, topGenres } from "./organise";

const app = (id: string, name: string, favourite = false) => ({ id, name, favourite, kind: "game" }) as unknown as App;
const ext = (genres: string[], played?: { at: string; secs: number }, added = "2026-01-01T00:00:00Z"): LibraryExtras => ({
  genres,
  added_at: added,
  play: played ? { last_played_at: played.at, total_seconds: played.secs } : null,
});

const apps = [app("b", "Borderlands", true), app("e", "Évoland"), app("n", "7 Days"), app("a", "Alan Wake")];
const extras: ExtrasMap = {
  b: ext(["Action"], { at: "2026-10-01T00:00:00Z", secs: 7200 }),
  e: ext(["Racing"], { at: "2026-10-10T00:00:00Z", secs: 600 }, "2026-09-01T00:00:00Z"),
  n: ext(["Action"]),
  a: ext(["Action"]),
};

describe("organised library", () => {
  it("files names under a letter, accents folded, the rest under #", () => {
    expect(letterOf("Évoland")).toBe("E");
    expect(letterOf("7 Days")).toBe("#");
    expect(letterOf("鬼谷八荒")).toBe("#");
  });

  it("a letter is never split: Äpple sits with Apple even where the locale sorts Ä after Z", () => {
    // Swedish collation puts "Ä" after "Z", so a pure locale sort splits the A run.
    const sv = new Intl.Collator("sv");
    const spy = vi.spyOn(String.prototype, "localeCompare").mockImplementation(function (this: string, other: string) {
      return sv.compare(this, other);
    });
    try {
      const list = [app("z", "Zebra"), app("ae", "Äpple"), app("a", "Apple"), app("ss", "ßtest")];
      expect(sv.compare("Äpple", "Zebra")).toBeGreaterThan(0);
      const sorted = arrangeApps(list, "az", null, null);
      expect(sorted.map((a) => a.id)).toEqual(["a", "ae", "ss", "z"]);
      const letters = sorted.map((a) => letterOf(a.name));
      expect(letters).toEqual(["A", "A", "S", "Z"]);
      // Contiguous runs: no letter appears in two sections.
      expect(new Set(letters).size).toBe(letters.filter((l, i) => l !== letters[i - 1]).length);
    } finally {
      spy.mockRestore();
    }
  });

  it("a multi-letter uppercasing still maps to an index letter", () => {
    expect(letterOf("ßtest")).toBe("S");
    expect(LETTERS).toContain(letterOf("ßtest"));
  });

  it("A–Z is one unbroken alphabet, # last, favourites not floated", () => {
    expect(arrangeApps(apps, "az", null, extras).map((a) => a.id)).toEqual(["a", "b", "e", "n"]);
  });

  it("recency and play time put played games first, the rest A–Z", () => {
    expect(arrangeApps(apps, "recent", null, extras).map((a) => a.id)).toEqual(["e", "b", "a", "n"]);
    expect(arrangeApps(apps, "most", null, extras).map((a) => a.id)).toEqual(["b", "e", "a", "n"]);
    expect(arrangeApps(apps, "added", null, extras)[0].id).toBe("e");
  });

  it("without extras every sort is plain A–Z and a genre matches nothing", () => {
    expect(arrangeApps(apps, "recent", null, null).map((a) => a.id)).toEqual(["a", "b", "e", "n"]);
    expect(arrangeApps(apps, "az", "Racing", null)).toEqual([]);
  });

  it("a genre filters, and a genre needs enough titles", () => {
    expect(arrangeApps(apps, "az", "Racing", extras).map((a) => a.id)).toEqual(["e"]);
    expect(topGenres(extras)).toEqual([]);
  });

  it("captions lead with the sort's own key", () => {
    expect(metaLine(apps[0], "az", extras)).toBeNull();
    expect(metaLine(apps[2], "recent", extras)).toBe("Not played yet");
    expect(metaLine(apps[0], "most", extras)).toMatch(/^2 h · /);
    expect(metaLine(apps[0], "recent", extras)).toMatch(/ · 2 h$/);
  });

  it("letter sections start at 60 titles, A–Z only", () => {
    expect(lettered("az", 59)).toBe(false);
    expect(lettered("az", 60)).toBe(true);
    expect(lettered("recent", 600)).toBe(false);
  });
});

describe("genreOptions", () => {
  const x = { a: { genres: ["Racing", "Action"] }, b: { genres: ["Action"] }, c: { genres: ["Indie"] } } as unknown as ExtrasMap;
  const t = (id: string) => ({ id, name: id }) as App;
  it("lists genres alphabetically with counts over the given list", () => {
    expect(genreOptions(["Action", "Racing", "Indie"], [t("a"), t("b"), t("c")], x, null)).toEqual([
      { value: "Action", label: "Action", count: 2 },
      { value: "Indie", label: "Indie", count: 1 },
      { value: "Racing", label: "Racing", count: 1 },
    ]);
  });
  it("drops empty genres but keeps the chosen one", () => {
    const opts = genreOptions(["Action", "Racing", "Indie"], [t("b")], x, "Racing");
    expect(opts.map((o) => [o.value, o.count])).toEqual([["Action", 1], ["Racing", 0]]);
  });
});

describe("spyLetter", () => {
  const secs = [
    { letter: "L", top: -900 },
    { letter: "M", top: 78 },
    { letter: "N", top: 900 },
  ];
  it("marks the section parked at the jump line, not the one above it", () => {
    expect(spyLetter(secs, 78, false, null, 1000)).toBe("M");
  });
  it("marks the previous section while the next is still below the line", () => {
    expect(spyLetter(secs, 60, false, null, 1000)).toBe("L");
  });
  it("marks nothing above the first section", () => {
    expect(spyLetter([{ letter: "A", top: 400 }], 78, false, null, 1000)).toBeNull();
  });
  it("at the bottom, a short last section that was pressed wins while on screen", () => {
    const end = [
      { letter: "Y", top: -200 },
      { letter: "Z", top: 500 },
    ];
    expect(spyLetter(end, 78, true, "Z", 1000)).toBe("Z");
    expect(spyLetter(end, 78, false, "Z", 1000)).toBe("Y");
  });
});
