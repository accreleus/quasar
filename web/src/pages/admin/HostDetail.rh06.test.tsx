/**
 * The host page's RH-06 warnings and Remove host (#366): design_handoff_v3
 * screens/rh06 seed-missing, conflict, conflict-error, inv-error, remove-*.
 */

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../auth/context", () => ({
  useAuth: () => ({ token: "tok", user: { username: "salty2011" } }),
}));
vi.mock("../../components/Toast", () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock("../../api/admin");

const reload = vi.fn().mockResolvedValue(undefined);
let fleet: FleetContextValue;
vi.mock("../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => fleet,
}));

import * as adminApi from "../../api/admin";
import { ApiError } from "../../api/client";
import type { AdminSession, Host } from "../../api/types";
import type { FleetContextValue } from "../../lib/fleet/FleetContext";
import { HostDetail } from "./HostDetail";
import { setRemoval } from "./fleet/removeHost";

const mocked = vi.mocked(adminApi);
const NOW = Date.parse("2026-09-26T14:12:00Z");
const ID = "3b8e5d17-0000-4000-8000-000000000004";
const COMMIT = "3f9a2c1e0c5a9d1b7a2f3e4d5c6b7a8901234567";

function host(over: Partial<Host> = {}): Host {
  return {
    id: ID,
    node_name: "gpu-host-4",
    status: "online",
    admission_restrictions: [],
    agent_version: "0.5.2",
    cpu_model: "AMD Ryzen 7 5800X3D",
    capacity_detection: "ok",
    capacity_reason: null,
    readiness: [],
    readiness_reported_at: new Date(NOW - 60_000).toISOString(),
    readiness_gate: { state: "active", blocking: [] },
    readiness_overrides: [],
    last_registered_at: new Date(NOW - 3600_000).toISOString(),
    last_heartbeat_at: new Date(NOW - 3000).toISOString(),
    storage: [],
    capacity: { slots_total: 2, slots_used: 0, vram_mb_total: 16384, vram_mb_used: 0, active_sessions: 0, gpu_count: 1 },
    agent_connected_since: new Date(NOW - 3600_000).toISOString(),
    agent_restart_count: 0,
    agent_last_restart_at: null,
    source_commit: COMMIT,
    built_at: new Date(NOW - 86400_000).toISOString(),
    install_mode: "owned",
    updater_present: true,
    recovery_actor_version: "0.5.2",
    recovery_actor_source_commit: COMMIT,
    seed_version: "0.5.0",
    ...over,
  } as Host;
}

const CONFLICT = {
  id: "owner_conflict",
  status: "fail",
  summary:
    "quasar-node-agent-1 looks like a Quasar node agent, but this installation did not create it (part of the Compose project quasar, probably left from an older Compose install)",
  remediation: "docker rm -f quasar-node-agent-1",
  source: "local",
};

function session(over: Partial<AdminSession> = {}): AdminSession {
  return {
    id: "s1",
    state: "running",
    host_id: ID,
    app_name: "Steam",
    username: "mara.k",
    created_at: new Date(NOW - 41 * 60_000).toISOString(),
    started_at: new Date(NOW - 41 * 60_000).toISOString(),
    ...over,
  } as AdminSession;
}

function setFleet(sessions: AdminSession[] = []) {
  fleet = {
    hosts: [],
    sessions,
    loading: false,
    lastFetchedAt: NOW,
    errors: { hosts: null, sessions: null },
    reload,
  };
}

