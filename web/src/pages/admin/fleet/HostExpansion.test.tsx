/**
 * The host-row drawer's Build column (mock §A.4): which agent build a host is
 * running and how it got there.
 *
 * Rendered directly rather than through the Hosts tab — the drawer takes its
 * host as a prop, so the tab's fleet-context stub would only add indirection
 * between the fixture and the assertion.
 */

import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";

import type { GPUAvailability, Host } from "../../../api/types";
import { HostExpansion } from "./HostExpansion";

const NOW = Date.parse("2026-08-29T12:00:00Z");
const COMMIT = "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567";

function host(over: Partial<Host> = {}): Host {
  return {
    id: "c2059601",
    node_name: "quasar-node-1",
    status: "online",
    agent_version: "0.1.0",
    cpu_cores: 16,
    cpu_model: "AMD Ryzen 9 9950X3D",
    mem_mb: 131072,
    capacity_detection: "ok",
    capacity_reason: null,
    readiness: [],
    readiness_reported_at: null,
    readiness_gate: { state: "active", blocking: [] },
    readiness_overrides: [],
    last_registered_at: "2026-08-01T00:00:00Z",
    last_heartbeat_at: new Date(NOW - 4000).toISOString(),
    storage: [{ label: "agent-data", path: "/var/lib/quasar", total_mb: 122880, available_mb: 98304 }],
    capacity: null,
    agent_connected_since: new Date(NOW - 90 * 60 * 1000).toISOString(),
    agent_restart_count: 0,
    agent_last_restart_at: null,
    source_commit: null,
    built_at: null,
    install_mode: null,
    updater_present: null,
    ...over,
  } as Host;
}

function gpu(over: Partial<GPUAvailability> = {}): GPUAvailability {
  return {
    gpu_id: "g1",
    gpu_index: 0,
    vendor: "NVIDIA",
    model: "NVIDIA GeForce RTX 5090",
    vram_mb_total: 32768,
    vram_mb_reserved: 0,
    vram_mb_used: 21504,
    vram_mb_free: 11264,
    vram_sampled_at: new Date(NOW).toISOString(),
    slots_total: 3,
    slots_reserved: 2,
    active_sessions: 2,
    render_node: "/dev/dri/renderD128",
    codecs: ["h264", "h265", "av1"],
    ...over,
  } as GPUAvailability;
}

function renderDrawer(over: Partial<Host> = {}, gpus: GPUAvailability[] | null | undefined = []) {
  return render(
    <MemoryRouter>
      <HostExpansion host={host(over)} gpus={gpus} gpuError={null} now={NOW} />
    </MemoryRouter>,
  );
}

/** The drawer's facts are label/value pairs, not table rows. */
function fact(label: string): HTMLElement {
  return screen.getByText(label).closest(".exp-fact") as HTMLElement;
}

describe("HostExpansion — the Build column", () => {
  it("shows a reported identity: short mono commit, build age, install mode, updater", () => {
    renderDrawer({
      source_commit: COMMIT,
      built_at: new Date(NOW - 3 * 24 * 60 * 60 * 1000).toISOString(),
      install_mode: "registry",
      updater_present: true,
    });

    const commit = within(fact("Commit")).getByTitle(COMMIT);
    expect(commit.textContent).toBe("1f0c1e0e0c5a");
    expect(commit.className).toContain("mono");

    expect(within(fact("Built")).getByText("3 days ago")).toBeTruthy();
    expect(within(fact("Install")).getByText("Registry")).toBeTruthy();
    expect(within(fact("Updater")).getByText("Present")).toBeTruthy();
  });

  it("reads a source-built host as such", () => {
    renderDrawer({ source_commit: COMMIT, install_mode: "source", updater_present: false });

    expect(within(fact("Install")).getByText("Built from source")).toBeTruthy();
  });

  // Amendment 14: an owned host's services are created by its recovery actor,
  // and `updater_present` then says whether that actor answered.
  it("reads an owned host as owned by Quasar, not as unknown", () => {
    renderDrawer({ source_commit: COMMIT, install_mode: "owned", updater_present: true });

    expect(within(fact("Install")).getByText("Owned by Quasar")).toBeTruthy();
    expect(within(fact("Install")).getByTitle(/recovery actor/)).toBeTruthy();
    expect(within(fact("Updater")).getByTitle(/recovery actor answered/)).toBeTruthy();
    expect(within(fact("Updater")).getByText("Recovery actor")).toBeTruthy();
  });

  // NULL is "nobody has said"; false is "an agent looked and found none". The
  // release surface reports them differently, so the drawer must too.
  it("does not render an unreported updater as a found-nothing one", () => {
    const { unmount } = renderDrawer({ updater_present: false });
    expect(within(fact("Updater")).getByText("None")).toBeTruthy();
    unmount();

    renderDrawer({ updater_present: null });
    expect(within(fact("Updater")).getByText("Unknown")).toBeTruthy();
  });

  it("says Unknown throughout for a host whose agent predates the identity fields", () => {
    renderDrawer();

    for (const label of ["Commit", "Built", "Install", "Updater"]) {
      expect(within(fact(label)).getByText("Unknown")).toBeTruthy();
    }
  });
});

