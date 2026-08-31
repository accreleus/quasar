/**
 * One host, composed from `GET /v1/hosts/{id}` + `/gpus` + the shared live
 * session poll (spec §5.8, mock §A.5).
 *
 * The sessions come from a stubbed `useFleetContext` for the same reason the
 * Hosts tab's test stubs it: this page must not open a second sessions poll.
 */

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "tok" }) }));
vi.mock("../../components/Toast", () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock("../../api/admin");

const reload = vi.fn().mockResolvedValue(undefined);
let fleet: FleetContextValue;
vi.mock("../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => fleet,
}));

import * as adminApi from "../../api/admin";
import type { AdminSession, GPUAvailability, Host } from "../../api/types";
import type { FleetContextValue } from "../../lib/fleet/FleetContext";
import { HostDetail } from "./HostDetail";

const mocked = vi.mocked(adminApi);
// Fixed, not Date.now(): several relative-time facts (uptime, "no heartbeat
// for N minutes") are computed against `res.updatedAt`, a real Date.now()
// read inside the resource hook when its mocked fetch resolves. A live NOW
// captured once at module load drifts against that second read by however
// long the file takes to reach this test — enough under a loaded test run to
// cross a minute boundary. vi.setSystemTime pins both reads to the same
// instant per test.
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
    storage: [{ label: "agent-data", path: "/var/lib/quasar", total_mb: 122880, available_mb: 24576 }],
    capacity: {
      slots_total: 3,
      slots_used: 2,
      vram_mb_total: 32768,
      vram_mb_used: 21504,
      active_sessions: 1,
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
    vram_mb_used: 16384,
    vram_mb_free: 16384,
    vram_sampled_at: new Date(NOW).toISOString(),
    slots_total: 3,
    slots_reserved: 2,
    active_sessions: 1,
    render_node: "/dev/dri/renderD128",
    ...over,
  } as GPUAvailability;
}

function session(over: Partial<AdminSession> = {}): AdminSession {
  return {
    id: "s1",
    state: "running",
    host_id: "c2059601",
    app_name: "Cyberpunk 2077",
    username: "mara.k",
    started_at: new Date(NOW - 72 * 60 * 1000).toISOString(),
    stream: { codec: "av1" },
    latest_metrics: { browser: { metrics: { fps: 118, rtt_ms: 14 } } },
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

function renderDetail() {
  return render(
    <MemoryRouter initialEntries={["/admin/fleet/hosts/c2059601"]}>
      <Routes>
        <Route path="/admin/fleet/hosts/:id" element={<HostDetail />} />
        <Route path="/admin/fleet/hosts/:id/settings" element={<div>host settings page</div>} />
        <Route path="/admin/sessions/:id" element={<div>session detail page</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  // shouldAdvanceTime: true lets the mocked API's real promises settle
  // (plain useFakeTimers() deadlocks React's scheduler here, same as
  // useSwapTransition.test.tsx) while setSystemTime keeps every Date.now()
  // read pinned to NOW for the duration of each test.
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(NOW);
  setFleet();
  mocked.getHost.mockResolvedValue({ host: host() } as never);
  mocked.getHostGPUs.mockResolvedValue({ items: [gpu()] } as never);
  mocked.drainHost.mockResolvedValue({} as never);
  mocked.uncordonHost.mockResolvedValue({} as never);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("HostDetail — head and facts", () => {
  it("names the host, its hardware and where it came from", async () => {
    renderDetail();

    await waitFor(() => expect(screen.getByRole("heading", { name: "quasar-node-1" })).toBeTruthy());
    expect(screen.getByText("AMD Ryzen 9 9950X3D · 128 GB")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Fleet" })).toBeTruthy();
    expect(mocked.getHost).toHaveBeenCalledWith("tok", "c2059601");
    expect(mocked.getHostGPUs).toHaveBeenCalledWith("tok", "c2059601");
  });

  it("lists the facts, saying n/a for host uptime rather than inventing one", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByText("Node ID")).toBeTruthy());

    expect(screen.getByTestId("fact-node-id").textContent).toBe("c2059601");
    expect(screen.getByText("AMD Ryzen 9 9950X3D · 16 cores")).toBeTruthy();
    expect(screen.getByText("0.1.0")).toBeTruthy();
    expect(screen.getByText("1h 30m")).toBeTruthy();
    expect(screen.getByText("accepting sessions")).toBeTruthy();
    const uptime = screen.getByText("Uptime").closest("tr");
    expect(within(uptime as HTMLElement).getByText("n/a")).toBeTruthy();
  });

  it("says scheduling is paused once the host is not online", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ status: "draining" }) } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByText("paused")).toBeTruthy());
  });
});

