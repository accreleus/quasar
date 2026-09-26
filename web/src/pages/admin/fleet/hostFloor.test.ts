import { describe, expect, it } from "vitest";
import type { Host, PlatformReleaseView } from "../../../api/types";
import { floorPhrase, hostFloorState } from "./hostFloor";

const host = { id: "h1", install_mode: "owned", source_commit: "a".repeat(40) } as Host;

describe("hostFloorState", () => {
  it("reads nothing from a partial or failed view", () => {
    expect(hostFloorState(host, null)).toBeNull();
    expect(hostFloorState(host, { faults: [] } as unknown as PlatformReleaseView)).toBeNull();
  });

  it("is null for a managed host", () => {
    const view = {
      available: [],
      targets: [],
      installed: {
        control_plane: { source_commit: "b".repeat(40) },
        hosts: [{ host_id: "h1", identity_known: true, below_floor: false }],
      },
    } as unknown as PlatformReleaseView;
    expect(hostFloorState(host, view)).toBeNull();
  });
});

describe("floorPhrase", () => {
  it("names one floor, or each when they differ", () => {
    expect(floorPhrase({ agent: "0.5.0", actor: "0.5.0" })).toBe("v0.5.0 and newer");
    expect(floorPhrase({ agent: "0.5.0", actor: "0.4.0" })).toBe(
      "node agents from v0.5.0 and recovery actors from v0.4.0",
    );
    expect(floorPhrase({ agent: null, actor: null })).toBeNull();
  });
});
