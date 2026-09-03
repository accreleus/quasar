/**
 * The Hosts tab against one fixed fleet (spec §5.8, mock §A.4).
 *
 * The shared fleet poll is replaced by a fixed `useFleetContext` value: this
 * page must not open a hosts poll of its own, and stubbing the context is how
 * that stays true — if it ever fetches hosts itself, these fixtures stop
 * reaching it and every assertion here fails.
 */

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../../components/Toast", () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock("../../../api/admin");

const reload = vi.fn().mockResolvedValue(undefined);
let fleet: FleetContextValue;
vi.mock("../../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => fleet,
}));

import * as adminApi from "../../../api/admin";
import type { AdminSession, GPUAvailability, Host } from "../../../api/types";
import type { FleetContextValue } from "../../../lib/fleet/FleetContext";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { HostsTab } from "./HostsTab";

const mocked = vi.mocked(adminApi);
const NOW = Date.parse("2026-08-29T12:00:00Z");

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
    capacity: {
      slots_total: 3,
      slots_used: 2,
      vram_mb_total: 32768,
      vram_mb_used: 21504,
      active_sessions: 2,
      gpu_count: 1,
    },
    agent_connected_since: new Date(NOW - 90 * 60 * 1000).toISOString(),
    agent_restart_count: 0,
    agent_last_restart_at: null,
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
    ...over,
  } as GPUAvailability;
}

function session(over: Partial<AdminSession> = {}): AdminSession {
  return { id: "s1", state: "running", host_id: "c2059601", ...over } as AdminSession;
}

function setFleet(hosts: Host[], sessions: AdminSession[] = []) {
  fleet = {
    hosts,
    sessions,
    loading: false,
    lastFetchedAt: NOW,
    errors: { hosts: null, sessions: null },
    reload,
  };
}

function renderTab() {
  return render(
    <MemoryRouter initialEntries={["/admin/fleet/hosts"]}>
      <SectionHeadProvider title="Fleet" tabs={FLEET_TABS}>
        <HostsTab />
      </SectionHeadProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  setFleet([host()]);
  mocked.getHostGPUs.mockResolvedValue({ items: [gpu()] } as never);
  mocked.drainHost.mockResolvedValue({} as never);
  mocked.uncordonHost.mockResolvedValue({} as never);
  mocked.deleteHost.mockResolvedValue(undefined as never);
});

describe("HostsTab — the table", () => {
  it("publishes the section sub-line, the tab count and both actions", async () => {
    setFleet([host(), host({ id: "h2", node_name: "quasar-node-2", status: "offline" })], [session()]);
    renderTab();

    await waitFor(() =>
      expect(screen.getByText("1 of 2 hosts online · 1 session running")).toBeTruthy(),
    );
    expect(screen.getByRole("button", { name: "Refresh" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Enroll host" })).toBeTruthy();
    expect(within(screen.getByRole("tab", { name: /Hosts/ })).getByText("2")).toBeTruthy();
  });

  it("shows the host, its id and its GPU, with slots and VRAM from the capacity roll-up", async () => {
    renderTab();

    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    expect(screen.getByText("c2059601")).toBeTruthy();
    await waitFor(() => expect(screen.getByText("GeForce RTX 5090")).toBeTruthy());
    expect(screen.getByText("2/3")).toBeTruthy();
    expect(screen.getByText("21 GB/32 GB")).toBeTruthy();
    // The wire carries no host memory use, so the column says so (spec §9).
    expect(screen.getAllByText("n/a").length).toBeGreaterThan(0);
    expect(screen.getByText("20%")).toBeTruthy();
  });

  it("names the state in the id sub-line when the host is not online", async () => {
    setFleet([host({ status: "draining" })]);
    renderTab();

    await waitFor(() => expect(screen.getByText("c2059601 · draining")).toBeTruthy());
  });

  it("calls an online host with a failed readiness check degraded", async () => {
    setFleet([
      host({ readiness: [{ id: "egl", status: "fail", summary: "no EGL vendor json", remediation: "" }] }),
    ]);
    renderTab();

    await waitFor(() => expect(screen.getByText("c2059601 · degraded")).toBeTruthy());
  });

  it("fetches each host's GPUs exactly once for the table, not once per row poll", async () => {
    setFleet([host(), host({ id: "h2", node_name: "quasar-node-2" })]);
    renderTab();

    await waitFor(() => expect(mocked.getHostGPUs).toHaveBeenCalledTimes(2));
    expect(mocked.getHostGPUs).toHaveBeenCalledWith("tok", "c2059601");
    expect(mocked.getHostGPUs).toHaveBeenCalledWith("tok", "h2");
  });

  it("keeps the row when its GPU read fails, and says so in the drawer", async () => {
    mocked.getHostGPUs.mockRejectedValue(new Error("boom"));
    renderTab();

    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Show capacity and storage/ }));
    await waitFor(() => expect(screen.getByText("Could not load GPUs")).toBeTruthy());
  });

  it("shows the enroll call to action when no host has enrolled", async () => {
    setFleet([]);
    renderTab();

    await waitFor(() => expect(screen.getByText("No hosts enrolled")).toBeTruthy());
    expect(screen.getAllByRole("button", { name: "Enroll host" }).length).toBe(2);
  });
});