describe("HostDetail — notes", () => {
  it("explains an offline host and that scheduling is paused", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({ status: "offline", last_heartbeat_at: new Date(NOW - 14 * 60_000).toISOString() }),
    } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByText(/No heartbeat for 14 minutes/)).toBeTruthy());
    expect(screen.getByText(/Scheduling is paused for this host/)).toBeTruthy();
  });

  it("says Never, not never, for a host that has not reported once", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ last_heartbeat_at: null }) } as never);
    renderDetail();

    // Every other standalone cell value on the console is capitalised; the
    // Fleet and Library tabs used to disagree about this one.
    await waitFor(() => expect(screen.getByText("Never")).toBeTruthy());
  });

  it("explains a failed capacity report, with the agent's reason", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({ capacity_detection: "failed", capacity_reason: "nvidia-smi not found" }),
    } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByText("GPU capacity failed.")).toBeTruthy());
    expect(screen.getByText(/nvidia-smi not found/)).toBeTruthy();
  });

  it("shows no warning note for a healthy host", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByRole("heading", { name: "quasar-node-1" })).toBeTruthy());

    expect(document.querySelector(".note.warn")).toBeNull();
  });
});

describe("HostDetail — capacity", () => {
  it("draws each GPU with its VRAM and slot readings", async () => {
    renderDetail();

    await waitFor(() => expect(screen.getByText("GeForce RTX 5090")).toBeTruthy());
    const row = screen.getByTestId("cap-row-gpu-g1");
    expect(within(row).getByText("16 GB / 32 GB")).toBeTruthy();
    expect(within(row).getByText("2 / 3")).toBeTruthy();
    expect(within(row).getByText("1 active")).toBeTruthy();
    // 16 of 32 GB: the gauge reads the same share as the bar.
    expect(within(row).getByRole("meter").getAttribute("aria-valuenow")).toBe("50");
  });

  it("says the VRAM is unknown rather than drawing an empty gauge", async () => {
    mocked.getHostGPUs.mockResolvedValue({
      items: [gpu({ vram_mb_used: null, vram_sampled_at: null })],
    } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByText("GeForce RTX 5090")).toBeTruthy());
    const row = screen.getByTestId("cap-row-gpu-g1");
    expect(within(row).queryByRole("meter")).toBeNull();
    expect(within(row).getAllByText("n/a").length).toBeGreaterThan(0);
  });

  it("reads storage off the reported volumes and links to managed homes", async () => {
    renderDetail();

    await waitFor(() => expect(screen.getByText("Storage")).toBeTruthy());
    const row = screen.getByTestId("cap-row-storage");
    expect(within(row).getByText("96 GB / 120 GB")).toBeTruthy();
    expect(within(row).getByText("24 GB")).toBeTruthy();
    expect(within(row).getByRole("meter").getAttribute("aria-valuenow")).toBe("80");
    expect(screen.getByRole("link", { name: "Managed homes" })).toBeTruthy();
  });

  it("keeps the memory row, saying memory use and CPU load are not reported", async () => {
    renderDetail();

    await waitFor(() => expect(screen.getByText("Memory")).toBeTruthy());
    const row = screen.getByTestId("cap-row-memory");
    expect(within(row).getByText("USED")).toBeTruthy();
    expect(within(row).getByText("CPU")).toBeTruthy();
    // Three readings the wire does not carry: the gauge, and both bars — a bar
    // with no value renders the console's glyph (components/Bar).
    expect(within(row).getAllByText("n/a")).toHaveLength(1);
    expect(within(row).getAllByText("—")).toHaveLength(2);
  });

  it("gauges storage on the fullest volume and lists the volumes beneath", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({
        storage: [
          { label: "agent-data", path: "/var/lib/quasar", total_mb: 10240, available_mb: 5120 },
          { label: "games", path: "/mnt/games", total_mb: 10240, available_mb: 1024 },
        ],
      }),
    } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByText("Storage")).toBeTruthy());
    const row = screen.getByTestId("cap-row-storage");
    // 90 % on /mnt/games, 50 % on the other: the average would read 70 %.
    expect(within(row).getByRole("meter").getAttribute("aria-valuenow")).toBe("90");
    // "agent-data" also names the row itself (.cell-id), so scope to the bars.
    expect(within(row).getByText("agent-data", { selector: ".lbl" })).toBeTruthy();
    expect(within(row).getByText("games", { selector: ".lbl" })).toBeTruthy();
  });
});

