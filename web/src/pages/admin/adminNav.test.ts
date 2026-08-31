// The v3 rail is a flat eight-row list (spec §A.0). What would rot silently:
// the order (it is the operator's visit frequency, not alphabetical), and the
// `match` predicates — a drill-down route that lights two rows, or none, is
// how a rail stops telling you where you are.

import { describe, expect, it } from "vitest";
import { ADMIN_NAV } from "./adminNav";

describe("ADMIN_NAV", () => {
  it("is the eight v3 destinations, in order", () => {
    expect(ADMIN_NAV.map((i) => i.id)).toEqual([
      "overview",
      "sessions",
      "fleet",
      "library",
      "streaming",
      "people",
      "audit",
      "settings",
    ]);
    expect(ADMIN_NAV.map((i) => i.label)).toEqual([
      "Overview",
      "Sessions",
      "Fleet",
      "Library",
      "Streaming",
      "People",
      "Audit log",
      "Settings",
    ]);
  });

  it("carries the two badges the mock has, and no others", () => {
    expect(ADMIN_NAV.filter((i) => i.badge).map((i) => [i.id, i.badge])).toEqual([
      ["sessions", "live"],
      ["fleet", "fault"],
    ]);
  });

  it("routes every row at a /admin path and gives each one a glyph", () => {
    for (const item of ADMIN_NAV) {
      expect(item.to.startsWith("/admin")).toBe(true);
      expect(item.icon).toBeTruthy();
    }
  });

  it("lights exactly one row for a deep drill-down", () => {
    const lit = ADMIN_NAV.filter((i) => i.match("/admin/fleet/hosts/abc/settings"));
    expect(lit.map((i) => i.id)).toEqual(["fleet"]);
  });

  it("lights exactly one row for every rail destination", () => {
    for (const item of ADMIN_NAV) {
      expect(ADMIN_NAV.filter((i) => i.match(item.to)).map((i) => i.id)).toEqual([item.id]);
    }
  });

  it("keeps Overview an exact match — /admin prefixes every admin route", () => {
    const overview = ADMIN_NAV[0];
    expect(overview.match("/admin")).toBe(true);
    expect(overview.match("/admin/sessions")).toBe(false);
  });
});
