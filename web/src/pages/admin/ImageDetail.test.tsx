/**
 * ImageDetail — v3 handoff §A.14 (pageImageDetail()). Sits outside the
 * Library section container (own crumbs + head, like the app editor).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ImageDetail } from "./ImageDetail";
import { AuthContext } from "../../auth/context";
import type { AuthContextValue } from "../../auth/context";
import { ToastProvider } from "../../components/Toast";
import { ApiError } from "../../api/client";
import type { AdminApp, CatalogImage, Host, RuntimePreset } from "../../api/types";

// The hosts come from the fleet poll AdminLayout mounts above this page, so the
// fixture is a stubbed context rather than a hosts read of its own — if the page
// ever opens one again, these fixtures stop reaching it.
let fleetHosts: Host[] = [];
vi.mock("../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({ hosts: fleetHosts, sessions: [] }),
}));

vi.mock("../../api/admin", () => ({
  listImages: vi.fn(),
  listRuntimePresets: vi.fn(),
  listAdminApps: vi.fn(),
  installImage: vi.fn(),
  uninstallImage: vi.fn(),
  pinImage: vi.fn(),
  unpinImage: vi.fn(),
  updateImage: vi.fn(),
  getSettings: vi.fn(),
}));

import * as adminApi from "../../api/admin";

const authValue: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
  token: "test-token",
  isAdmin: true,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function renderDetail(id = "steam") {
  return render(
    <MemoryRouter initialEntries={[`/admin/library/images/${id}`]}>
      <AuthContext.Provider value={authValue}>
        <ToastProvider>
          <Routes>
            <Route path="/admin/library/images/:id" element={<ImageDetail />} />
          </Routes>
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

function host(id: string, node_name: string): Host {
  return { id, node_name } as Host;
}

const steam: CatalogImage = {
  id: "steam",
  display_name: "Steam",
  description: "Valve's Steam client, discovered through the library provider.",
  kind: "prebuilt",
  version: "2026.08.07",
  registry_ref: "ghcr.io/quasar/steam:2026.08.07",
  registry_digest: "sha256:8f2a91c4e7b3",
  library_provider: "steam",
  installed: true,
  installed_version: "2026.08.07",
  pinned: false,
  update_available: false,
  hosts: [
    { host_id: "h1", node_name: "node-1", state: "ready", version: "2026.08.07", error: null },
    { host_id: "h2", node_name: "node-2", state: "ready", version: "2026.07.01", error: null },
  ],
};

const hosts = [host("h1", "node-1"), host("h2", "node-2"), host("h3", "node-3")];

function preset(over: Partial<RuntimePreset> = {}): RuntimePreset {
  return {
    id: "p1",
    name: "Proton GPU",
    image: "ghcr.io/quasar/steam:2026.08.07",
    env: {},
    mounts: [],
    used_by: [],
    ...over,
  } as RuntimePreset;
}

function app(over: Partial<AdminApp> = {}): AdminApp {
  return {
    id: "a1",
    name: "Steam",
    runtime_preset_id: null,
    runtime_spec: {},
    ...over,
  } as AdminApp;
}

beforeEach(() => {
  fleetHosts = hosts;
  vi.mocked(adminApi.listRuntimePresets).mockResolvedValue({ items: [] });
  vi.mocked(adminApi.listAdminApps).mockResolvedValue({ items: [], next_cursor: null } as never);
  vi.mocked(adminApi.getSettings).mockResolvedValue({
    settings: { image_update_policy: "auto" },
  } as never);
});

afterEach(() => {
  cleanup();
});

describe("ImageDetail", () => {
  it("renders the crumbs, head and facts", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    expect(screen.getByText("Library")).toBeInTheDocument();
    expect(screen.getByText("Images")).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 1, name: "Steam" })).toBeInTheDocument();
    expect(screen.getByText(/Prebuilt · from Steam · 1 of 3 hosts ready/)).toBeInTheDocument();

    expect(screen.getByText("ghcr.io/quasar/steam:2026.08.07")).toBeInTheDocument();
    expect(screen.getByText("sha256:8f2a91c4e7b3")).toBeInTheDocument();
    expect(screen.getByText("not tracked")).toBeInTheDocument();
  });

  it("marks a ready host whose version trails installed_version as stale in the per-host table", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    renderDetail();

    await waitFor(() => screen.getByText("node-2"));
    const row = screen.getByText("node-2").closest("tr")!;
    expect(within(row).getByText("stale")).toBeInTheDocument();

    const readyRow = screen.getByText("node-1").closest("tr")!;
    expect(within(readyRow).getByText("ready")).toBeInTheDocument();

    const absentRow = screen.getByText("node-3").closest("tr")!;
    expect(within(absentRow).getByText("not installed")).toBeInTheDocument();
  });

  it("lists presets by image ref and apps by image ref or inherited preset in Used by", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    vi.mocked(adminApi.listRuntimePresets).mockResolvedValue({ items: [preset()] });
    vi.mocked(adminApi.listAdminApps).mockResolvedValue({
      items: [
        app({ id: "a1", name: "Steam app", runtime_preset_id: "p1" }),
        app({ id: "a2", name: "Direct ref app", runtime_preset_id: null, runtime_spec: { image: steam.registry_ref } }),
        app({ id: "a3", name: "Unrelated app", runtime_preset_id: null, runtime_spec: {} }),
      ],
      next_cursor: null,
    } as never);
    renderDetail();

    await waitFor(() => screen.getByText("Proton GPU"));
    expect(screen.getByText("Steam app")).toBeInTheDocument();
    expect(screen.getByText("Direct ref app")).toBeInTheDocument();
    expect(screen.queryByText("Unrelated app")).not.toBeInTheDocument();
  });

  it("shows the 'nothing points at this image' note when unused", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    expect(screen.getByText(/Nothing points at this image/)).toBeInTheDocument();
    expect(screen.getByText(/Uninstalling it reclaims the space on every host/)).toBeInTheDocument();
  });

  it("lead action is 'Update to {version}' when installed and an update is available", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [{ ...steam, update_available: true, version: "2026.09.01" }] } as never);
    vi.mocked(adminApi.updateImage).mockResolvedValue({ applied: true, image: { ...steam, update_available: false } });
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    const btn = screen.getByRole("button", { name: /Update to 2026.09.01/ });
    fireEvent.click(btn);

    await waitFor(() => {
      expect(adminApi.updateImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("a pinned image with an update never shows Update to X - falls back to Re-ensure (409 image is pinned)", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({
      images: [{ ...steam, update_available: true, pinned: true, version: "2026.09.01" }],
    } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    expect(screen.queryByRole("button", { name: /Update to/ })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Re-ensure" })).toBeInTheDocument();
  });

  it("reads the real Update policy from settings, not a hardcoded Auto", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({
      settings: { image_update_policy: "manual" },
    } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    expect(await screen.findByText("Manual")).toBeInTheDocument();
    expect(screen.queryByText("Auto")).not.toBeInTheDocument();
  });

  it("a stale host Version column shows the version it is running, not the image installed_version", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    renderDetail();

    await waitFor(() => screen.getByText("node-2"));
    const staleRow = screen.getByText("node-2").closest("tr")!;
    // steam.installed_version is 2026.08.07; node-2 own reported version is 2026.07.01.
    expect(within(staleRow).getByText("2026.07.01")).toBeInTheDocument();
    expect(within(staleRow).queryByText("2026.08.07")).not.toBeInTheDocument();
  });

  it("Sessions is the host own active session count", async () => {
    const hostsWithSessions = [
      {
        ...host("h1", "node-1"),
        capacity: { slots_total: 4, slots_used: 1, vram_mb_total: 0, vram_mb_used: 0, active_sessions: 3, gpu_count: 1 },
      } as Host,
      host("h2", "node-2"),
      host("h3", "node-3"),
    ];
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    fleetHosts = hostsWithSessions;
    renderDetail();

    await waitFor(() => screen.getByText("node-1"));
    const row = screen.getByText("node-1").closest("tr")!;
    expect(within(row).getByText("3")).toBeInTheDocument();
  });

  it("installing offers a Pull on first launch switch, and honours it", async () => {
    const notInstalled: CatalogImage = { ...steam, installed: false, installed_version: null, hosts: [] };
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [notInstalled] } as never);
    vi.mocked(adminApi.installImage).mockResolvedValue({ ...notInstalled, installed: true, lazy: true });
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    fireEvent.click(screen.getByLabelText("Pull on first launch"));
    fireEvent.click(screen.getByRole("button", { name: "Install on every host" }));

    await waitFor(() => {
      expect(adminApi.installImage).toHaveBeenCalledWith("test-token", "steam", { lazy: true });
    });
  });


  it("lead action is a disabled 'Install on every host' for a not-installed template image", async () => {
    const template: CatalogImage = { ...steam, installed: false, installed_version: null, kind: "template", hosts: [] };
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [template] } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    const btn = screen.getByRole("button", { name: "Install on every host" });
    expect(btn).toBeDisabled();
  });

  it("lead action is ghost 'Re-ensure' when installed with no update", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    vi.mocked(adminApi.installImage).mockResolvedValue({ ...steam });
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    fireEvent.click(screen.getByRole("button", { name: "Re-ensure" }));

    await waitFor(() => {
      expect(adminApi.installImage).toHaveBeenCalledWith("test-token", "steam", { lazy: false });
    });
  });

  it("pins and unpins from the rail action", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    vi.mocked(adminApi.pinImage).mockResolvedValue(undefined);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    fireEvent.click(screen.getByRole("button", { name: "Pin version" }));

    await waitFor(() => {
      expect(adminApi.pinImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("uninstalls everywhere via the rail button and its confirm modal", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    vi.mocked(adminApi.uninstallImage).mockResolvedValue(undefined);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    fireEvent.click(screen.getByRole("button", { name: "Uninstall everywhere" }));

    const dialog = await screen.findByRole("dialog");
    expect(adminApi.uninstallImage).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: /^uninstall$/i }));

    await waitFor(() => {
      expect(adminApi.uninstallImage).toHaveBeenCalledWith("test-token", "steam");
    });
  });

  it("shows the rail's rollout bar and state facts", async () => {
    vi.mocked(adminApi.listImages).mockResolvedValue({ images: [steam] } as never);
    renderDetail();

    await waitFor(() => screen.getAllByText("Steam")[0]);
    expect(screen.getByText("1/3")).toBeInTheDocument();
    expect(screen.getByText("up to date")).toBeInTheDocument();
    expect(screen.getByText("no")).toBeInTheDocument();
  });

  it("reports a load failure as an alert", async () => {
    vi.mocked(adminApi.listImages).mockRejectedValue(new ApiError(500, "internal", "server error"));
    renderDetail();

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("server error");
    });
  });
});
