import { describe, expect, it } from "vitest";
import {
  blockedFamilyIds,
  COVER_CLASSES,
  coverClassAt,
  familyRootApp,
  filterLibraryApps,
  formatElapsedMinutes,
  formatRelativeTime,
  homeAppId,
  lastIndexInRow,
  nextTileIndex,
  presentLaunchError,
  rowSize,
  sortLibraryApps,
} from "./libraryGrid";

// Defect 3 (SessionQuickSwitch rail): the per-app cover colour derivation is
// shared between the library grid (AppHomeNext.tsx) and the drawer's rail
// (SessionQuickSwitch.tsx) — this is the one place it is defined and tested.
describe("coverClassAt", () => {
  it("gives different apps at different name-sorted positions different classes", () => {
    const classes = [0, 1, 2, 3, 4, 5].map(coverClassAt);
    expect(new Set(classes).size).toBe(COVER_CLASSES.length);
  });

  it("wraps around after the palette is exhausted, deterministically", () => {
    expect(coverClassAt(0)).toBe(coverClassAt(COVER_CLASSES.length));
    expect(coverClassAt(1)).toBe(coverClassAt(COVER_CLASSES.length + 1));
  });

  it("only ever returns a known palette class", () => {
    for (let i = 0; i < 20; i++) {
      expect(COVER_CLASSES).toContain(coverClassAt(i));
    }
  });
});

describe("filterLibraryApps", () => {
  const apps = [
    { name: "Path of Exile 2", description: "Action RPG", kind: "game" as const },
    { name: "Blender", description: "3D creation suite", kind: "desktop" as const },
    { name: "Firefox", description: "A browser", kind: "desktop" as const },
    { name: "Age of Empires", description: "RTS classic", kind: "game" as const },
  ];

  it("returns everything for kind=all with no search", () => {
    expect(filterLibraryApps(apps, "all", "")).toHaveLength(4);
  });

  it("filters by kind", () => {
    expect(filterLibraryApps(apps, "game", "").map((a) => a.name)).toEqual([
      "Path of Exile 2",
      "Age of Empires",
    ]);
    expect(filterLibraryApps(apps, "desktop", "").map((a) => a.name)).toEqual([
      "Blender",
      "Firefox",
    ]);
  });

  it("filters by search over name (case-insensitive)", () => {
    expect(filterLibraryApps(apps, "all", "blender").map((a) => a.name)).toEqual(["Blender"]);
    expect(filterLibraryApps(apps, "all", "FIRE").map((a) => a.name)).toEqual(["Firefox"]);
  });

  it("filters by search over description", () => {
    expect(filterLibraryApps(apps, "all", "rpg").map((a) => a.name)).toEqual(["Path of Exile 2"]);
  });

  it("combines kind AND search", () => {
    expect(filterLibraryApps(apps, "desktop", "browser").map((a) => a.name)).toEqual(["Firefox"]);
    expect(filterLibraryApps(apps, "game", "browser")).toHaveLength(0);
  });

  it("treats a missing description as empty, not a crash", () => {
    const noDesc = [{ name: "Weston Terminal", kind: "desktop" as const }];
    expect(filterLibraryApps(noDesc, "all", "weston")).toHaveLength(1);
    expect(filterLibraryApps(noDesc, "all", "nonexistent")).toHaveLength(0);
  });
});