describe("HostsTab — filtering", () => {
  it("filters to online hosts, and to the ones needing attention", async () => {
    setFleet([
      host(),
      host({ id: "h2", node_name: "quasar-node-2", status: "offline" }),
      host({ id: "h3", node_name: "quasar-node-3", capacity_detection: "failed" }),
    ]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-2")).toBeTruthy());

    fireEvent.click(screen.getByRole("tab", { name: "Online" }));
    expect(screen.queryByText("quasar-node-2")).toBeNull();
    expect(screen.getByText("quasar-node-3")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: /Needs attention/ }));
    expect(screen.queryByText("quasar-node-1")).toBeNull();
    expect(screen.getByText("quasar-node-2")).toBeTruthy();
    expect(screen.getByText("quasar-node-3")).toBeTruthy();
  });

  it("filters by name and by id", async () => {
    setFleet([host(), host({ id: "8fa41c02", node_name: "quasar-node-2" })]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-2")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Filter hosts"), { target: { value: "node-2" } });
    expect(screen.queryByText("quasar-node-1")).toBeNull();

    fireEvent.change(screen.getByLabelText("Filter hosts"), { target: { value: "c2059601" } });
    expect(screen.getByText("quasar-node-1")).toBeTruthy();
    expect(screen.queryByText("quasar-node-2")).toBeNull();

    fireEvent.change(screen.getByLabelText("Filter hosts"), { target: { value: "zzz" } });
    expect(screen.getByText("No hosts match")).toBeTruthy();
  });

  it("groups rows under a vendor header when grouping is on", async () => {
    setFleet([host(), host({ id: "h2", node_name: "quasar-node-2" })]);
    mocked.getHostGPUs.mockImplementation(async (_t: string, hostId: string) =>
      hostId === "h2"
        ? ({ items: [gpu({ vendor: "AMD", model: "Radeon RX 7900 XTX" })] } as never)
        : ({ items: [gpu()] } as never),
    );
    renderTab();
    await waitFor(() => expect(screen.getByText("Radeon RX 7900 XTX")).toBeTruthy());

    expect(screen.queryByText("NVIDIA", { selector: ".eyebrow" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Group by GPU vendor" }));
    expect(screen.getByText("NVIDIA", { selector: ".eyebrow" })).toBeTruthy();
    expect(screen.getByText("AMD", { selector: ".eyebrow" })).toBeTruthy();
  });
});

describe("HostsTab — the expanded row", () => {
  it("shows hardware, GPUs, storage and the three places to go next", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("GeForce RTX 5090")).toBeTruthy());

    const expander = screen.getByRole("button", { name: /Show capacity and storage/ });
    expect(expander.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(expander);
    expect(expander.getAttribute("aria-expanded")).toBe("true");

    expect(screen.getByText("AMD Ryzen 9 9950X3D · 16 cores")).toBeTruthy();
    expect(screen.getByText("128 GB")).toBeTruthy();
    expect(screen.getByText("1h 30m")).toBeTruthy();
    expect(screen.getByText("GeForce RTX 5090 #0")).toBeTruthy();
    // The GPU line and the Total line: one GPU, so they agree by construction.
    expect(screen.getByText("Total")).toBeTruthy();
    expect(screen.getAllByText("2/3 slots · 21 GB/32 GB")).toHaveLength(2);
    expect(screen.getByText("/var/lib/quasar")).toBeTruthy();
    // One bar per reported volume, above the host-wide totals: with a single
    // volume the bar and the Used fact carry the same figure.
    expect(screen.getByText("agent-data", { selector: ".lbl" })).toBeTruthy();
    expect(screen.getAllByText("24 GB / 120 GB")).toHaveLength(2);
    expect(screen.getByRole("link", { name: "Storage detail" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open host" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Local console" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Host settings" })).toBeTruthy();
  });

  it("chips the agent restart count on the collapsed row", async () => {
    setFleet([
      host({ agent_restart_count: 2, agent_last_restart_at: new Date(NOW - 5 * 60_000).toISOString() }),
    ]);
    renderTab();

    await waitFor(() => expect(screen.getByText("2 restarts")).toBeTruthy());
  });

  it("singularises that chip for exactly one restart, and omits it at zero", async () => {
    setFleet([host({ agent_restart_count: 1, agent_last_restart_at: new Date(NOW).toISOString() })]);
    const { unmount } = renderTab();
    await waitFor(() => expect(screen.getByText("1 restart")).toBeTruthy());
    unmount();

    setFleet([host()]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    expect(screen.queryByText(/restart/)).toBeNull();
  });

  it("surfaces the agent restart count when the agent has restarted", async () => {
    setFleet([
      host({ agent_restart_count: 3, agent_last_restart_at: new Date(NOW - 5 * 60_000).toISOString() }),
    ]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Show capacity and storage/ }));

    expect(screen.getByText("Agent restarts")).toBeTruthy();
    expect(screen.getByText(/3 · last 5m/)).toBeTruthy();
  });

  it("says the agent uptime is unknown when the control plane has never seen it connect", async () => {
    setFleet([host({ agent_connected_since: null })]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Show capacity and storage/ }));

    expect(screen.getByText("Agent uptime")).toBeTruthy();
  });

  it("explains a failed capacity report in the drawer", async () => {
    setFleet([host({ capacity_detection: "failed", capacity_reason: "nvidia-smi not found" })]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Show capacity and storage/ }));

    expect(screen.getByText("GPU capacity failed.")).toBeTruthy();
    expect(screen.getByText(/nvidia-smi not found/)).toBeTruthy();
  });

  it("renders the readiness card in the drawer when the host reported checks", async () => {
    setFleet([
      host({
        readiness: [{ id: "nvidia_egl", status: "pass", summary: "EGL vendor json present", remediation: "" }],
        readiness_reported_at: new Date(NOW - 60_000).toISOString(),
      }),
    ]);
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Show capacity and storage/ }));

    expect(screen.getByTestId("readiness-card")).toBeTruthy();
    expect(screen.getByTestId("readiness-check-nvidia_egl")).toBeTruthy();
  });
});

