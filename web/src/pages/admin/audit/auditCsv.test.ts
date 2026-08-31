import { describe, expect, it } from "vitest";
import type { AdminActivityItem } from "../../../api/admin";
import { toCsv } from "./auditCsv";

function item(over: Partial<AdminActivityItem> = {}): AdminActivityItem {
  return {
    id: 1,
    actor_user_id: "u-1",
    actor_username: "salty2011",
    action: "host.drain",
    target_type: "host",
    target_id: "h-1234567890",
    details: { reason: "maintenance" },
    created_at: "2026-08-08T14:02:11Z",
    severity: "warn",
    ...over,
  };
}

describe("toCsv", () => {
  it("emits the header row", () => {
    expect(toCsv([]).split("\n")[0]).toBe(
      "time,actor,action,target_type,target_id,severity,details",
    );
  });

  it("emits one row per entry, in order", () => {
    const csv = toCsv([item({ id: 1 }), item({ id: 2, action: "host.uncordon" })]);
    const lines = csv.split("\n");
    expect(lines).toHaveLength(3);
    expect(lines[1]).toContain("host.drain");
    expect(lines[2]).toContain("host.uncordon");
  });

  it("falls back to system for a null actor", () => {
    const csv = toCsv([item({ actor_username: null })]);
    expect(csv.split("\n")[1].startsWith("2026-08-08T14:02:11Z,system,")).toBe(true);
  });

  it("quotes a field containing a comma", () => {
    const csv = toCsv([item({ target_type: "host, cluster" })]);
    expect(csv).toContain('"host, cluster"');
  });

  it("quotes and doubles an embedded quote", () => {
    const csv = toCsv([item({ target_type: 'ho"st' })]);
    expect(csv).toContain('"ho""st"');
  });

  it("serialises details as compact JSON", () => {
    const csv = toCsv([item({ details: { a: 1, b: "two" } })]);
    expect(csv).toContain('"{""a"":1,""b"":""two""}"');
  });

  it("leaves target_id blank when absent", () => {
    const csv = toCsv([item({ target_id: null })]);
    const fields = csv.split("\n")[1].split(",");
    // target_id is the 5th field (time,actor,action,target_type,target_id,…)
    expect(fields[4]).toBe("");
  });
});
