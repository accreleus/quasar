/**
 * The host page below the floor (design_handoff_v3 rh06 floor*.png; control-api.md
 * amendment 14 §"below_floor"): the note, its one action, and what is not offered.
 * `below_floor` is the server's; the page only reads it.
 */

import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../components/Toast", () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock("../../api/admin");

let fleet: FleetContextValue;
vi.mock("../../lib/fleet/FleetContext", () => ({ useFleetContext: () => fleet }));

import * as adminApi from "../../api/admin";
import type { AdminSession, Host, PlatformApplyAttempt } from "../../api/types";
import type { FleetContextValue } from "../../lib/fleet/FleetContext";
import { HostDetail } from "./HostDetail";

const mocked = vi.mocked(adminApi);

const CP = "3f9a2c1e0c5a9d1b7a2f3e4d5c6b7a8901234567";
const OLD = "0a1b2c3d4e5f60718293a4b5c6d7e8f901234567";
const HOST = "c4e21a09";

function host(over: Partial<Host> = {}): Host {
  return {
    id: HOST,
    node_name: "gpu-host-3",
    status: "online",
    admission_restrictions: [],
    agent_version: "0.4.1",
    recovery_actor_version: "0.4.1",
    source_commit: OLD,
    recovery_actor_source_commit: OLD,
    seed_version: "0.4.1",
    built_at: "2026-09-01T00:00:00Z",
    install_mode: "owned",
    updater_present: true,
    capacity_detection: "ok",
    readiness: [],
    readiness_gate: { state: "active", blocking: [] },
    readiness_overrides: [],
    storage: [],
    agent_restart_count: 0,
    ...over,
  } as Host;
}

const release = {
  id: "rel-052",
  channel: "stable",
  version: "0.5.2",
  source_commit: CP,
  built_at: "2026-09-20T00:00:00Z",
  schema_version: 88,
  prerelease: false,
  migrates: false,
  manifest: {
    format_version: 2,
    components: [{ name: "control-plane" }, { name: "node-agent" }, { name: "recovery-actor" }],
    floor: [
      { name: "node-agent", version: "0.5.0" },
      { name: "recovery-actor", version: "0.5.0" },
    ],
  },
};

function view(opts: { belowFloor?: boolean; identityKnown?: boolean; eligible?: boolean } = {}) {
  const { belowFloor = true, identityKnown = true, eligible = true } = opts;
  return {
    faults: [],
    available: [release],
    installed: {
      control_plane: { version: "0.5.2", source_commit: CP, built_at: null, schema_version: 88 },
      hosts: [
        { host_id: HOST, node_name: "gpu-host-3", identity_known: identityKnown, below_floor: belowFloor },
      ],
    },
    targets: [
      { kind: "control_plane", host_id: null, node_name: null, eligible: false, reason: "up_to_date" },
      {
        kind: "host",
        host_id: HOST,
        node_name: "gpu-host-3",
        eligible,
        reason: eligible ? null : "host_offline",
      },
    ],
  } as never;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={[`/admin/fleet/hosts/${HOST}`]}>
      <Routes>
        <Route path="/admin/fleet/hosts/:id" element={<HostDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  fleet = {
    hosts: [],
    sessions: [{ id: "s1", state: "running", host_id: HOST } as AdminSession],
    loading: false,
    lastFetchedAt: Date.now(),
    errors: { hosts: null, sessions: null },
    reload: vi.fn().mockResolvedValue(undefined),
  };
  mocked.getHost.mockResolvedValue({ host: host() } as never);
  mocked.getHostGPUs.mockResolvedValue({ items: [] } as never);
  mocked.getPlatformReleases.mockResolvedValue(view());
  mocked.listPlatformAttempts.mockResolvedValue({ attempts: [] } as never);
});