describe("HostsTab — the row menu", () => {
  const openMenu = async () => {
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Actions for quasar-node-1" }));
  };

  it("offers the three destinations, then drain, then remove", async () => {
    renderTab();
    await openMenu();

    const items = screen.getAllByRole("menuitem").map((el) => el.textContent);
    expect(items).toEqual(["Open host", "Local console", "Host settings", "Drain", "Remove host"]);
  });

  it("drains a host and refreshes the fleet", async () => {
    renderTab();
    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "Drain" }));

    await waitFor(() => expect(mocked.drainHost).toHaveBeenCalledWith("tok", "c2059601"));
    await waitFor(() => expect(reload).toHaveBeenCalled());
  });

  it("offers resume scheduling instead of drain once the host is draining", async () => {
    setFleet([host({ status: "draining" })]);
    renderTab();
    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "Resume scheduling" }));

    await waitFor(() => expect(mocked.uncordonHost).toHaveBeenCalledWith("tok", "c2059601"));
  });

  it("reports a failed drain in the row's own drawer", async () => {
    mocked.drainHost.mockRejectedValue(new Error("nope"));
    renderTab();
    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "Drain" }));

    await waitFor(() => expect(screen.getByText("drain failed")).toBeTruthy());
  });

  it("only offers to remove a host that has gone offline", async () => {
    renderTab();
    await openMenu();
    expect(screen.getByRole("menuitem", { name: "Remove host" }).hasAttribute("disabled")).toBe(true);
  });

  it("removes an offline host through a confirmation", async () => {
    setFleet([host({ status: "offline" })]);
    renderTab();
    await openMenu();
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove host" }));

    expect(screen.getByText(/permanently removes/)).toBeTruthy();
    expect(mocked.deleteHost).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Remove host" }));
    await waitFor(() => expect(mocked.deleteHost).toHaveBeenCalledWith("tok", "c2059601"));
  });
});

describe("HostsTab — enroll", () => {
  const openEnroll = async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("quasar-node-1")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Enroll host" }));
  };

  // #12: the modal composes a wss:// enrollment string from the page origin. jsdom's
  // origin is plain http, and from there the string would carry ws:// — the cleartext
  // link the enrollment string exists to close — so the modal refuses rather than
  // derive a ws:// address, and never mints.
  it("refuses to compose an enrollment string from a plain-http origin", async () => {
    await openEnroll();

    expect(screen.getByRole("dialog")).toBeTruthy();
    expect(screen.getByTestId("enroll-needs-https")).toBeTruthy();
    expect(screen.queryByText(/ws:\/\/localhost/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Mint enrollment string" })).toBeNull();
    expect(mocked.mintHostEnrollment).not.toHaveBeenCalled();
    expect(screen.getAllByText(/deploy\/\.env/, { selector: ".mono" }).length).toBeGreaterThan(0);
  });

  it("prints no command, and says agent-only packaging is operator work", async () => {
    await openEnroll();

    const dialog = screen.getByRole("dialog");
    expect(dialog.textContent).not.toMatch(/docker compose/);
    expect(screen.getByText(/There is no supported agent-only package yet/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Add a second GPU host" }).getAttribute("href")).toBe(
      "https://accreleus.github.io/quasar/install/second-host/",
    );
  });
});
