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

import type { Host } from "../../../api/types";
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

function renderDrawer(over: Partial<Host> = {}) {
  return render(
    <MemoryRouter>
      <HostExpansion host={host(over)} gpus={[]} gpuError={null} now={NOW} />
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
