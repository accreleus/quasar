import { describe, expect, it } from "vitest";
import { usersToCsv, type UserCsvRow } from "./usersCsv";

function row(over: Partial<UserCsvRow> = {}): UserCsvRow {
  return {
    username: "grace",
    email: "grace@example.com",
    role: "User",
    state: "Active",
    activeSessionCount: 0,
    homeBytes: 1024,
    lastSeenAt: "2026-08-28T00:00:00Z",
    ...over,
  };
}

describe("usersToCsv", () => {
  it("emits the header row first", () => {
    const csv = usersToCsv([]);
    expect(csv).toBe("Username,Email,Role,State,Sessions,Home bytes,Last seen\n");
  });

  it("emits one row per user in column order", () => {
    const csv = usersToCsv([row()]);
    const lines = csv.trim().split("\n");
    expect(lines[1]).toBe(
      "grace,grace@example.com,User,Active,0,1024,2026-08-28T00:00:00Z",
    );
  });

  it("renders empty fields for a never-seen user with no resolved storage", () => {
    const csv = usersToCsv([row({ homeBytes: null, lastSeenAt: null })]);
    const lines = csv.trim().split("\n");
    expect(lines[1]).toBe("grace,grace@example.com,User,Active,0,,");
  });

  it("quotes a field containing a comma and doubles embedded quotes", () => {
    const csv = usersToCsv([row({ username: 'grace, "gg"' })]);
    const lines = csv.trim().split("\n");
    expect(lines[1]).toBe(
      '"grace, ""gg""",grace@example.com,User,Active,0,1024,2026-08-28T00:00:00Z',
    );
  });
});