function renderDetail(path = `/admin/fleet/hosts/${ID}`) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="/admin/fleet/hosts/:id" element={<HostDetail />} />
        <Route path="/admin/fleet/hosts" element={<div>hosts list</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function card(): Promise<HTMLElement> {
  return (await screen.findByText("Services on this machine")).closest(".card") as HTMLElement;
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
  setFleet();
  setRemoval(ID, null);
  mocked.getHost.mockResolvedValue({ host: host() } as never);
  mocked.getHostGPUs.mockResolvedValue({ items: [] } as never);
  mocked.getPlatformReleases.mockResolvedValue({
    faults: [],
    installed: {
      control_plane: { version: "0.5.2", source_commit: COMMIT, built_at: null, schema_version: 96 },
      hosts: [],
    },
  } as never);
  mocked.listPlatformAttempts.mockResolvedValue({ attempts: [] } as never);
  mocked.drainHost.mockResolvedValue({ host: host({ status: "draining" }) } as never);
  mocked.uncordonHost.mockResolvedValue({ host: host() } as never);
  mocked.removePlatformHost.mockResolvedValue({ host: host({ status: "draining" }) } as never);
  mocked.deleteHost.mockResolvedValue(undefined as never);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("HostDetail — seed missing (#366)", () => {
  it("warns when the answering recovery actor finds no seed", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ seed_version: null }) } as never);
    renderDetail();
    expect(await screen.findByText("No seed found on gpu-host-4.")).toBeTruthy();
    expect(screen.getByText(/nothing will re-create it/)).toBeTruthy();
  });

  it("never special-cases an opaque seed version, and says nothing while the actor has not answered", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ seed_version: "unknown" }) } as never);
    const { unmount } = renderDetail();
    await card();
    expect(screen.queryByText(/No seed found/)).toBeNull();
    unmount();

    mocked.getHost.mockResolvedValue({
      host: host({ seed_version: null, updater_present: false }),
    } as never);
    renderDetail();
    await card();
    expect(screen.queryByText(/No seed found/)).toBeNull();
  });
});

describe("HostDetail — owner conflict (#366)", () => {
  it("names what is in the way, adds its row, and keeps identifiers under Details", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ readiness: [CONFLICT] as never }) } as never);
    renderDetail();

    const note = (await screen.findByText("Another owner’s container is in the way on gpu-host-4.")).closest(
      ".note",
    ) as HTMLElement;
    expect(note.textContent).toMatch(/quasar-node-agent-1 looks like a Quasar node agent/);
    expect(note.textContent).toMatch(/will not update this machine while that container exists/);
    const details = within(note).getByText("Details").closest("details") as HTMLElement;
    expect(details.hasAttribute("open")).toBe(false);
    expect(within(details).getByText(/readiness check: owner_conflict/)).toBeTruthy();
    expect(within(details).getByText(/fix: docker rm -f quasar-node-agent-1/)).toBeTruthy();

    const row = within(await card()).getByText("Another owner’s container").closest("tr") as HTMLElement;
    expect(within(row).getByText("Another owner")).toBeTruthy();
    expect(within(row).getByText("in the way")).toBeTruthy();
    // Not removed from here while another owner's container is in the way.
    expect(screen.queryByRole("button", { name: /Remove host/ })).toBeNull();
  });

  it("checks again, and says it is still in the way when a newer report still has it", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ readiness: [CONFLICT] as never }) } as never);
    renderDetail();
    const again = await screen.findByRole("button", { name: /Check again/ });

    mocked.getHost.mockResolvedValue({
      host: host({
        readiness: [CONFLICT] as never,
        readiness_reported_at: new Date(NOW + 20_000).toISOString(),
      }),
    } as never);
    fireEvent.click(again);
    expect(await screen.findByRole("button", { name: /Checking/ })).toBeTruthy();
    await act(async () => {
      vi.advanceTimersByTime(21_000);
    });
    expect(await screen.findByText("Still in the way.")).toBeTruthy();
    expect(screen.getByText(/stopped containers count too/)).toBeTruthy();
  });

  it("clears once the container is gone", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ readiness: [CONFLICT] as never }) } as never);
    renderDetail();
    await screen.findByText(/in the way on gpu-host-4/);
    mocked.getHost.mockResolvedValue({ host: host({ readiness: [] }) } as never);
    await act(async () => {
      vi.advanceTimersByTime(6_000);
    });
    await waitFor(() => expect(screen.queryByText(/in the way on gpu-host-4/)).toBeNull());
  });
});

