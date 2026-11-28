import { beforeEach, describe, expect, it, vi } from "vitest";
import * as homeApi from "../../../api/home";
import { ApiError } from "../../../api/client";
import { BUILTIN_HOME, fetchHomeLayout, shownSections } from "./homeLayout";

vi.mock("../../../api/home");
const mocked = vi.mocked(homeApi);
const signal = new AbortController().signal;

beforeEach(() => vi.resetAllMocks());

describe("home layout", () => {
  it("an older server's 404 renders today's order: Highlights, Jump back in, Favourites", async () => {
    mocked.getHomeLayout.mockRejectedValue(new ApiError(404, "not_found", "not found"));
    const layout = await fetchHomeLayout("tok", signal);
    expect(layout).toBe(BUILTIN_HOME);
    expect(shownSections(layout).map((s) => [s.kind, s.title])).toEqual([
      ["highlights", "Highlights"],
      ["jump_back_in", "Jump back in"],
      ["favourites", "Favourites"],
    ]);
  });

  it("any other failure also renders the built-in layout, never an empty Home", async () => {
    mocked.getHomeLayout.mockRejectedValue(new TypeError("network"));
    expect(await fetchHomeLayout("tok", signal)).toBe(BUILTIN_HOME);
  });

  it("skips an unknown kind and a disabled section, keeping the server's order", () => {
    const sections = shownSections({
      source: "admin",
      sections: [
        { id: "fav", kind: "favourites", title: "Picks", enabled: true, params: {} },
        { id: "cw", kind: "continue_watching", title: "Watch", enabled: true, params: {} },
        { id: "hl", kind: "highlights", title: "Highlights", enabled: false, params: {} },
        { id: "jb", kind: "jump_back_in", title: "Again", enabled: true, params: { future: 1 } },
      ],
    });
    expect(sections.map((s) => s.id)).toEqual(["fav", "jb"]);
  });
});
