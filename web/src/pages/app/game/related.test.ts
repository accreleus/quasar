import { describe, expect, it } from "vitest";
import type { App } from "../../../api/types";
import type { ExtrasMap, LibraryExtras } from "../library/libraryExtras";
import { moreLikeThis } from "./related";

const app = (id: string) => ({ id, name: id, kind: "game" }) as App;
const ex = (rank: number, genres: string[], developer: string | null = null) => ({ rank, genres, developer }) as LibraryExtras;

describe("related", () => {
  const apps = ["a", "b", "c", "d"].map(app);
  const x: ExtrasMap = {
    a: ex(0, ["Indie", "Racing", "Sports"], "Studio"),
    b: ex(5, ["Racing"], "Studio"),
    c: ex(1, ["Racing", "Sports"]),
    d: ex(2, ["Indie"], "Studio"),
  };
  it("ranks by shared specific genres, then popularity, never self or broad-only matches", () =>
    expect(moreLikeThis(apps, x, apps[0]).map((a) => a.id)).toEqual(["c", "b"]));
});