describe("HostDetail — sessions on this host", () => {
  it("lists only this host's sessions, with their live figures", async () => {
    setFleet([session(), session({ id: "s2", host_id: "other", app_name: "Blender" })]);
    renderDetail();

    await waitFor(() => expect(screen.getByText("Cyberpunk 2077")).toBeTruthy());
    expect(screen.queryByText("Blender")).toBeNull();
    expect(screen.getByText("mara.k")).toBeTruthy();
    expect(screen.getByText("av1")).toBeTruthy();
    expect(screen.getByText("118")).toBeTruthy();
    expect(screen.getByText("14 ms")).toBeTruthy();
    expect(screen.getByText("1h 12m")).toBeTruthy();
  });

  it("opens a session from its row", async () => {
    setFleet([session()]);
    renderDetail();
    await waitFor(() => expect(screen.getByText("Cyberpunk 2077")).toBeTruthy());

    fireEvent.click(screen.getByText("Cyberpunk 2077"));
    await waitFor(() => expect(screen.getByText("session detail page")).toBeTruthy());
  });

  it("says so when the host is running nothing", async () => {
    renderDetail();

    await waitFor(() => expect(screen.getByText("No sessions")).toBeTruthy());
    expect(screen.getByText("This host is not running anything right now.")).toBeTruthy();
  });
});

describe("HostDetail — actions", () => {
  it("drains the host", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByRole("button", { name: "Drain" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Drain" }));
    await waitFor(() => expect(mocked.drainHost).toHaveBeenCalledWith("tok", "c2059601"));
  });

  it("offers to resume scheduling on a draining host", async () => {
    mocked.getHost.mockResolvedValue({ host: host({ status: "draining" }) } as never);
    renderDetail();
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume scheduling" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Resume scheduling" }));
    await waitFor(() => expect(mocked.uncordonHost).toHaveBeenCalledWith("tok", "c2059601"));
  });

  it("goes to the host's settings", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByRole("button", { name: "Settings" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Settings" }));
    await waitFor(() => expect(screen.getByText("host settings page")).toBeTruthy());
  });

  it("shows the readiness card for this host", async () => {
    mocked.getHost.mockResolvedValue({
      host: host({
        readiness: [{ id: "nvidia_egl", status: "pass", summary: "EGL vendor json present", remediation: "" }],
      }),
    } as never);
    renderDetail();

    await waitFor(() => expect(screen.getByTestId("readiness-card")).toBeTruthy());
    expect(screen.getByTestId("readiness-check-nvidia_egl")).toBeTruthy();
  });
});
