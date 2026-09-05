import { describe, expect, it } from "vitest";
import type { PlatformRelease } from "../../api/types";
import {
  AGENT_IMAGE_VAR,
  CONTROL_IMAGE_VAR,
  REGISTRY_PULL_COMMAND,
  REGISTRY_RECREATE_COMMAND,
  UPDATER_UP_COMMAND,
  manualUpdatePath,
  redeployProfile,
  redeployRef,
} from "./manualUpdate";

const manifest = {
  format_version: 1,
  version: "0.3.0",
  prerelease: false,
  source_commit: "c".repeat(40),
  built_at: "2026-09-03T00:00:00Z",
  schema_version: 74,
  components: [
    {
      name: "control-plane",
      image: "ghcr.io/accreleus/quasar/quasar-control-plane",
      digest: "sha256:aaa",
    },
    {
      name: "node-agent",
      image: "ghcr.io/accreleus/quasar/quasar-node-agent",
      digest: "sha256:bbb",
    },
  ],
};

function release(over: Partial<PlatformRelease> = {}): PlatformRelease {
  return {
    id: "r1",
    channel: "stable",
    version: "0.3.0",
    source_commit: "c".repeat(40),
    built_at: "2026-09-03T00:00:00Z",
    schema_version: 74,
    prerelease: false,
    notes: "",
    compare_url: null,
    manifest,
    discovered_at: "2026-09-03T01:00:00Z",
    ...over,
  } as PlatformRelease;
}

const commandsOf = (reason: string, over = {}) =>
  manualUpdatePath({ reason, kind: "host", release: release(), ...over })
    ?.commands.map((c) => c.command)
    .join("\n") ?? null;

describe("redeployProfile", () => {
  it("maps a known vendor to its build profile", () => {
    expect(redeployProfile("NVIDIA")).toBe("nvidia");
    expect(redeployProfile("amd")).toBe("va");
    expect(redeployProfile("Intel")).toBe("va");
  });

  it("keeps the doc's placeholder when the vendor is unknown — a guess builds the wrong image", () => {
    expect(redeployProfile(null)).toBe("<va|nvidia>");
    expect(redeployProfile("")).toBe("<va|nvidia>");
  });
});

describe("redeployRef", () => {
  it("is the version tag on stable and the commit on edge", () => {
    expect(redeployRef(release())).toBe("v0.3.0");
    expect(redeployRef(release({ version: null, channel: "edge" }))).toBe("c".repeat(40));
    expect(redeployRef(null)).toBe("<ref>");
  });
});

describe("manualUpdatePath", () => {
  it("shows nothing for an eligible target or one that is merely waiting", () => {
    for (const reason of [
      null,
      "up_to_date",
      "no_release",
      "release_above_control_plane",
      "control_plane_not_first",
      "attempt_in_flight",
      "run_active",
    ]) {
      expect(manualUpdatePath({ reason, kind: "host", release: release() })).toBeNull();
    }
  });

  it("renders an unknown identifier as no command rather than a wrong one", () => {
    expect(manualUpdatePath({ reason: "something_new", kind: "host" })).toBeNull();
  });

  it("host_offline is a state, not a command", () => {
    const path = manualUpdatePath({ reason: "host_offline", kind: "host", release: release() });
    expect(path?.commands).toEqual([]);
    expect(path?.summary).toMatch(/online/i);
  });

  it("install_mode_source is the redeploy script, at the release's tag", () => {
    expect(commandsOf("install_mode_source")).toBe("deploy/redeploy.sh <va|nvidia> v0.3.0");
    expect(commandsOf("install_mode_source", { gpuVendor: "nvidia" })).toBe(
      "deploy/redeploy.sh nvidia v0.3.0",
    );
  });

  it("install_mode_source on edge redeploys the commit, which is all edge publishes", () => {
    const commit = "d".repeat(40);
    expect(
      commandsOf("install_mode_source", {
        release: release({ version: null, channel: "edge", source_commit: commit }),
      }),
    ).toBe(`deploy/redeploy.sh <va|nvidia> ${commit}`);
  });

  it("updater_absent adds the updater once, then the registry recipe", () => {
    const commands = commandsOf("updater_absent") ?? "";
    expect(commands).toContain(UPDATER_UP_COMMAND);
    expect(commands).toContain(REGISTRY_PULL_COMMAND);
    expect(commands).toContain(REGISTRY_RECREATE_COMMAND);
  });

  it("fills the two .env lines with the release manifest's digests, in its normative order", () => {
    const commands = commandsOf("updater_absent") ?? "";
    expect(commands).toContain(
      `${CONTROL_IMAGE_VAR}=ghcr.io/accreleus/quasar/quasar-control-plane@sha256:aaa`,
    );
    expect(commands).toContain(
      `${AGENT_IMAGE_VAR}=ghcr.io/accreleus/quasar/quasar-node-agent@sha256:bbb`,
    );
  });

  it("falls back to placeholders when the release carries no usable manifest", () => {
    const commands =
      commandsOf("updater_absent", { release: release({ manifest: null }) }) ?? "";
    expect(commands).toContain("<control-plane digest>");
    expect(commands).toContain("<node-agent digest>");
  });

  it("a half-understood manifest is no manifest: one bad component means placeholders", () => {
    const broken = {
      ...manifest,
      components: [manifest.components[0], { name: "node-agent" }],
    } as unknown as PlatformRelease["manifest"];
    const commands =
      commandsOf("updater_absent", { release: release({ manifest: broken }) }) ?? "";
    expect(commands).toContain("<node-agent digest>");
    expect(commands).not.toContain("sha256:aaa");
  });

  it("identity_unknown says to upgrade the agent first, with the recipe for its install", () => {
    const path = manualUpdatePath({
      reason: "identity_unknown",
      kind: "host",
      release: release(),
    });
    expect(path?.summary).toMatch(/identity reporting/i);
    expect(path?.commands.map((c) => c.command).join("\n")).toContain(REGISTRY_PULL_COMMAND);

    const source = manualUpdatePath({
      reason: "identity_unknown",
      kind: "host",
      installMode: "source",
      gpuVendor: "AMD",
      release: release(),
    });
    expect(source?.commands.map((c) => c.command)).toEqual(["deploy/redeploy.sh va v0.3.0"]);
  });

  it("never interpolates anything from the environment: every path is deploy/-relative", () => {
    for (const reason of ["install_mode_source", "updater_absent", "identity_unknown"]) {
      const commands = commandsOf(reason) ?? "";
      expect(commands).not.toMatch(/\/home\/|\/root\/|ssh |https?:\/\/(?!ghcr\.io)/);
    }
  });
});