describe("HostDetail below the floor", () => {
  it("says the host must update, names the floor, and offers only the update", async () => {
    renderPage();

    const note = (await screen.findByText("gpu-host-3 must update before it can be managed."))
      .closest(".note") as HTMLElement;
    expect(note.textContent).toContain(
      "Its node agent and recovery actor are v0.4.1; this control plane manages v0.5.0 and newer.",
    );
    expect(note.textContent).toContain("Updating ends its 1 live session.");
    expect(within(note).getByRole("button", { name: /Update to v0\.5\.2/ })).toBeTruthy();

    // Settings, the local console and cached images are not offered; drain is.
    expect(screen.queryByRole("button", { name: "Settings" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Local console" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Manage cached images" })).toBeNull();
    expect(screen.getByRole("button", { name: /drain/i })).toBeTruthy();

    // Both services the update replaces read "must update", each below the floor.
    const card = screen.getByText("Services on this machine").closest(".card") as HTMLElement;
    for (const name of ["Recovery actor", "Node agent"]) {
      const row = within(card).getByText(name).closest("tr") as HTMLElement;
      expect(within(row).getByText("must update")).toBeTruthy();
      expect(within(row).getByText("below v0.5.0")).toBeTruthy();
    }
  });

  it("opens the update confirmation, naming the sessions it ends", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /Update to v0\.5\.2/ }));
    expect(await screen.findByText("Update gpu-host-3")).toBeTruthy();
    expect(screen.getByText("Update now — ends 1 live session")).toBeTruthy();
  });

  it("an update that moves only the recovery actor ends no sessions", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ source_commit: CP, agent_version: "0.5.2" }) } as never);
    renderPage();

    const note = (await screen.findByText("gpu-host-3 must update before it can be managed."))
      .closest(".note") as HTMLElement;
    expect(note.textContent).toContain("Its recovery actor is v0.4.1");
    expect(note.textContent).toContain("which replaces the recovery actor and ends no sessions.");
    const card = screen.getByText("Services on this machine").closest(".card") as HTMLElement;
    const agent = within(card).getByText("Node agent").closest("tr") as HTMLElement;
    expect(within(agent).queryByText("must update")).toBeNull();
  });

  it("a failed update says it did not finish and offers it again", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({ recovery_actor_source_commit: CP, recovery_actor_version: "0.5.2" }),
    } as never);
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        {
          id: "1c7e0000-0000-4000-8000-0000000000a4",
          kind: "apply",
          state: "failed",
          reason: "unhealthy",
          requested_digests: [
            { name: "recovery-actor", image: "r/quasar-recovery", digest: "sha256:a" },
            { name: "node-agent", image: "r/quasar-node-agent", digest: "sha256:b" },
          ],
        } as unknown as PlatformApplyAttempt,
      ],
    } as never);
    renderPage();

    const note = (await screen.findByText("The update did not finish; gpu-host-3 still must update."))
      .closest(".note") as HTMLElement;
    expect(note.textContent).toContain("it replaced the recovery actor first, which is now v0.5.2.");
    expect(note.textContent).toContain("The new container started but never became healthy.");
    expect(within(note).getByText("Details")).toBeTruthy();
    expect(within(note).getByRole("button", { name: /Try again/ })).toBeTruthy();
  });

  it("a failed revert is not reported as a failed update", async () => {
    mocked.listPlatformAttempts.mockResolvedValue({
      attempts: [
        {
          id: "2d8f0000-0000-4000-8000-0000000000b5",
          kind: "revert",
          state: "failed",
          reason: "unhealthy",
          requested_digests: [{ name: "node-agent", image: "r/quasar-node-agent", digest: "sha256:b" }],
        } as unknown as PlatformApplyAttempt,
      ],
    } as never);
    renderPage();
    expect(await screen.findByText("gpu-host-3 must update before it can be managed.")).toBeTruthy();
    expect(screen.queryByText(/The update did not finish/)).toBeNull();
  });

  it("an update the host cannot take right now is shown, not offered", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ eligible: false }));
    renderPage();
    const button = await screen.findByRole("button", { name: /Update to v0\.5\.2/ });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("The host's agent is not connected.")).toBeTruthy();
  });

  it("an owned host that has not reported its release: nothing is offered until it does", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ belowFloor: false, identityKnown: false }));
    renderPage();
    expect(await screen.findByText("Version not reported.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Settings" })).toBeTruthy();
  });

  it("stacks the floor note above a missing seed, and still offers Remove host", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ seed_version: null }) } as never);
    renderPage();

    const floorNote = (await screen.findByText("gpu-host-3 must update before it can be managed."))
      .closest(".note") as HTMLElement;
    const seedNote = screen.getByText("No seed found on gpu-host-3.").closest(".note") as HTMLElement;
    expect(floorNote.compareDocumentPosition(seedNote) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    const card = screen.getByText("Services on this machine").closest(".card") as HTMLElement;
    expect(within(card).getAllByText("must update")).toHaveLength(2);
    expect(within(card).getByText("not found")).toBeTruthy();
    expect(within(card).getByRole("button", { name: /Remove host/ })).toBeTruthy();
  });

  it("stacks the floor note above another owner's container", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({
        readiness: [{ id: "owner_conflict", status: "fail", summary: "x", remediation: "" }] as never,
      }),
    } as never);
    renderPage();

    const floorNote = (await screen.findByText("gpu-host-3 must update before it can be managed."))
      .closest(".note") as HTMLElement;
    const conflictNote = screen
      .getByText("Another owner’s container is in the way on gpu-host-3.")
      .closest(".note") as HTMLElement;
    expect(floorNote.compareDocumentPosition(conflictNote) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    const card = screen.getByText("Services on this machine").closest(".card") as HTMLElement;
    expect(within(card).getAllByText("must update")).toHaveLength(2);
    expect(within(card).getByText("in the way")).toBeTruthy();
    // As rh06/conflict draws it: no removal while another owner's container is in the way.
    expect(within(card).queryByRole("button", { name: /Remove host/ })).toBeNull();
  });

  it("a managed host shows no floor note and keeps its settings", async () => {
    mocked.getPlatformReleases.mockResolvedValue(view({ belowFloor: false }));
    renderPage();
    expect(await screen.findByRole("button", { name: "Settings" })).toBeTruthy();
    expect(screen.queryByText(/must update before it can be managed/)).toBeNull();
    expect(screen.queryByText("must update")).toBeNull();
  });
});
