/**
 * StepHosts — the wizard's host/GPU truth-telling step. It must report real
 * control-plane state (registered hosts, detected encoders, capacity
 * misconfig) and degrade honestly: a failed per-host GPU fetch says so
 * rather than pretending the host has no GPUs.
 */

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { StepHosts } from "./StepHosts";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ApiError } from "../../api/client";
import type { Host } from "../../api/types";

vi.mock("../../api/admin", () => ({
  listHosts: vi.fn(),
  getHostGPUs: vi.fn(),
  getSettings: vi.fn(),
  getHostSettings: vi.fn(),
  updateHostSettings: vi.fn(),
}));

import * as adminApi from "../../api/admin";

function makeHost(overrides: Partial<Record<string, unknown>> = {}): Host {
  return {
    id: "h1",
    node_name: "tower",
    status: "online",
    last_registered_at: "2026-08-07T00:00:00Z",
    capacity_detection: "ok",
    capacity_reason: null,
    ...overrides,
  } as unknown as Host;
}

const gpu = {
  gpu_id: "g1",
  vendor: "nvidia",
  model: "RTX 5090",
  slots_total: 3,
  active_sessions: 1,
} as never;

function renderStep(onNext = vi.fn()) {
  const authValue: AuthContextValue = {
    status: "authenticated",
    user: { id: "u1", email: "admin@example.com", username: "admin", role: "admin" },
    token: "tok",
    isAdmin: true,
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
  render(
    <MemoryRouter>
      <AuthContext.Provider value={authValue}>
        <StepHosts onNext={onNext} />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
  return onNext;
}

describe("StepHosts", () => {
  beforeEach(() => {
    vi.mocked(adminApi.listHosts).mockReset();
    vi.mocked(adminApi.getHostGPUs).mockReset();
    vi.mocked(adminApi.getSettings).mockReset().mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings)
      .mockReset()
      .mockResolvedValue({ resolved: {}, overrides: {}, effective: null, pending_restart: false } as never);
    vi.mocked(adminApi.updateHostSettings).mockReset();
  });

  it("shows a loading state while the host list is in flight", () => {
    vi.mocked(adminApi.listHosts).mockReturnValue(new Promise(() => {}) as never);
    renderStep();

    expect(screen.getByText(/checking registered hosts/i)).toBeInTheDocument();
  });

  it("an empty host list shows the no-hosts warning with a link to /admin/fleet/hosts", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [] } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/no hosts have registered/i)).toBeInTheDocument();
    });
    const links = screen.getAllByRole("link", { name: /hosts/i });
    expect(links.length).toBeGreaterThan(0);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "/admin/fleet/hosts");
    }
  });

  it("a populated list renders hosts with their GPU/encoder detail", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText("tower")).toBeInTheDocument();
    });
    expect(screen.getByText(/rtx 5090/i)).toBeInTheDocument();
    expect(screen.getByText(/3 encode slots/i)).toBeInTheDocument();
    expect(screen.getByText(/1 active session\b/i)).toBeInTheDocument();
    expect(screen.getByText("Online")).toBeInTheDocument();
    // Healthy host, GPUs present — no error/warning surfaces.
    expect(screen.queryByText(/could not read gpu\/encoder detail/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/needs attention/i)).not.toBeInTheDocument();
  });

  it("a failed per-host GPU fetch shows the 'could not read GPU/encoder detail' message", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockRejectedValue(new ApiError(500, "internal", "gpu fetch broke"));
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/could not read gpu\/encoder detail/i)).toBeInTheDocument();
    });
    // It must NOT be conflated with "no GPU detected" — that claim is a lie
    // when the fetch simply failed.
    expect(screen.queryByText(/no gpu\/encoder detected/i)).not.toBeInTheDocument();
  });

  it("a host with capacity_detection !== 'ok' surfaces capacity_reason", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({
      items: [makeHost({ capacity_detection: "failed", capacity_reason: "nvml unavailable in container" })],
    } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText("nvml unavailable in container")).toBeInTheDocument();
    });
    // And the non-blocking "fix it later" warning appears alongside.
    expect(screen.getByText(/does not have to happen\s+now/i)).toBeInTheDocument();
  });

  // --- wizard-v2 §S4b/§S4c — storage driver truth-telling + constrained set-root ---

  it("§S4b: an 'auto' provider with an agent-reported root shows the local driver and the root", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "/data/quasar-homes" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/local — \/data\/quasar-homes/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/docker volume/i)).not.toBeInTheDocument();
  });

  // 2026-08-10: this test used to assert the OPPOSITE — that the same state
  // showed a "Docker volume" chip and the library warning. That was truthful
  // only while 'auto' silently downgraded to volumes. Now the launch fails
  // instead, so a calm volume chip here would be the wizard's single worst
  // lie: it would tell an operator their host is configured, in the one place
  // built to tell them it is not.
  it("§S4b: an 'auto' provider with NO reported root is called out as having no storage root, never as a docker volume", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getAllByText(/no storage root/i).length).toBeGreaterThan(0);
    });
    // It must say what will actually happen, and it must NOT reach for the
    // volume vocabulary — nothing is going to be volume-backed.
    expect(screen.getByText(/cannot start/i)).toBeInTheDocument();
    expect(screen.queryByText(/docker volume/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/never appear as quick-launch tiles/i)).not.toBeInTheDocument();
  });

  it("§S4c: offers the agent-reported effective home_root as the one-click recommended path", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "/data/quasar-homes" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /use this host's reported path/i })).toBeInTheDocument();
    });
    expect(screen.getByText("/data/quasar-homes")).toBeInTheDocument();
  });

  it("§S4c: with no reported root, there is no advanced entry — just the deploy/.env remedy", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/nothing to set a subdirectory of/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/QUASAR_HOME_ROOT in deploy\/\.env/i)).toBeInTheDocument();
    expect(screen.queryByText(/advanced: set a different path/i)).not.toBeInTheDocument();
  });

  it("§S4c: the advanced entry is a subdirectory field under the fixed reported root, and cannot hold an unrelated path", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "/data/quasar-homes" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/advanced: set a different path/i)).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText(/advanced: set a different path/i));

    // The root is shown as a fixed prefix, not part of the editable value.
    expect(screen.getByText("/data/quasar-homes/")).toBeInTheDocument();
    const input = await screen.findByPlaceholderText(/a subdirectory/i);
    expect(input).not.toBeDisabled();

    fireEvent.change(input, { target: { value: "instance-a" } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      expect(adminApi.updateHostSettings).toHaveBeenCalledWith("tok", "h1", {
        home_root: "/data/quasar-homes/instance-a",
      });
    });
  });

  it("§S4c: names deploy/.env + redeploy as the remedy for a genuinely different root", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { home_root: "/data/quasar-homes" },
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/advanced: set a different path/i)).toBeInTheDocument();
    });
    fireEvent.click(screen.getByText(/advanced: set a different path/i));

    expect(screen.getByText(/QUASAR_HOME_ROOT in deploy\/\.env/i)).toBeInTheDocument();
    expect(screen.getByText(/redeploy/i)).toBeInTheDocument();
  });

  it("§S4c: saving re-reads the host settings so the update round-trips into the wizard", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getSettings).mockResolvedValue({ settings: { storage_provider: "auto" } } as never);
    vi.mocked(adminApi.getHostSettings)
      .mockResolvedValueOnce({
        resolved: {},
        overrides: {},
        effective: { home_root: "/data/quasar-homes" },
        pending_restart: false,
      } as never)
      .mockResolvedValue({
        resolved: {},
        overrides: { home_root: "/data/quasar-homes" },
        effective: { home_root: "/data/quasar-homes" },
        pending_restart: false,
      } as never);
    vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
      resolved: {},
      overrides: { home_root: "/data/quasar-homes" },
      effective: { home_root: "/data/quasar-homes" },
      restart_triggered: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: /use this host's reported path/i })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /use this host's reported path/i }));

    await waitFor(() => {
      expect(adminApi.updateHostSettings).toHaveBeenCalledWith("tok", "h1", { home_root: "/data/quasar-homes" });
    });
    // The re-read (S4c's "surface a bad value immediately") happens after the save.
    expect(adminApi.getHostSettings).toHaveBeenCalledTimes(2);
    await waitFor(() => {
      expect(screen.getByText(/saved \/data\/quasar-homes/i)).toBeInTheDocument();
    });
  });

  // --- wizard-v2 §S5 — codec truth-telling (no toggle, by design) ---

  it("§S5: lists the codecs the HOST reported, and adds no control to change them", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { encoder: "nvenc" },
      codecs: ["h264", "h265", "av1"],
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/codecs this host reports/i)).toBeInTheDocument();
    });
    expect(screen.getByText("H.264")).toBeInTheDocument();
    expect(screen.getByText("HEVC")).toBeInTheDocument();
    expect(screen.getByText("AV1")).toBeInTheDocument();
    // A complete host has no gap to explain, and nothing to enable — §S5 is
    // explicit that no rung/profile control belongs in the wizard.
    expect(screen.queryByText(/QUASAR_VULKAN_HEVC/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /codec|enable|hevc|av1/i })).not.toBeInTheDocument();
  });

  it("§S5: an h264-only VULKAN host is told about the QUASAR_VULKAN_HEVC knob", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { encoder: "vulkan" },
      codecs: ["h264"],
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/QUASAR_VULKAN_HEVC/)).toBeInTheDocument();
    });
  });

  it("§S5: an h264-only NON-Vulkan host is told the element is missing, never that it is misconfigured", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { encoder: "va" },
      codecs: ["h264"],
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/not registered on this host/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/QUASAR_VULKAN_HEVC/)).not.toBeInTheDocument();
    expect(screen.queryByText(/misconfigur/i)).not.toBeInTheDocument();
  });

  it("§S5: the encoder comes from the AGENT-reported effective settings, not the `resolved` display view", async () => {
    // `resolved` is catalog-default <- overrides and cannot see the agent's env,
    // so a Vulkan host with no explicit encoder override reads openh264 there.
    // Reading it would suppress the one gap that has a real one-line fix.
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: { encoder: "openh264" },
      overrides: {},
      effective: { encoder: "vulkan" },
      codecs: ["h264"],
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/QUASAR_VULKAN_HEVC/)).toBeInTheDocument();
    });
  });

  it("§S5: a host that never reported codecs says so — it does not claim H.264-only", async () => {
    vi.mocked(adminApi.listHosts).mockResolvedValue({ items: [makeHost()] } as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [gpu] } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue({
      resolved: {},
      overrides: {},
      effective: { encoder: "vulkan" },
      codecs: null,
      pending_restart: false,
    } as never);
    renderStep();

    await waitFor(() => {
      expect(screen.getByText(/has not reported a codec set yet/i)).toBeInTheDocument();
    });
    // "not reported" is an unknown, not a gap — so no gap explanation fires,
    // even though the encoder is the one that has a known gate.
    expect(screen.queryByText(/QUASAR_VULKAN_HEVC/)).not.toBeInTheDocument();
  });
});