// #385 item 5: `favourite` shipped in UI-P1 with no reader at all. These are
// the pure halves of two of its three consumers (the third is the tile marker,
// covered in ProfilePicker.test.tsx).
describe("filterLibraryApps — the Favourites segment (#385 item 5)", () => {
  const apps = [
    { name: "Path of Exile 2", description: "Action RPG", kind: "game" as const, favourite: true },
    { name: "Blender", description: "3D creation suite", kind: "desktop" as const, favourite: true },
    { name: "Firefox", description: "A browser", kind: "desktop" as const, favourite: false },
    { name: "Age of Empires", description: "RTS classic", kind: "game" as const },
  ];

  it("filters to favourites across kinds — it is a fourth segment value, not a kind", () => {
    expect(filterLibraryApps(apps, "favourite", "").map((a) => a.name)).toEqual([
      "Path of Exile 2",
      "Blender",
    ]);
  });

  it("treats an absent favourite flag as not-favourited", () => {
    expect(filterLibraryApps(apps, "favourite", "").map((a) => a.name)).not.toContain(
      "Age of Empires",
    );
  });

  it("still applies the search on top of the Favourites segment", () => {
    expect(filterLibraryApps(apps, "favourite", "blender").map((a) => a.name)).toEqual(["Blender"]);
    expect(filterLibraryApps(apps, "favourite", "firefox")).toHaveLength(0);
  });

  it("leaves the kind segments untouched — a favourite is still a game/desktop", () => {
    expect(filterLibraryApps(apps, "game", "").map((a) => a.name)).toEqual([
      "Path of Exile 2",
      "Age of Empires",
    ]);
  });
});

describe("sortLibraryApps (favourites hoisted, then name)", () => {
  it("hoists favourites ahead of alphabetically-earlier non-favourites", () => {
    const apps = [
      { name: "Alpha", favourite: false },
      { name: "Zeta", favourite: true },
      { name: "Beta", favourite: false },
      { name: "Mu", favourite: true },
    ];
    expect(sortLibraryApps(apps).map((a) => a.name)).toEqual(["Mu", "Zeta", "Alpha", "Beta"]);
  });

  it("sorts by name within each group and treats an absent flag as not-favourited", () => {
    const apps = [{ name: "Delta" }, { name: "Charlie" }, { name: "Echo", favourite: true }];
    expect(sortLibraryApps(apps).map((a) => a.name)).toEqual(["Echo", "Charlie", "Delta"]);
  });

  it("does not mutate its input — the caller keeps a separate name-only order", () => {
    const apps = [{ name: "Alpha", favourite: false }, { name: "Zeta", favourite: true }];
    const out = sortLibraryApps(apps);
    expect(apps.map((a) => a.name)).toEqual(["Alpha", "Zeta"]);
    expect(out.map((a) => a.name)).toEqual(["Zeta", "Alpha"]);
  });
});

describe("row computation (offsetTop-based, column-count-agnostic)", () => {
  // 7 tiles at 4 columns wide: rows of [0,1,2,3], [4,5,6]
  const fourCol = [0, 0, 0, 0, 220, 220, 220];
  // Same 7 tiles at 3 columns wide: rows of [0,1,2],[3,4,5],[6] — the last row
  // is a partial row of 1, the case most likely to break a row calculation.
  const threeCol = [0, 0, 0, 220, 220, 220, 440];
  // Same 7 tiles at 2 columns wide: rows of [0,1],[2,3],[4,5],[6]
  const twoCol = [0, 0, 220, 220, 440, 440, 660];
  // Same 7 tiles at 1 column wide: every tile its own row
  const oneCol = [0, 220, 440, 660, 880, 1100, 1320];

  it("finds the last tile in the clicked tile's row at 4 columns", () => {
    expect(lastIndexInRow(fourCol, 1)).toBe(3); // click index 1 -> row ends at 3
    expect(lastIndexInRow(fourCol, 5)).toBe(6); // second row
  });

  it("finds the last tile in the clicked tile's row at 3 columns, including a final partial row", () => {
    expect(lastIndexInRow(threeCol, 1)).toBe(2); // first full row
    expect(lastIndexInRow(threeCol, 4)).toBe(5); // second full row
    expect(lastIndexInRow(threeCol, 6)).toBe(6); // final partial row of 1
  });

  it("finds the last tile in the clicked tile's row at 2 columns", () => {
    expect(lastIndexInRow(twoCol, 0)).toBe(1);
    expect(lastIndexInRow(twoCol, 3)).toBe(3);
    expect(lastIndexInRow(twoCol, 6)).toBe(6);
  });

  it("finds the last tile in the clicked tile's row at 1 column (every row length 1)", () => {
    expect(lastIndexInRow(oneCol, 4)).toBe(4);
  });

  it("rowSize reads the first row's tile count, whatever the column count is", () => {
    expect(rowSize(fourCol)).toBe(4);
    expect(rowSize(threeCol)).toBe(3);
    expect(rowSize(twoCol)).toBe(2);
    expect(rowSize(oneCol)).toBe(1);
  });

  it("rowSize of an empty grid is 0", () => {
    expect(rowSize([])).toBe(0);
  });
});