describe("HostDetail — the last report (#366)", () => {
  it("keeps the last answered report on screen, marked with its time, when the actor stops answering", async () => {
    renderDetail();
    await card();
    mocked.getHost.mockResolvedValue({ host: host({ updater_present: false }) } as never);
    await act(async () => {
      vi.advanceTimersByTime(6_000);
    });
    const c = await card();
    await waitFor(() => expect(within(c).getByText(/^Last report from \d\d:\d\d\. Nothing here is acted on/)).toBeTruthy());
    expect(within(c).getByText(/so the list below is its last report/)).toBeTruthy();
    const actor = within(c).getByText("Recovery actor").closest("tr") as HTMLElement;
    expect(within(actor).getByText("v0.5.2")).toBeTruthy();
    expect(within(actor).getByText(/^as of \d\d:\d\d$/)).toBeTruthy();
  });
});

describe("HostDetail — remove host (#366)", () => {
  it("confirms, drains, waits for the host's sessions, then asks the recovery actor", async () => {
    setFleet([session()]);
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));

    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Remove gpu-host-4?")).toBeTruthy();
    expect(within(dialog).getByText("1 live session")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: /Remove host/ }));

    await waitFor(() => expect(mocked.drainHost).toHaveBeenCalledWith("tok", ID));
    expect(await screen.findByText("Removing gpu-host-4.")).toBeTruthy();
    expect(screen.getByText(/waiting for 1 live session to end \(longest: 41 minutes so far\)/)).toBeTruthy();
    expect(screen.getByText(/Removal started by salty2011 at \d\d:\d\d\./)).toBeTruthy();
    expect(screen.getByText("removing")).toBeTruthy();
    expect(mocked.removePlatformHost).not.toHaveBeenCalled();

    // The session ends: the removal is sent.
    mocked.getHost.mockResolvedValue({ host: host({ status: "draining" }) } as never);
    setFleet([]);
    await act(async () => {
      vi.advanceTimersByTime(6_000);
    });
    await waitFor(() => expect(mocked.removePlatformHost).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/removing the node agent, then\s+itself/)).toBeTruthy();

    // The agent is removed: the host goes offline, and it can be forgotten.
    mocked.getHost.mockResolvedValue({ host: host({ status: "offline" }) } as never);
    await act(async () => {
      vi.advanceTimersByTime(6_000);
    });
    expect(await screen.findByText("gpu-host-4 was removed.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Forget host" }));
    await waitFor(() => expect(mocked.deleteHost).toHaveBeenCalledWith("tok", ID));
    expect(await screen.findByText("hosts list")).toBeTruthy();
    expect(mocked.removePlatformHost).toHaveBeenCalledTimes(1);
  });

  it("with no live session asks the recovery actor at once", async () => {
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));
    await waitFor(() => expect(mocked.removePlatformHost).toHaveBeenCalledWith("tok", ID, {}));
    expect(mocked.drainHost).not.toHaveBeenCalled();
  });

  it("keeps waiting when the server still counts a session the poll no longer shows", async () => {
    mocked.removePlatformHost.mockRejectedValueOnce(
      new ApiError(409, "conflict", "1 session(s) are still live on this host"),
    );
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));
    await waitFor(() => expect(mocked.removePlatformHost).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Removing gpu-host-4.")).toBeTruthy();
    expect(screen.queryByText("Removing gpu-host-4 did not finish.")).toBeNull();

    await act(async () => {
      vi.advanceTimersByTime(11_000);
    });
    await waitFor(() => expect(mocked.removePlatformHost).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/removing the node agent, then\s+itself/)).toBeTruthy();
  });

  it("cancels a removal that is waiting, lifting the drain it took", async () => {
    setFleet([session()]);
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel removal" }));
    await waitFor(() => expect(mocked.uncordonHost).toHaveBeenCalledWith("tok", ID));
    await waitFor(() => expect(screen.queryByText("Removing gpu-host-4.")).toBeNull());
  });

  it("says a refused removal did not finish, with the reason under Details, and retries", async () => {
    mocked.removePlatformHost.mockRejectedValueOnce(
      new ApiError(409, "host_not_removable", "the host's recovery actor refused the removal (busy); nothing was removed"),
    );
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));

    const note = (await screen.findByText("Removing gpu-host-4 did not finish.")).closest(".note") as HTMLElement;
    expect(within(note).getByText(/409 host_not_removable/)).toBeTruthy();
    fireEvent.click(within(note).getByRole("button", { name: /Retry removal/ }));
    await waitFor(() => expect(mocked.removePlatformHost).toHaveBeenCalledTimes(2));
  });

  it("explains a host that is not connected, and does not offer the removal", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({ status: "offline", last_heartbeat_at: new Date(NOW - 3 * 86400_000).toISOString() }),
    } as never);
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("gpu-host-4 is not connected")).toBeTruthy();
    expect(within(dialog).getByText(/last seen 3 days ago/)).toBeTruthy();
    expect((within(dialog).getByRole("button", { name: /Remove host/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("reads a disconnected owned host that an admission hold keeps 'draining' as not connected", async () => {
    // Live on the owned fleet: the journal reconciliation hold keeps a gone agent's host
    // `draining`; only its stopped heartbeat says it is gone.
    mocked.getHost.mockResolvedValue({
      host: host({
        status: "draining",
        admission_restrictions: [{ owner_kind: "reconciliation", reason: "journal_reconciliation" }] as never,
        last_heartbeat_at: new Date(NOW - 10 * 60_000).toISOString(),
      }),
    } as never);
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("gpu-host-4 is not connected")).toBeTruthy();
    expect((within(dialog).getByRole("button", { name: /Remove host/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("says a host that went away before the removal was sent is not connected", async () => {
    mocked.removePlatformHost.mockRejectedValueOnce(
      new ApiError(409, "host_not_eligible", "this host cannot take this release right now", undefined, undefined, undefined, "host_offline"),
    );
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));
    const note = (await screen.findByText("Removing gpu-host-4 did not finish.")).closest(".note") as HTMLElement;
    expect(note.textContent).toContain("It is not connected, so its recovery actor could not be asked");
  });

  it("counts a removal done when the host's heartbeats stop, though its drain keeps it 'draining'", async () => {
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /Remove host/ }));
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: /Remove host/ }));
    expect(await screen.findByText(/removing the node agent, then\s+itself/)).toBeTruthy();
    mocked.getHost.mockResolvedValue({
      host: host({
        status: "draining",
        admission_restrictions: [{ owner_kind: "manual", reason: "manual_drain" }] as never,
        last_heartbeat_at: new Date(NOW - 3000).toISOString(),
      }),
    } as never);
    await act(async () => {
      vi.advanceTimersByTime(70_000);
    });
    expect(await screen.findByText("gpu-host-4 was removed.")).toBeTruthy();
  });

  it("opens the confirmation when the Hosts tab sends the admin here to remove", async () => {
    renderDetail(`/admin/fleet/hosts/${ID}?remove=1`);
    expect(await screen.findByText("Remove gpu-host-4?")).toBeTruthy();
  });

  it("never offers removal of the control plane's own machine", async () => {
    mocked.getPlatformReleases.mockResolvedValue({
      faults: [],
      installed: {
        control_plane: {
          version: "0.5.2",
          source_commit: COMMIT,
          built_at: null,
          schema_version: 96,
          machine_role: "combined",
          machine_node_name: "gpu-host-4",
        },
        hosts: [],
      },
    } as never);
    renderDetail();
    await card();
    expect(screen.getByText(/run the uninstall command on the machine/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Remove host/ })).toBeNull();
  });
});
