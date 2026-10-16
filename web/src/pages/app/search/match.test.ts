import { describe, expect, it } from "vitest";
import { osa, rank, score, type Recency } from "./match";

const apps = [
  { id: "p1", name: "Portal" },
  { id: "p2", name: "Portal 2" },
  { id: "pt", name: "My Time at Portia" },
  { id: "c", name: "Cities: Skylines" },
  { id: "f", name: "Fable Anniversary" },
  { id: "h", name: "Half-Life 2" },
  { id: "w", name: "The Witcher 3: Wild Hunt" },
];
const names = (q: string, r: Recency = () => 0) => rank(apps, q, r).map((a) => a.name);

describe("fuzzy matcher", () => {
  it("osa counts a swap as one edit", () => {
    expect(osa("wticher", "witcher")).toBe(1);
    expect(osa("ciies", "cities")).toBe(1);
  });

  it("ranks exact > prefix > typo", () => {
    expect(names("portal")[0]).toBe("Portal");
    expect(names("portl").slice(0, 2)).toEqual(["Portal", "Portal 2"]);
    expect(score(apps[0], "portal")).toBeGreaterThan(score(apps[0], "porta"));
    expect(score(apps[0], "porta")).toBeGreaterThan(score(apps[0], "portl"));
  });

  it("finds half-remembered, misspelled names that substring search misses", () => {
    expect(names("ciies sky")).toEqual(["Cities: Skylines"]);
    expect(names("fale ann")).toEqual(["Fable Anniversary"]);
    expect(names("wticher 3")).toEqual(["The Witcher 3: Wild Hunt"]);
    expect(names("halflife")).toEqual(["Half-Life 2"]);
  });

  it("allows no typo under 4 letters, and every word must land", () => {
    expect(names("pxr")).toEqual([]);
    expect(names("portal zzzz")).toEqual([]);
  });

  it("breaks ties by recency", () => {
    const recent = (id: string) => (id === "p2" ? 2 : 0);
    expect(names("port", recent)[0]).toBe("Portal 2");
  });
});

describe("relative cutoff", () => {
  const list = ["GRIS", "Prison Architect", "Risk of Rain 2", "Rise of Nations", "Fable Anniversary", "Fallout 4"].map((name, i) => ({ id: `a${i}`, name }));
  it("an exact query returns only its match, a typo still finds its game", () => {
    expect(rank(list, "gris").map((a) => a.name)).toEqual(["GRIS"]);
    expect(rank(list, "fale ann")[0].name).toBe("Fable Anniversary");
  });
});

describe("the cutoff keeps every clean hit", () => {
  const titles = [
    "Tomb Raider", "Rise of the Tomb Raider", "Shadow of the Tomb Raider", "Raiders of the Broken Planet",
    "Space Engineers", "Kerbal Space Program", "Dead Space", "Spaceteam",
    "Apex Legends", "League of Legends", "Legend of Zelda",
    "Risk of Rain 2", "Half-Life 2", "Left 4 Dead 2", "2K Basketball", "GRIS", "Prison Architect",
  ].map((name, i) => ({ id: `t${i}`, name }));
  const got = (q: string) => rank(titles, q).map((a) => a.name);

  it("a whole-word match late in a name is not cut by a better-scoring one", () => {
    expect(got("tomb raider")).toEqual(
      expect.arrayContaining(["Tomb Raider", "Rise of the Tomb Raider", "Shadow of the Tomb Raider"]),
    );
    expect(got("space")).toEqual(expect.arrayContaining(["Kerbal Space Program", "Dead Space", "Space Engineers"]));
    expect(got("legends")).toEqual(expect.arrayContaining(["Apex Legends", "League of Legends"]));
    expect(got("2")).toEqual(expect.arrayContaining(["Risk of Rain 2", "Half-Life 2", "Left 4 Dead 2", "2K Basketball"]));
  });

  it("still drops typo-level hits once something matches cleanly", () => {
    expect(got("gris")).toEqual(["GRIS"]);
    expect(got("tomb raider")).not.toContain("Prison Architect");
  });

  it("keeps typo hits when nothing matches cleanly", () => {
    expect(got("gruis")).toContain("GRIS");
  });
});