describe("HostExpansion — the Services column (#357)", () => {
  const owned: Partial<Host> = {
    source_commit: COMMIT,
    install_mode: "owned",
    updater_present: true,
    agent_version: "0.5.2",
    recovery_actor_version: "0.5.2",
    recovery_actor_source_commit: COMMIT,
    seed_version: "0.5.0",
  };

  function services(): HTMLElement {
    return screen.getByText("Services", { selector: ".eyebrow" }).parentElement as HTMLElement;
  }

  function service(name: string): HTMLElement {
    return within(services()).getByText(name).closest(".exp-fact") as HTMLElement;
  }

  it("lists an owned GPU host's services and their versions", () => {
    renderDrawer(owned);

    expect(within(service("Seed")).getByText("v0.5.0")).toBeTruthy();
    expect(within(service("Recovery actor")).getByText("v0.5.2")).toBeTruthy();
    expect(within(service("Database")).getByText("none on this machine")).toBeTruthy();
    expect(within(service("Control plane")).getByText("not on this machine")).toBeTruthy();
    expect(within(service("Node agent")).getByText("v0.5.2")).toBeTruthy();
  });

  it("says not reported before the recovery actor has reported", () => {
    renderDrawer({ ...owned, updater_present: null, recovery_actor_version: null, seed_version: null });

    expect(within(service("Recovery actor")).getByText("not reported")).toBeTruthy();
    expect(within(service("Node agent")).getByText("not reported")).toBeTruthy();
    expect(within(service("Seed")).getByText("not reported")).toBeTruthy();
  });

  it("names the seed's owner beside its version, and says when none was found (#358)", () => {
    const { unmount } = renderDrawer(owned);
    expect(within(service("Seed")).getByText(/· external manager/)).toBeTruthy();
    unmount();

    renderDrawer({ ...owned, seed_version: null });
    expect(within(service("Seed")).getByText("not found")).toBeTruthy();
  });

  it("still lists the agent when the answering actor's version is not a release", () => {
    renderDrawer({ ...owned, recovery_actor_version: null, agent_version: "dev" });

    expect(within(service("Recovery actor")).getByText("not reported")).toBeTruthy();
    expect(within(service("Node agent")).getByText("dev")).toBeTruthy();
  });

  it("has no Services column for a host that is not owned", () => {
    renderDrawer({ ...owned, install_mode: "registry" });

    expect(screen.queryByText("Services", { selector: ".eyebrow" })).toBeNull();
  });
});

describe("HostExpansion — GPUs and slots codec chips (#302)", () => {
  it("shows a GPU's codecs beside its slots", () => {
    renderDrawer({}, [gpu()]);

    const row = fact(`GeForce RTX 5090 #0`);
    expect(within(row).getByText("H.264")).toBeTruthy();
    expect(within(row).getByText("HEVC")).toBeTruthy();
    expect(within(row).getByText("AV1")).toBeTruthy();
  });

  it("shows a muted 'Not reported' chip for a GPU inheriting an unreported host set", () => {
    renderDrawer({}, [gpu({ codecs: null })]);

    const row = fact(`GeForce RTX 5090 #0`);
    expect(within(row).getByText("Not reported")).toBeTruthy();
  });
});
