import { describe, expect, it } from "vitest";
import type { AdminActivityItem } from "../../../api/admin";
import {
  actorLabel,
  applySegment,
  queryFor,
  segmentCounts,
  segmentPredicate,
  sinceFor,
  targetLabel,
} from "./auditFilters";

function item(over: Partial<AdminActivityItem> = {}): AdminActivityItem {
  return {
    id: 1,
    actor_user_id: "u-1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: "h-1234567890",
    details: {},
    created_at: "2026-08-08T14:02:11Z",
    severity: "info",
    ...over,
  };
}

describe("sinceFor", () => {
  const now = new Date("2026-08-08T14:00:00Z");

  it("24h is 24 hours back", () => {
    expect(sinceFor("24h", now)).toBe("2026-08-07T14:00:00.000Z");
  });

  it("7d is 7 days back", () => {
    expect(sinceFor("7d", now)).toBe("2026-08-01T14:00:00.000Z");
  });

  it("30d is 30 days back", () => {
    expect(sinceFor("30d", now)).toBe("2026-07-09T14:00:00.000Z");
  });
});

describe("queryFor", () => {
  const now = new Date("2026-08-08T14:00:00Z");

  it("sends since and q — takes no segment; segment never reaches the server", () => {
    const query = queryFor({ q: "drain", range: "24h" }, now);
    expect(query).toEqual({ since: "2026-08-07T14:00:00.000Z", q: "drain" });
  });

  it("omits q when blank or whitespace-only", () => {
    expect(queryFor({ q: "   ", range: "24h" }, now).q).toBeUndefined();
  });

  it("reuses the same `now` across two calls — the caller's stability contract", () => {
    const first = queryFor({ q: "", range: "24h" }, now);
    const second = queryFor({ q: "", range: "24h" }, now);
    expect(first.since).toBe(second.since);
  });
});

describe("segmentPredicate / applySegment", () => {
  const withActor = item({ actor_user_id: "u-1", actor_username: "salty2011" });
  const system = item({ id: 2, actor_user_id: null, actor_username: null, severity: "err" });
  const warnRow = item({ id: 3, severity: "warn" });
  const rows = [withActor, system, warnRow];

  it("all keeps everything", () => {
    expect(applySegment(rows, "all")).toEqual(rows);
  });

  it("operator keeps rows with a non-null actor_user_id", () => {
    expect(applySegment(rows, "operator")).toEqual([withActor, warnRow]);
  });

  it("system keeps rows with a null actor_user_id", () => {
    expect(applySegment(rows, "system")).toEqual([system]);
  });

  it("errors keeps severity === err", () => {
    expect(applySegment(rows, "errors")).toEqual([system]);
  });

  it("segmentPredicate is the same rule applySegment uses", () => {
    expect(rows.filter(segmentPredicate("system"))).toEqual(applySegment(rows, "system"));
  });
});

describe("segmentCounts", () => {
  it("counts each segment over the given rows, independent of any active filter", () => {
    const rows = [
      item({ id: 1, actor_user_id: "u-1", severity: "info" }),
      item({ id: 2, actor_user_id: null, actor_username: null, severity: "err" }),
      item({ id: 3, actor_user_id: null, actor_username: null, severity: "warn" }),
    ];
    expect(segmentCounts(rows)).toEqual({ all: 3, operator: 1, system: 2, errors: 1 });
  });
});

describe("actorLabel", () => {
  it("uses actor_username when present", () => {
    expect(actorLabel({ actor_username: "priya" })).toBe("priya");
  });

  it("falls back to system", () => {
    expect(actorLabel({ actor_username: null })).toBe("system");
  });
});

describe("targetLabel", () => {
  it("is type + short id when an id is present", () => {
    expect(targetLabel({ target_type: "host", target_id: "h-1234567890" })).toBe("host h-123456");
  });

  it("is bare type when there is no id", () => {
    expect(targetLabel({ target_type: "instance", target_id: null })).toBe("instance");
  });
});
