// Source grouping for the Library view (v3 handoff §B "Library view").
//
// Fixture names are the real wire names. The plan's sketch used `origin`,
// `provider` and `parent_id`; `AppListItem` (api/schema.d.ts) has none of
// those on the user read shape. What it has, and what this module reads, is:
//   · `kind`            — "game" | "desktop" | "launcher"
//   · `parent_app_id`   — the provider tile this one was discovered from
//   · `external_source` — "" | "steam", the provider that owns the title
// `origin` and `library_provider` are admin-only (control-api.md §Derived
// tiles), and there is no host name on any user-visible shape — `GET
// /v1/hosts` is admin-only — so the mock's "on host-01" half of the sub-line
// has no source to come from and is not invented.

import { describe, expect, it } from "vitest";
import { groupBySource, type GroupableApp } from "./librarySources";

const apps: GroupableApp[] = [
  { id: "1", name: "Steam", kind: "launcher", parent_app_id: null, external_source: "" },
  { id: "2", name: "Hades", kind: "game", parent_app_id: "1", external_source: "steam" },
  { id: "3", name: "Blender", kind: "desktop", parent_app_id: null, external_source: "" },
  { id: "4", name: "Doom", kind: "game", parent_app_id: null, external_source: "" },
];

describe("groupBySource", () => {
  it("groups discovered tiles under their provider, desktops together, the rest as Manual", () => {
    const g = groupBySource(apps);
    expect(g.map((s) => s.title)).toEqual(["Steam", "Manual", "Desktops"]);
    expect(g[0].sub).toBe("via Steam");
    expect(g[0].apps.map((a) => a.id)).toEqual(["2"]);
    expect(g[2].apps.map((a) => a.id)).toEqual(["3"]);
  });

  it("files the provider tile itself as Manual — it is the app an admin added", () => {
    // The Steam client is a hand-added launcher; only the titles discovered
    // through it are the provider's.
    const g = groupBySource(apps);
    expect(g[1].title).toBe("Manual");
    expect(g[1].apps.map((a) => a.id)).toEqual(["1", "4"]);
  });

  it("keeps a provider-tagged app with its provider even without a parent tile", () => {
    // `external_source` alone is enough: a title tagged "steam" whose parent
    // launcher the caller cannot see must not fall out into Manual.
    const g = groupBySource([
      { id: "9", name: "Portal 2", kind: "game", parent_app_id: null, external_source: "steam" },
      { id: "2", name: "Hades", kind: "game", parent_app_id: "1", external_source: "steam" },
      { id: "1", name: "Steam", kind: "launcher", parent_app_id: null, external_source: "" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Steam", "Manual"]);
    expect(g[0].apps.map((a) => a.id)).toEqual(["9", "2"]);
  });

  it("degrades an unknown provider id to a readable title rather than dropping it", () => {
    const g = groupBySource([
      { id: "1", name: "Tyrian", kind: "game", parent_app_id: null, external_source: "gog" },
      { id: "2", name: "Doom", kind: "game", parent_app_id: null, external_source: "" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Gog", "Manual"]);
    expect(g[0].sub).toBe("via Gog");
  });

  it("names a group after the parent tile when the tile carries no provider id", () => {
    const g = groupBySource([
      { id: "1", name: "RetroArch", kind: "launcher", parent_app_id: null, external_source: "" },
      { id: "2", name: "Super Metroid", kind: "game", parent_app_id: "1", external_source: "" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["RetroArch", "Manual"]);
    expect(g[0].sub).toBe("via RetroArch");
    expect(g[0].apps.map((a) => a.id)).toEqual(["2"]);
  });

  it("falls back to the provider id when the parent tile is not in the catalogue", () => {
    // Entitlement drift: the user holds a derived tile but not its parent.
    const g = groupBySource([
      { id: "2", name: "Hades", kind: "game", parent_app_id: "gone", external_source: "steam" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Steam"]);
  });

  it("falls back to Manual when neither the parent tile nor a provider id is there", () => {
    // Both halves missing: an untitled group headed "" is worse than filing the
    // tile with the hand-added ones.
    const g = groupBySource([
      { id: "2", name: "Hades", kind: "game", parent_app_id: "gone", external_source: "" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Manual"]);
    expect(g[0].sub).toBe("Added to the catalogue directly");
    expect(g[0].apps.map((a) => a.id)).toEqual(["2"]);
  });

  it("buckets by WHAT a group is, not by what it is called", () => {
    // A provider genuinely named "Manual" or "Desktops" must still sort and read
    // as a provider — the rank is the bucket, never the title string.
    const g = groupBySource([
      { id: "1", name: "Doom", kind: "game", parent_app_id: null, external_source: "" },
      { id: "2", name: "Weston", kind: "desktop", parent_app_id: null, external_source: "" },
      { id: "3", name: "Zork", kind: "game", parent_app_id: null, external_source: "desktops" },
      { id: "4", name: "Tyrian", kind: "game", parent_app_id: null, external_source: "manual" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Desktops", "Manual", "Manual", "Desktops"]);
    expect(g.map((s) => s.id)).toEqual([
      "src-provider-desktops",
      "src-provider-manual",
      "src-manual",
      "src-desktops",
    ]);
    expect(g.map((s) => s.sub)).toEqual([
      "via Desktops",
      "via Manual",
      "Added to the catalogue directly",
      "Full desktop environments",
    ]);
    expect(g.map((s) => s.apps.map((a) => a.id))).toEqual([["3"], ["4"], ["1"], ["2"]]);
  });

  it("files a desktop by kind even when it carries a provider id", () => {
    const g = groupBySource([
      { id: "1", name: "Weston", kind: "desktop", parent_app_id: null, external_source: "steam" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Desktops"]);
    expect(g[0].sub).toBe("Full desktop environments");
  });

  it("orders providers alphabetically, then Manual, then Desktops, and emits no empty group", () => {
    const g = groupBySource([
      { id: "1", name: "Weston", kind: "desktop", parent_app_id: null, external_source: "" },
      { id: "2", name: "Doom", kind: "game", parent_app_id: null, external_source: "" },
      { id: "3", name: "Tyrian", kind: "game", parent_app_id: null, external_source: "zog" },
      { id: "4", name: "Hades", kind: "game", parent_app_id: null, external_source: "amber" },
    ]);
    expect(g.map((s) => s.title)).toEqual(["Amber", "Zog", "Manual", "Desktops"]);
    expect(g.every((s) => s.apps.length > 0)).toBe(true);
  });

  it("gives every group a stable, DOM-safe id", () => {
    expect(groupBySource(apps).map((s) => s.id)).toEqual([
      "src-provider-steam",
      "src-manual",
      "src-desktops",
    ]);
  });

  it("returns nothing for an empty catalogue", () => {
    expect(groupBySource([])).toEqual([]);
  });

  it("preserves the caller's order inside a group", () => {
    const g = groupBySource([
      { id: "b", name: "B", kind: "game", parent_app_id: null, external_source: "" },
      { id: "a", name: "A", kind: "game", parent_app_id: null, external_source: "" },
    ]);
    expect(g[0].apps.map((a) => a.id)).toEqual(["b", "a"]);
  });
});
