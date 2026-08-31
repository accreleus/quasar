import { describe, expect, it } from "vitest";

import type { AdminSession } from "../../../api/types";
import { filterSessions, segmentCounts } from "./sessionFilters";

function makeSession(overrides: Partial<AdminSession>): AdminSession {
  return {
    id: "id",
    user_id: "user",
    app_id: "app",
    host_id: "host-1",
    state: "running",
    state_detail: null,
    created_at: "2024-01-01T00:00:00Z",
    started_at: null,
    ended_at: null,
    ...overrides,
  } as AdminSession;
}

const ROWS: AdminSession[] = [
  makeSession({ id: "running", state: "running", username: "ada", app_name: "Hades II", host_name: "quasar-node-1", host_id: "h1" }),
  makeSession({ id: "starting", state: "starting", username: "bob", app_name: "Blender", host_name: "quasar-node-2", host_id: "h2" }),
  makeSession({ id: "stopped", state: "stopped", username: "cy", app_name: "Steam", host_name: "quasar-node-1", host_id: "h1" }),
  makeSession({ id: "failed", state: "failed", username: "dee", app_name: "Diablo IV", host_name: "quasar-node-2", host_id: "h2" }),
];

const FAILED_H1 = makeSession({ id: "failed-h1", state: "failed", username: "eve", app_name: "Steam", host_name: "quasar-node-1", host_id: "h1" });

const ALL = { segment: "all" as const, q: "", hostId: "" };

describe("filterSessions — segments", () => {
  it("passes everything through on 'all'", () => {
    expect(filterSessions(ROWS, ALL)).toHaveLength(4);
  });

  it("counts every non-terminal state as live, not just 'running'", () => {
    // The load-bearing case: a session still starting is holding a slot, so the
    // Live segment must show it. A `state === "running"` rule would hide it.
    const live = filterSessions(ROWS, { ...ALL, segment: "live" }).map((s) => s.id);
    expect(live).toEqual(["running", "starting"]);
  });

  it("leaves 'failed' to the wire rather than re-deriving it from `state`", () => {
    // `state=failed` is the server's classification; repeating it here would be
    // a second definition that drops any terminal failure state added later.
    expect(filterSessions(ROWS, { ...ALL, segment: "failed" })).toHaveLength(4);
  });
});

describe("filterSessions — search", () => {
  it("matches user, app and host, case-insensitively", () => {
    expect(filterSessions(ROWS, { ...ALL, q: "ADA" }).map((s) => s.id)).toEqual(["running"]);
    expect(filterSessions(ROWS, { ...ALL, q: "blender" }).map((s) => s.id)).toEqual(["starting"]);
    expect(filterSessions(ROWS, { ...ALL, q: "node-2" }).map((s) => s.id)).toEqual([
      "starting",
      "failed",
    ]);
  });

  it("ignores surrounding whitespace, and an all-whitespace query filters nothing", () => {
    expect(filterSessions(ROWS, { ...ALL, q: "  ada  " }).map((s) => s.id)).toEqual(["running"]);
    expect(filterSessions(ROWS, { ...ALL, q: "   " })).toHaveLength(4);
  });

  it("does not match on ids — the box promises user, app or host", () => {
    expect(filterSessions(ROWS, { ...ALL, q: "h1" })).toHaveLength(0);
  });

  it("survives rows whose user, app or host row is gone", () => {
    const orphan = [makeSession({ id: "orphan", username: undefined, app_name: undefined, host_name: undefined })];
    expect(filterSessions(orphan, { ...ALL, q: "anything" })).toHaveLength(0);
    expect(filterSessions(orphan, ALL)).toHaveLength(1);
  });
});

describe("filterSessions — host", () => {
  it("matches a host id exactly", () => {
    expect(filterSessions(ROWS, { ...ALL, hostId: "h1" }).map((s) => s.id)).toEqual([
      "running",
      "stopped",
    ]);
  });

  it("excludes an unplaced session from any specific host, but not from 'all hosts'", () => {
    const rows = [...ROWS, makeSession({ id: "unplaced", host_id: null, host_name: undefined })];
    expect(filterSessions(rows, { ...ALL, hostId: "h1" }).map((s) => s.id)).toEqual([
      "running",
      "stopped",
    ]);
    expect(filterSessions(rows, ALL)).toHaveLength(5);
  });

  it("composes with the segment and the search box", () => {
    const rows = filterSessions(ROWS, { segment: "live", q: "quasar", hostId: "h2" });
    expect(rows.map((s) => s.id)).toEqual(["starting"]);
  });

  it("narrows a wire-filtered 'failed' page by host and text all the same", () => {
    const failures = [FAILED_H1, ROWS[3]];
    expect(filterSessions(failures, { segment: "failed", q: "", hostId: "h2" }).map((s) => s.id))
      .toEqual(["failed"]);
  });
});

describe("segmentCounts", () => {
  it("counts each segment over the unfiltered rows", () => {
    expect(segmentCounts(ROWS)).toEqual({ all: 4, live: 2, failed: 1 });
  });

  it("agrees with filterSessions on the segments it narrows", () => {
    const counts = segmentCounts(ROWS);
    expect(counts.all).toBe(filterSessions(ROWS, { ...ALL, segment: "all" }).length);
    expect(counts.live).toBe(filterSessions(ROWS, { ...ALL, segment: "live" }).length);
  });

  it("still counts failures, since the count is taken over an unfiltered read", () => {
    expect(segmentCounts(ROWS).failed).toBe(1);
  });

  it("is all zeroes for no rows", () => {
    expect(segmentCounts([])).toEqual({ all: 0, live: 0, failed: 0 });
  });
});
