import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "token" }) }));
const { addToastMock } = vi.hoisted(() => ({ addToastMock: vi.fn() }));
vi.mock("../../../components/Toast", () => ({ useToast: () => ({ addToast: addToastMock }) }));
// Hosts come from the fleet poll AdminLayout mounts above this page; stubbing
// the context is what keeps the page from opening a second hosts read.
let fleetHosts: Host[] = [];
vi.mock("../../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({ hosts: fleetHosts, sessions: [] }),
}));
vi.mock("../../../api/admin", () => ({
  listAdminHomes: vi.fn(),
  listHosts: vi.fn(),
  tombstoneHome: vi.fn(),
  runJobNow: vi.fn(),
}));

import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { AdminHome, Host } from "../../../api/types";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { FLEET_TABS } from "../../../components/shell/sectionTabs";
import { StorageTab } from "./StorageTab";

// The page publishes its head to the Fleet section container — its Refresh /
// Reclaim pending actions render there, so every test mounts the container.
function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/fleet/storage"]}>
      <SectionHeadProvider title="Fleet" tabs={FLEET_TABS}>
        <StorageTab />
      </SectionHeadProvider>
    </MemoryRouter>,
  );
}

function makeHome(overrides: Partial<AdminHome>): AdminHome {
  return {
    id: "home-id",
    user_id: "u-alice",
    app_id: "a-steam",
    host_id: "h-tower",
    username: "alice",
    app_name: "Steam",
    host_name: "tower",
    provider: "local",
    ref: "/data/quasar/homes/alice/steam",
    bytes_used: 500,
    created_at: "2026-01-01T00:00:00Z",
    last_used_at: "2026-01-01T00:00:00Z",
    gc_after: null,
    ...overrides,
  };
}

function makeHost(overrides: Partial<Host> = {}): Host {
  return {
    id: "h-tower",
    node_name: "tower",
    status: "online",
    agent_version: "0.1.0",
    cpu_cores: 16,
    mem_mb: 65536,
    cpu_model: "AMD",
    last_registered_at: "2026-01-01T00:00:00Z",
    last_heartbeat_at: "2026-01-01T00:00:00Z",
    storage: [{ label: "root", path: "/var/lib/quasar", total_mb: 1_000_000, available_mb: 500_000 }],
    capacity_detection: "ok",
    capacity_reason: null,
    readiness: null,
    readiness_reported_at: null,
    capacity: { slots_total: 0, slots_used: 0, vram_mb_total: 0, vram_mb_used: 0 },
    ...overrides,
  } as Host;
}

