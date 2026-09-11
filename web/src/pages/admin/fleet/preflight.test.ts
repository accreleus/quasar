import { describe, expect, it } from "vitest";
import type { PlatformApplyRun, PlatformReleaseTarget } from "../../../api/types";
import { blockingChecks, holdoutText, partialSummary, unknownChecks, willBeSkipped } from "./preflight";

function target(over: Partial<PlatformReleaseTarget>): PlatformReleaseTarget {
  return { kind: "host", host_id: "h1", node_name: "gpu-01", eligible: true, reason: null, ...over } as PlatformReleaseTarget;
}

describe("preflight phrasing", () => {
  it("tolerates a server that sends no preflight", () => {
    const t = target({});
    expect(blockingChecks(t)).toEqual([]);
    expect(unknownChecks(t)).toEqual([]);
    expect(holdoutText(target({ eligible: false, reason: "host_offline" }))).toBe("The host's agent is not connected.");
  });

  it("names the first failing check for a blocked target", () => {
    const t = target({
      eligible: false,
      reason: "preflight_blocked",
      preflight: {
        state: "blocked",
        checked_at: null,
        checks: [
          { id: "updater_socket", status: "pass", detail: "" },
          { id: "updater_stack_dir", status: "fail", detail: "set QUASAR_STACK_DIR" },
          { id: "image_resolvable", status: "unknown", detail: "no release" },
        ],
      },
    });
    expect(blockingChecks(t).map((c) => c.id)).toEqual(["updater_stack_dir"]);
    expect(unknownChecks(t).map((c) => c.id)).toEqual(["image_resolvable"]);
    expect(holdoutText(t)).toBe("Blocked: updater sees the stack directory");
  });

  it("a fleet run will skip every ineligible host except an up-to-date one", () => {
    const skipped = willBeSkipped([
      target({ kind: "control_plane", host_id: null, eligible: false, reason: "up_to_date" }),
      target({ host_id: "h1", eligible: true }),
      target({ host_id: "h2", eligible: false, reason: "up_to_date" }),
      target({ host_id: "h3", eligible: false, reason: "install_mode_source" }),
      target({ host_id: "h4", eligible: false, reason: "preflight_blocked" }),
    ]);
    expect(skipped.map((t) => t.host_id)).toEqual(["h3", "h4"]);
  });

  it("summarises a partial run from its attempts and skips", () => {
    const run = {
      attempts: [
        { target: "host", state: "succeeded" },
        { target: "host", state: "succeeded" },
      ],
      skipped: [
        { host_id: "h3", node_name: "gpu-03", reason: "up_to_date" },
        { host_id: "h4", node_name: "gpu-04", reason: "install_mode_source" },
        { host_id: "h5", node_name: "gpu-05", reason: "preflight_blocked" },
      ],
    } as PlatformApplyRun;
    expect(partialSummary(run)).toBe(
      "Applied to 2 of 4 hosts — 2 skipped: gpu-04 (built from source), gpu-05 (a pre-update check failed)",
    );
  });
});
