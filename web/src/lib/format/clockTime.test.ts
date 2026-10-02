import { describe, expect, it } from "vitest";
import { clockTime } from "./clockTime";

// Local wall-clock inputs (no offset), so the expected strings hold in any
// timezone the suite runs in.
describe("clockTime", () => {
  it("renders a 24-hour local time with seconds by default", () => {
    expect(clockTime("2026-08-08T21:05:07")).toBe("21:05:07");
  });

  it("drops the seconds when asked", () => {
    expect(clockTime("2026-08-08T09:05:07", { seconds: false })).toBe("09:05");
  });

  it("says a dash for an instant it cannot read", () => {
    expect(clockTime("not a time")).toBe("—");
  });
});