describe("nextTileIndex (grid-aware roving focus)", () => {
  const fourCol = [0, 0, 0, 0, 220, 220, 220];

  it("ArrowRight/ArrowLeft walk the flat list", () => {
    expect(nextTileIndex(fourCol, 1, "ArrowRight")).toBe(2);
    expect(nextTileIndex(fourCol, 1, "ArrowLeft")).toBe(0);
  });

  it("ArrowDown/ArrowUp jump by the row size", () => {
    expect(nextTileIndex(fourCol, 1, "ArrowDown")).toBe(5);
    expect(nextTileIndex(fourCol, 5, "ArrowUp")).toBe(1);
  });

  it("returns null rather than going out of bounds", () => {
    expect(nextTileIndex(fourCol, 0, "ArrowLeft")).toBeNull();
    expect(nextTileIndex(fourCol, 6, "ArrowRight")).toBeNull();
    expect(nextTileIndex(fourCol, 6, "ArrowDown")).toBeNull();
    expect(nextTileIndex(fourCol, 0, "ArrowUp")).toBeNull();
  });

  it("returns null on an empty grid rather than dividing by zero weirdness", () => {
    expect(nextTileIndex([], 0, "ArrowDown")).toBeNull();
  });
});

describe("formatElapsedMinutes", () => {
  const now = new Date("2026-07-27T12:00:00Z").getTime();

  it("formats under an hour as N minutes", () => {
    expect(formatElapsedMinutes(new Date(now - 3 * 60_000).toISOString(), now)).toBe("3 minutes");
  });

  it("uses singular for exactly one minute", () => {
    expect(formatElapsedMinutes(new Date(now - 60_000).toISOString(), now)).toBe("1 minute");
  });

  it("formats an hour or more as Nh Nm", () => {
    expect(formatElapsedMinutes(new Date(now - 72 * 60_000).toISOString(), now)).toBe("1h 12m");
  });

  it("clamps negative elapsed (clock skew) to zero", () => {
    expect(formatElapsedMinutes(new Date(now + 60_000).toISOString(), now)).toBe("0 minutes");
  });
});

// steam-library-discovery spec §2.2: a derived tile borrows its parent's home,
// so the single-writer lock is one lock per FAMILY, not per tile. These are the
// pure halves of the blocked-tile presentation (AppHome renders the marker/note
// off blockedFamilyIds and names the family off familyRootApp).
describe("homeAppId / blockedFamilyIds / familyRootApp (single-writer lock UX)", () => {
  const steam = { id: "steam", name: "Steam", parent_app_id: null };
  const gameA = { id: "game-a", name: "Half-Life", parent_app_id: "steam" };
  const gameB = { id: "game-b", name: "Portal 2", parent_app_id: "steam" };
  const unrelated = { id: "blender", name: "Blender", parent_app_id: null };
  const family = [steam, gameA, gameB, unrelated];

  it("homeAppId is the app itself for a normal app (including the Launcher tile)", () => {
    expect(homeAppId(steam)).toBe("steam");
    expect(homeAppId(unrelated)).toBe("blender");
  });

  it("homeAppId is the parent for a derived tile", () => {
    expect(homeAppId(gameA)).toBe("steam");
    expect(homeAppId(gameB)).toBe("steam");
  });

  it("with the Launcher (parent) live, every derived tile is blocked — and nothing else", () => {
    const blocked = blockedFamilyIds(family, "steam");
    expect([...blocked].sort()).toEqual(["game-a", "game-b"]);
  });

  it("with a derived tile live, the Launcher tile AND its siblings are blocked", () => {
    const blocked = blockedFamilyIds(family, "game-a");
    expect([...blocked].sort()).toEqual(["game-b", "steam"]);
  });

  it("the live app itself is never in its own blocked set", () => {
    expect(blockedFamilyIds(family, "game-a").has("game-a")).toBe(false);
  });

  it("an unrelated app is never blocked by a different family's live session", () => {
    expect(blockedFamilyIds(family, "steam").has("blender")).toBe(false);
  });

  it("returns empty when nothing is live", () => {
    expect(blockedFamilyIds(family, null).size).toBe(0);
  });

  it("returns empty when the live app id isn't in the list (stale data)", () => {
    expect(blockedFamilyIds(family, "unknown").size).toBe(0);
  });

  it("familyRootApp resolves a derived tile's parent, and a normal app's self", () => {
    expect(familyRootApp(family, gameA)?.id).toBe("steam");
    expect(familyRootApp(family, steam)?.id).toBe("steam");
  });

  it("familyRootApp is null when the parent isn't in the list", () => {
    const orphan = { id: "orphan", name: "Orphan", parent_app_id: "missing-parent" };
    expect(familyRootApp(family, orphan)).toBeNull();
  });
});