describe("StorageTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    addToastMock.mockClear();
    fleetHosts = [makeHost()];
  });

  it("shows the empty state with no homes", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({ items: [], next_cursor: null } as never);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/No managed homes yet\. Enable/)).toBeTruthy(),
    );
    expect(screen.queryByText("alice")).toBeNull();
  });

  it("publishes the section head sub-line and storage count", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1", bytes_used: 500 })],
      next_cursor: null,
    } as never);
    renderPage();
    await waitFor(() => expect(screen.getByText(/1 managed home/)).toBeTruthy());
    expect(screen.getByText("1", { selector: ".cnt" })).toBeTruthy();
  });

  it("shows the KPI strip: managed homes, total size, active, pending cleanup", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", bytes_used: 500, gc_after: null }),
        makeHome({ id: "h2", bytes_used: 1024 * 1024 * 1024, gc_after: "2026-02-01T00:00:00Z" }),
        makeHome({ id: "h3", bytes_used: 6.5 * 1024 * 1024 * 1024, gc_after: null }),
      ],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("Managed homes")).toBeTruthy());
    const managedHomesCard = screen.getByText("Managed homes").closest<HTMLElement>(".card")!;
    expect(within(managedHomesCard).getByText("3")).toBeTruthy();
    expect(screen.getByText("Active")).toBeTruthy();
    expect(screen.getByText("Pending cleanup")).toBeTruthy();
    expect(screen.getByText("attached to a user")).toBeTruthy();
    // One formatter (lib/format/bytes) for the head sub-line, the KPI and the
    // Size column, so a home never reads two sizes on one screen — and it keeps
    // the fraction rather than rounding 7.5 GB down to 7.
    const totalSizeCard = screen.getByText("Total size").closest<HTMLElement>(".card")!;
    expect(within(totalSizeCard).getByText("7.5 GB")).toBeTruthy();
  });

  it("groups homes by user, collapsed until expanded", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", app_id: "a-steam", app_name: "Steam", bytes_used: 500 }),
        makeHome({ id: "h2", app_id: "a-portal", app_name: "Portal 2", bytes_used: 300 }),
      ],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    expect(screen.queryByText("Steam")).toBeNull();
    expect(screen.queryByText("Portal 2")).toBeNull();

    fireEvent.click(screen.getByLabelText("Expand row"));
    expect(screen.getByText("Steam")).toBeTruthy();
    expect(screen.getByText("Portal 2")).toBeTruthy();
  });

  it("shows last-used and the short home id in the per-home meta line", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "home-abcdefgh12345", last_used_at: "2026-01-01T00:00:00Z" })],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    fireEvent.click(screen.getByLabelText("Expand row"));

    expect(screen.getByText(/Last used .* · home-abc/)).toBeTruthy();
  });

  it("shows a State chip per home once expanded", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", app_id: "a-steam", app_name: "Steam", bytes_used: 100 }),
        makeHome({ id: "h2", app_id: null, app_name: null, host_id: null, host_name: null, bytes_used: 250 }),
      ],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    fireEvent.click(screen.getByLabelText("Expand row"));

    const expanded = screen.getByText("Steam").closest("table")!;
    expect(within(expanded).getByText("Active")).toBeTruthy();
    expect(within(expanded).getByText("Host deleted")).toBeTruthy();
    // Appears twice for this row: the Host column's sub-line, and the State
    // chip (an app-orphaned home's state is also literally "App deleted").
    expect(within(expanded).getAllByText("App deleted").length).toBeGreaterThan(0);
  });

  it("buckets homes with no linked user under a clearly-labeled group", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1", user_id: null, username: null, bytes_used: 42 })],
      next_cursor: null,
    } as never);
    renderPage();

    // Both the group's User cell and its State chip read "No linked user".
    await waitFor(() => expect(screen.getAllByText("No linked user").length).toBeGreaterThan(0));
  });

  it("deletes a home from its expanded row via the row menu, after confirming", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1", bytes_used: 500 })],
      next_cursor: null,
    } as never);
    vi.mocked(adminApi.tombstoneHome).mockResolvedValue(undefined as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    fireEvent.click(screen.getByLabelText("Expand row"));
    fireEvent.click(screen.getByLabelText(/Actions for alice home/));
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete" }));

    const dialog = await screen.findByRole("dialog", { name: "Delete managed home" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));

    await waitFor(() => expect(adminApi.tombstoneHome).toHaveBeenCalledWith("token", "h1"));
  });

  it("filters by user or host via the toolbar search", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", user_id: "u-alice", username: "alice", bytes_used: 100 }),
        makeHome({ id: "h2", user_id: "u-bob", username: "bob", host_name: "node-2", bytes_used: 200 }),
      ],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    expect(screen.getByText("bob")).toBeTruthy();

    fireEvent.change(screen.getByPlaceholderText("Filter by user or host"), { target: { value: "bob" } });
    expect(screen.queryByText("alice")).toBeNull();
    expect(screen.getByText("bob")).toBeTruthy();
  });

  it("filters by provider via the toolbar select", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", user_id: "u-alice", username: "alice", provider: "local", bytes_used: 100 }),
        makeHome({ id: "h2", user_id: "u-bob", username: "bob", provider: "auto", bytes_used: 200 }),
      ],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("Filter by provider"), { target: { value: "auto" } });
    expect(screen.queryByText("alice")).toBeNull();
    expect(screen.getByText("bob")).toBeTruthy();
  });

  it("offers no storage-provider control, and points at where the root is set", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({ items: [], next_cursor: null } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText(/storage root/i)).toBeTruthy());
    // Only the provider filter select exists — nothing that writes settings.
    expect(screen.queryByRole("combobox", { name: /new homes use/i })).toBeNull();
    expect(screen.getByText(/Fleet › Hosts › Settings/)).toBeTruthy();
  });

  it("never renders the removed legacy-volume banner or escape hatch, even for a legacy-provider home", async () => {
    // A home can still legitimately carry the pre-#473 "volume" value in its
    // own `provider` column (old data) — the Provider cell honestly labels
    // it "Volume"; what must not come back is the removed banner/button.
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1", provider: "volume" })],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    expect(screen.queryByText(/Legacy/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /use host storage roots/i })).toBeNull();
  });

  it("reclaim pending runs the home.gc job once per host with a pending home", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", user_id: "u-alice", username: "alice", host_id: "h-tower", gc_after: "2026-02-01T00:00:00Z", bytes_used: 100 }),
        makeHome({ id: "h2", user_id: "u-bob", username: "bob", host_id: "h-tower", gc_after: "2026-02-01T00:00:00Z", bytes_used: 200 }),
        makeHome({ id: "h3", user_id: "u-carl", username: "carl", host_id: "h-node2", gc_after: "2026-02-01T00:00:00Z", bytes_used: 50 }),
        makeHome({ id: "h4", user_id: "u-dana", username: "dana", bytes_used: 300 }),
      ],
      next_cursor: null,
    } as never);
    vi.mocked(adminApi.runJobNow).mockResolvedValue({
      run_id: "run-1",
      state: "pending",
      scheduled_for: "2026-08-29T00:00:00Z",
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("Reclaim pending")).toBeTruthy());
    fireEvent.click(screen.getByText("Reclaim pending"));

    // Two distinct hosts have a pending home (h-tower, h-node2).
    const dialog = await screen.findByRole("dialog", { name: "Reclaim pending homes" });
    expect(within(dialog).getByText(/Runs the cleanup job now on 2 hosts/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Reclaim" }));

    await waitFor(() => expect(adminApi.runJobNow).toHaveBeenCalledTimes(2));
    expect(adminApi.runJobNow).toHaveBeenCalledWith("token", "home.gc", { host_id: "h-tower" });
    expect(adminApi.runJobNow).toHaveBeenCalledWith("token", "home.gc", { host_id: "h-node2" });
    expect(adminApi.tombstoneHome).not.toHaveBeenCalled();
  });

  it("disables Reclaim pending, with a hint, when nothing is pending", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1", bytes_used: 100 })],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("Reclaim pending")).toBeTruthy());
    const btn = screen.getByText("Reclaim pending").closest("button")!;
    expect(btn).toBeDisabled();
    expect(btn.title).toMatch(/No homes are pending cleanup/);
  });

  it("reports a partial failure when queuing the cleanup job on some hosts fails", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [
        makeHome({ id: "h1", host_id: "h-tower", gc_after: "2026-02-01T00:00:00Z" }),
        makeHome({ id: "h2", host_id: "h-node2", gc_after: "2026-02-01T00:00:00Z" }),
      ],
      next_cursor: null,
    } as never);
    vi.mocked(adminApi.runJobNow)
      .mockResolvedValueOnce({ run_id: "run-1", state: "pending", scheduled_for: "2026-08-29T00:00:00Z" } as never)
      .mockRejectedValueOnce(new ApiError(409, "job_already_running", "conflict"));
    renderPage();

    await waitFor(() => expect(screen.getByText("Reclaim pending")).toBeTruthy());
    fireEvent.click(screen.getByText("Reclaim pending"));
    const dialog = await screen.findByRole("dialog", { name: "Reclaim pending homes" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Reclaim" }));

    await waitFor(() => expect(adminApi.runJobNow).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(addToastMock).toHaveBeenCalledWith(
        expect.objectContaining({ variant: "danger", title: expect.stringContaining("Queued cleanup on 1 of 2 hosts") }),
      ),
    );
  });

  it("opens no hosts read of its own — the allocated figure rides on the fleet poll", async () => {
    vi.mocked(adminApi.listAdminHomes).mockResolvedValue({
      items: [makeHome({ id: "h1" })],
      next_cursor: null,
    } as never);
    renderPage();

    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
    expect(adminApi.listHosts).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
