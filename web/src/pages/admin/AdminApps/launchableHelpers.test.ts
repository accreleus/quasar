import { describe, expect, it } from "vitest";

import {
  allowListApplies,
  effectiveLaunchableIds,
  pinnedLaunchProfileId,
} from "./launchableHelpers";

describe("UI-P5 launchable allow-list derivation", () => {
  it("does not apply to a force app — that policy pins the profile", () => {
    expect(allowListApplies("force")).toBe(false);
    expect(allowListApplies("inherit")).toBe(true);
    expect(allowListApplies("prefer")).toBe(true);
    // …and a force app therefore never sends a list, mirroring the server,
    // which 400s on one and clears any stored rows.
    expect(effectiveLaunchableIds("force", "high", true, ["balanced"])).toEqual([]);
  });

  it("pins the app default only under prefer", () => {
    expect(pinnedLaunchProfileId("prefer", "high")).toBe("high");
    // Under inherit the account/global default decides, so a leftover
    // default_profile_id is not this app's default — the server folds nothing
    // in either, and the two must agree or the UI shows a tick the launch path
    // does not honour.
    expect(pinnedLaunchProfileId("inherit", "high")).toBe("");
    expect(pinnedLaunchProfileId("force", "high")).toBe("");
  });

  it("sends [] when the control is off — that is how 'unrestricted' is spelled", () => {
    expect(effectiveLaunchableIds("prefer", "high", false, ["balanced"])).toEqual([]);
  });

  it("includes the pinned default so 'only the default' is not read as 'no restriction'", () => {
    // The operator unticked everything. Without the pinned default this would
    // serialise to [] = unrestricted, the exact inverse of the intent.
    expect(effectiveLaunchableIds("prefer", "high", true, [])).toEqual(["high"]);
  });

  it("puts the pinned default first and never duplicates it", () => {
    expect(effectiveLaunchableIds("prefer", "high", true, ["balanced", "high"])).toEqual([
      "high",
      "balanced",
    ]);
  });

  it("has nothing to pin under inherit, so the ticked set is sent verbatim", () => {
    expect(effectiveLaunchableIds("inherit", "high", true, ["balanced"])).toEqual(["balanced"]);
  });
});
