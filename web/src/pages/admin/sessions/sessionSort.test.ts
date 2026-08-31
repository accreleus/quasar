import { describe, expect, it } from "vitest";
import { sortSessions } from "./sessionSort";
import type { AdminSession } from "../../../api/types";

function makeSession(overrides: Partial<AdminSession>): AdminSession {
  return {
    id: "id",
    user_id: "user",
    app_id: "app",
    host_id: null,
    state: "running",
    state_detail: null,
    created_at: "2024-01-01T00:00:00Z",
    started_at: null,
    ended_at: null,
    ...overrides,
  } as AdminSession;
}

// #385 item 7: the User cell now stacks `username` under `user_id`, and the sort
// follows what is displayed (see userSortValue). These fixtures therefore carry a
// username whose alphabetical order is the REVERSE of the user_id order — so a
// test that passes could not be passing by accident on the old id key.
const ROWS: AdminSession[] = [
  makeSession({ id: "b", user_id: "bravo", username: "yankee", state: "pending", started_at: "2024-01-01T10:30:00Z" }),
  makeSession({ id: "a", user_id: "alpha", username: "zulu", state: "running", started_at: "2024-01-01T09:00:00Z" }),
  makeSession({ id: "c", user_id: "charlie", username: "xray", state: "failed", started_at: "2024-01-01T12:00:00Z" }),
];

describe("sortSessions", () => {
  it("returns the original order when key is null (no sort selected)", () => {
    expect(sortSessions(ROWS, null, "asc")).toEqual(ROWS);
  });

  it("does not mutate the input array", () => {
    const copy = [...ROWS];
    sortSessions(ROWS, "started", "asc");
    expect(ROWS).toEqual(copy);
  });

  it("sorts by started ascending (earliest first)", () => {
    const result = sortSessions(ROWS, "started", "asc").map((s) => s.id);
    expect(result).toEqual(["a", "b", "c"]);
  });

  it("sorts by started descending (latest first)", () => {
    const result = sortSessions(ROWS, "started", "desc").map((s) => s.id);
    expect(result).toEqual(["c", "b", "a"]);
  });

  it("falls back to created_at when started_at is null", () => {
    const rows = [
      makeSession({ id: "x", started_at: null, created_at: "2024-01-01T05:00:00Z" }),
      makeSession({ id: "y", started_at: null, created_at: "2024-01-01T01:00:00Z" }),
    ];
    expect(sortSessions(rows, "started", "asc").map((s) => s.id)).toEqual(["y", "x"]);
  });

  it("sorts by state alphabetically ascending", () => {
    const result = sortSessions(ROWS, "state", "asc").map((s) => s.state);
    expect(result).toEqual(["failed", "pending", "running"]);
  });

  it("sorts by state descending", () => {
    const result = sortSessions(ROWS, "state", "desc").map((s) => s.state);
    expect(result).toEqual(["running", "pending", "failed"]);
  });

  // Premise change (#385 item 7): this suite previously asserted the User column
  // sorts on `user_id`. The column no longer displays only the id — it stacks the
  // username under it — and the sort now follows the display. The old assertion
  // is not weakened away: it is replaced by the two cases that together pin the
  // new rule (name wins when present, id is the fallback when it is not).
  it("sorts by username ascending, not by user_id", () => {
    const result = sortSessions(ROWS, "user", "asc").map((s) => s.username);
    expect(result).toEqual(["xray", "yankee", "zulu"]);
    // and that is genuinely different from the id order
    expect(sortSessions(ROWS, "user", "asc").map((s) => s.user_id))
      .toEqual(["charlie", "bravo", "alpha"]);
  });

  it("sorts by username descending", () => {
    const result = sortSessions(ROWS, "user", "desc").map((s) => s.username);
    expect(result).toEqual(["zulu", "yankee", "xray"]);
  });

  it("falls back to user_id when username is absent (deleted user row)", () => {
    const rows = [
      makeSession({ id: "n", user_id: "zzz-uuid", username: "alpha" }),
      makeSession({ id: "u", user_id: "bbb-uuid", username: undefined }),
    ];
    // "alpha" < "bbb-uuid", so the named row sorts first even though its id is last.
    expect(sortSessions(rows, "user", "asc").map((s) => s.id)).toEqual(["n", "u"]);
  });
});
