import { describe, expect, it } from "vitest";
import type { AdminActivityItem } from "../../../api/admin";
import { groupByDay } from "./dayGroups";

function item(id: number, created_at: string): AdminActivityItem {
  return {
    id,
    actor_user_id: "u-1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: "h-1",
    details: {},
    created_at,
    severity: "info",
  };
}

// A Saturday, local time (no explicit TZ so tests run in whatever TZ CI uses —
// the point under test is the day-boundary maths, not a specific offset).
const NOW = new Date(2026, 7, 8, 14, 0, 0); // 8 August 2026, a Saturday

describe("groupByDay", () => {
  it("labels the current local day Today", () => {
    const groups = groupByDay([item(1, new Date(2026, 7, 8, 9, 0, 0).toISOString())], NOW);
    expect(groups).toHaveLength(1);
    expect(groups[0].label).toBe("Today · 8 August 2026");
  });

  it("labels the previous local day Yesterday", () => {
    const groups = groupByDay([item(1, new Date(2026, 7, 7, 23, 0, 0).toISOString())], NOW);
    expect(groups[0].label).toBe("Yesterday · 7 August 2026");
  });

  it("labels an older day by weekday", () => {
    // 3 days before the Saturday NOW is a Wednesday.
    const groups = groupByDay([item(1, new Date(2026, 7, 5, 10, 0, 0).toISOString())], NOW);
    expect(groups[0].label).toBe("Wednesday · 5 August 2026");
  });

  it("groups multiple rows on the same local day into one card", () => {
    const rows = [
      item(1, new Date(2026, 7, 8, 9, 0, 0).toISOString()),
      item(2, new Date(2026, 7, 8, 13, 30, 0).toISOString()),
    ];
    const groups = groupByDay(rows, NOW);
    expect(groups).toHaveLength(1);
    expect(groups[0].items.map((i) => i.id)).toEqual([1, 2]);
  });

  it("orders groups newest day first", () => {
    const rows = [
      item(1, new Date(2026, 7, 5, 10, 0, 0).toISOString()),
      item(2, new Date(2026, 7, 8, 9, 0, 0).toISOString()),
      item(3, new Date(2026, 7, 7, 23, 0, 0).toISOString()),
    ];
    const groups = groupByDay(rows, NOW);
    expect(groups.map((g) => g.label)).toEqual([
      "Today · 8 August 2026",
      "Yesterday · 7 August 2026",
      "Wednesday · 5 August 2026",
    ]);
  });

  it("returns no groups for no rows", () => {
    expect(groupByDay([], NOW)).toEqual([]);
  });
});
