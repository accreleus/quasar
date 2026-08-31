import { describe, expect, it } from "vitest";
import type { AdminApp, LibraryUnpublishedItem } from "../../../api/types";
import {
  filterApps,
  isSteamSourced,
  segmentCounts,
  type PendingImportItem,
} from "./appsFilters";

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "app-1",
    name: "Half-Life 2",
    description: "",
    cover_url: null,
    hero_url: null,
    kind: "game",
    parent_app_id: null,
    external_source: "",
    external_id: "",
    origin: "manual",
    library_provider: "",
    library_discovery_suspended: false,
    favourite: false,
    default_width: 1920,
    default_height: 1080,
    default_fps: 60,
    default_bitrate_kbps: 8000,
    default_profile_id: null,
    profile_policy: "inherit",
    enabled: true,
    default_vram_mb: 0,
    default_encode_slots: 1,
    runtime_spec: {},
    managed_home: false,
    home_container_path: "",
    runtime_preset_id: null,
    launchable_profile_ids: [],
    sessions_30d: 0,
    ...over,
  } as AdminApp;
}

function pendingItem(over: Partial<LibraryUnpublishedItem> = {}): PendingImportItem {
  return {
    providerAppId: "provider-1",
    item: {
      external_source: "steam",
      external_id: "228980",
      name: "Steamworks Common Redistributables",
      suppressed_by: "other",
      users: 3,
      last_seen_at: "2026-08-01T00:00:00Z",
      has_tile: false,
      ...over,
    },
  };
}

describe("isSteamSourced", () => {
  it("is true for a derived tile (parent_app_id set)", () => {
    expect(isSteamSourced(app({ parent_app_id: "provider-1" }))).toBe(true);
  });

  it("is true for the provider app itself (external_source set)", () => {
    expect(isSteamSourced(app({ external_source: "steam" }))).toBe(true);
  });

  it("is false for a manually created app", () => {
    expect(isSteamSourced(app())).toBe(false);
  });
});

describe("filterApps — segments", () => {
  const games = app({ id: "g1", kind: "game" });
  const desktops = app({ id: "d1", kind: "desktop" });
  const disabled = app({ id: "x1", kind: "launcher", enabled: false });
  const apps = [games, desktops, disabled];

  it("all: keeps every app", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "all", q: "", source: "all" });
    expect(rows).toHaveLength(3);
  });

  it("games: keeps only kind=game", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "games", q: "", source: "all" });
    expect(rows).toEqual([games]);
  });

  it("desktops: keeps only kind=desktop", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "desktops", q: "", source: "all" });
    expect(rows).toEqual([desktops]);
  });

  it("disabled: keeps only !enabled", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "disabled", q: "", source: "all" });
    expect(rows).toEqual([disabled]);
  });

  it("pending: excludes every real app row", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "pending", q: "", source: "all" });
    expect(rows).toHaveLength(0);
  });
});

describe("filterApps — pending rows", () => {
  const pending = [pendingItem()];

  it("shown under the all segment", () => {
    const { pending: rows } = filterApps([], pending, { segment: "all", q: "", source: "all" });
    expect(rows).toEqual(pending);
  });

  it("shown under the pending segment", () => {
    const { pending: rows } = filterApps([], pending, { segment: "pending", q: "", source: "all" });
    expect(rows).toEqual(pending);
  });

  it("shown under games too — a pending item renders with a Game chip", () => {
    const { pending: rows } = filterApps([], pending, { segment: "games", q: "", source: "all" });
    expect(rows).toEqual(pending);
  });

  it("hidden under desktops/disabled — a pending item has no kind or enabled state", () => {
    for (const segment of ["desktops", "disabled"] as const) {
      const { pending: rows } = filterApps([], pending, { segment, q: "", source: "all" });
      expect(rows).toHaveLength(0);
    }
  });

  it("hidden once a preset filter is active", () => {
    const { pending: rows } = filterApps([], pending, {
      segment: "all",
      q: "",
      source: "all",
      presetId: "preset-1",
    });
    expect(rows).toHaveLength(0);
  });

  it("hidden under the Manual source filter — a pending item is always Steam", () => {
    const { pending: rows } = filterApps([], pending, { segment: "all", q: "", source: "manual" });
    expect(rows).toHaveLength(0);
  });

  it("kept under the Steam source filter", () => {
    const { pending: rows } = filterApps([], pending, { segment: "all", q: "", source: "steam" });
    expect(rows).toEqual(pending);
  });
});

describe("filterApps — search", () => {
  it("matches an app by name, case-insensitively", () => {
    const apps = [app({ id: "a1", name: "Stardew Valley" }), app({ id: "a2", name: "Portal 2" })];
    const { apps: rows } = filterApps(apps, [], { segment: "all", q: "stardew", source: "all" });
    expect(rows.map((a) => a.id)).toEqual(["a1"]);
  });

  it("matches a pending item by name", () => {
    const pending = [pendingItem({ name: "Deep Rock Galactic" }), pendingItem({ name: "Satisfactory" })];
    const { pending: rows } = filterApps([], pending, { segment: "all", q: "deep rock", source: "all" });
    expect(rows).toHaveLength(1);
    expect(rows[0].item.name).toBe("Deep Rock Galactic");
  });

  it("falls back to the external id when a pending item has no name", () => {
    const pending = [pendingItem({ name: "", external_id: "228980" })];
    const { pending: rows } = filterApps([], pending, { segment: "all", q: "228980", source: "all" });
    expect(rows).toHaveLength(1);
  });
});

describe("filterApps — source", () => {
  const manual = app({ id: "m1" });
  const steamTile = app({ id: "s1", parent_app_id: "provider-1" });
  const apps = [manual, steamTile];

  it("steam: keeps only provider-sourced apps", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "all", q: "", source: "steam" });
    expect(rows).toEqual([steamTile]);
  });

  it("manual: keeps only apps with no provider marker", () => {
    const { apps: rows } = filterApps(apps, [], { segment: "all", q: "", source: "manual" });
    expect(rows).toEqual([manual]);
  });
});

describe("filterApps — preset", () => {
  it("keeps only apps inheriting the given runtime preset", () => {
    const a = app({ id: "a1", runtime_preset_id: "preset-1" });
    const b = app({ id: "b1", runtime_preset_id: "preset-2" });
    const { apps: rows } = filterApps([a, b], [], {
      segment: "all",
      q: "",
      source: "all",
      presetId: "preset-1",
    });
    expect(rows).toEqual([a]);
  });
});

describe("segmentCounts", () => {
  it("counts each segment against the full lists, not a filtered result", () => {
    const apps = [
      app({ id: "g1", kind: "game" }),
      app({ id: "d1", kind: "desktop" }),
      app({ id: "x1", kind: "launcher", enabled: false }),
    ];
    const counts = segmentCounts(apps, [pendingItem(), pendingItem({ external_id: "1" })]);
    expect(counts).toEqual({ all: 3, games: 1, desktops: 1, disabled: 1, pending: 2 });
  });
});
