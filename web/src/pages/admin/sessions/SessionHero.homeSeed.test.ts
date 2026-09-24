import { describe, expect, it } from "vitest";
import type { AdminSession } from "../../../api/types";

import { homeSeedLabel } from "./SessionHero";

describe("initial managed-home evidence", () => {
  it("distinguishes actual reflink, copy, cold and preserved homes", () => {
    expect(homeSeedLabel({ mode: "reflink", reason: "seeded" })).toContain("Reflink clone completed");
    expect(homeSeedLabel({ mode: "copy", reason: "seeded" })).toContain("no reflink storage saving");
    expect(homeSeedLabel({ mode: "cold", reason: "clone_failed" })).toContain("Cold start");
    expect(homeSeedLabel({ mode: "existing", reason: "existing_home" })).toContain("preserved");
  });

  it("does not infer cold or saving from missing or unknown evidence", () => {
    expect(homeSeedLabel(null)).toBe("No verified initial home outcome");
    const futureCode = { mode: "copy", reason: "unexpected" } as unknown as AdminSession["home_seed"];
    expect(homeSeedLabel(futureCode)).toBe("Unknown home outcome");
  });
});