// steam-library-discovery spec §2.2: the two named 409s get named, actionable
// copy — never a generic "Launch failed" — and the copy names the LAUNCHER
// TILE (its family root's name, e.g. "Steam"), never a bare parent app id.
describe("presentLaunchError", () => {
  const steam = { id: "steam", name: "Steam", parent_app_id: null };
  const gameA = { id: "game-a", name: "Half-Life", parent_app_id: "steam" };
  const family = [steam, gameA];

  it("home_in_use names the family's Launcher tile and carries the session id", () => {
    const result = presentLaunchError(family, gameA, "home_in_use", "Steam is already running", "sess-1");
    expect(result.variant).toBe("danger");
    expect(result.title).toBe("Steam is already running");
    expect(result.sessionId).toBe("sess-1");
  });

  it("home_in_use on the Launcher tile itself still names Steam, not its own id", () => {
    const result = presentLaunchError(family, steam, "home_in_use", "conflict", "sess-2");
    expect(result.title).toBe("Steam is already running");
  });

  it("home_in_use omits sessionId when the server didn't name a conflicting session", () => {
    const result = presentLaunchError(family, gameA, "home_in_use", "conflict");
    expect(result.sessionId).toBeUndefined();
  });

  it("home_not_provisioned is rendered as guidance, not a failure", () => {
    const result = presentLaunchError(family, gameA, "home_not_provisioned", "no home");
    expect(result.variant).toBe("info");
    expect(result.body).toContain("Steam");
    expect(result.body).not.toContain("failed");
  });

  it("falls back to the generic message for anything else", () => {
    const result = presentLaunchError(family, gameA, "profile_ineligible", "not eligible on this device");
    expect(result.title).toBe("Launch failed");
    expect(result.body).toBe("not eligible on this device");
  });

  it("still renders the pre-existing capacity_unavailable copy", () => {
    const result = presentLaunchError(family, gameA, "capacity_unavailable", "raw message");
    expect(result.body).toBe("No host available right now — try again shortly.");
  });
});

describe("formatRelativeTime", () => {
  const now = new Date("2026-07-27T12:00:00Z").getTime();

  it("formats sub-minute as just now", () => {
    expect(formatRelativeTime(new Date(now - 10_000).toISOString(), now)).toBe("just now");
  });

  it("formats minutes/hours/days with correct pluralisation", () => {
    expect(formatRelativeTime(new Date(now - 2 * 60_000).toISOString(), now)).toBe("2 minutes ago");
    expect(formatRelativeTime(new Date(now - 60_000).toISOString(), now)).toBe("1 minute ago");
    expect(formatRelativeTime(new Date(now - 3 * 3_600_000).toISOString(), now)).toBe("3 hours ago");
    expect(formatRelativeTime(new Date(now - 3_600_000).toISOString(), now)).toBe("1 hour ago");
    expect(formatRelativeTime(new Date(now - 2 * 86_400_000).toISOString(), now)).toBe("2 days ago");
    expect(formatRelativeTime(new Date(now - 86_400_000).toISOString(), now)).toBe("1 day ago");
  });
});
