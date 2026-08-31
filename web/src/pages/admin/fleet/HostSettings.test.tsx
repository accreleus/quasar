import { act, configure, fireEvent, render, screen, waitFor } from "@testing-library/react";

// The page resolves two reads and a knob catalog before the first assertion can
// pass; under a loaded box the default 1 s async timeout is not enough.
configure({ asyncUtilTimeout: 10000 });
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "token" }) }));
vi.mock("../../../api/admin", () => ({
  getHost: vi.fn(),
  getConfigCatalog: vi.fn(),
  getHostSettings: vi.fn(),
  getHostGPUs: vi.fn(),
  updateHostSettings: vi.fn(),
  restartHost: vi.fn(),
}));

let fleetSessions: { id: string; host_id: string | null }[] = [];
vi.mock("../../../lib/fleet/FleetContext", () => ({
  useFleetContext: () => ({
    hosts: [],
    sessions: fleetSessions,
    loading: false,
    lastFetchedAt: 0,
    errors: { hosts: null, sessions: null },
    reload: vi.fn(),
  }),
}));

import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import type { ConfigKnob, Host } from "../../../api/types";
import { HostSettings } from "./HostSettings";

function knob(over: Partial<ConfigKnob>): ConfigKnob {
  return {
    key: "some_key",
    type: "string",
    default: null,
    nullable: true,
    class: "live",
    env_var: "QUASAR_SOME_KEY",
    ...over,
  } as ConfigKnob;
}

const KNOBS: ConfigKnob[] = [
  knob({ key: "idle_timeout_secs", type: "int", default: 1800, class: "live", env_var: "QUASAR_IDLE_TIMEOUT_SECS" }),
  knob({
    key: "encoder", type: "enum", enum: ["nvenc", "va", "software"], default: "va",
    class: "restart", env_var: "QUASAR_ENCODER",
  }),
  knob({ key: "gop", type: "int", default: 120, class: "live", env_var: "QUASAR_GOP" }),
];

const HOST: Host = {
  id: "host-1",
  node_name: "Tower",
  status: "online",
  agent_version: "1.4.0",
  cpu_cores: 16,
  mem_mb: 65536,
  cpu_model: "AMD Ryzen 9",
  last_registered_at: "2026-08-01T00:00:00Z",
  last_heartbeat_at: "2026-08-29T11:58:00Z",
  storage: null,
  capacity_detection: "ok",
  capacity_reason: null,
  readiness: null,
  readiness_reported_at: null,
  capacity: { slots_total: 4, slots_used: 1, vram_mb_total: 24576, vram_mb_used: 4096 },
  agent_connected_since: null,
  agent_restart_count: 0,
  agent_last_restart_at: null,
} as unknown as Host;

function settingsResponse(over: Partial<{
  resolved: Record<string, boolean | number | string>;
  overrides: Record<string, boolean | number | string>;
  pending_restart: boolean;
  effective: Record<string, string> | null;
}> = {}) {
  return {
    resolved: { idle_timeout_secs: 1800, encoder: "va", gop: 120 },
    overrides: {},
    pending_restart: false,
    effective: null,
    ...over,
  };
}

describe("HostSettings", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    fleetSessions = [];
    vi.mocked(adminApi.getHost).mockResolvedValue({ host: HOST } as never);
    vi.mocked(adminApi.getConfigCatalog).mockResolvedValue({ knobs: KNOBS } as never);
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse() as never);
    vi.mocked(adminApi.getHostGPUs).mockResolvedValue({ items: [] } as never);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  function renderPage() {
    return render(
      <MemoryRouter initialEntries={["/admin/fleet/hosts/host-1/settings"]}>
        <Routes><Route path="/admin/fleet/hosts/:id/settings" element={<HostSettings />} /></Routes>
      </MemoryRouter>,
    );
  }

  it("crumbs to Fleet and the host, heads with the mock's copy, and renders the knob catalog", async () => {
    renderPage();

    await waitFor(() => expect(screen.getByRole("heading", { name: "Host settings" })).toBeTruthy());
    expect(screen.getByText("Fleet")).toBeTruthy();
    expect(screen.getByText(/Runtime configuration for Tower/)).toBeTruthy();
    expect(screen.getByText("Runtime defaults")).toBeTruthy();
    expect(screen.getByText("Encoder and GPU")).toBeTruthy();
    expect(screen.getByText("Idle timeout")).toBeTruthy();
    // Encoder is restart-class: its row carries the "restart" chip.
    const encoderRow = screen.getByText("Encoder").closest(".cset") as HTMLElement;
    expect(encoderRow.textContent).toMatch(/restart/);
  });

  it("Discard and Save changes are disabled while the draft is clean", async () => {
    renderPage();

    await screen.findByText("Idle timeout");
    expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("edits a live knob, saves it as a PATCH override, and shows no restart confirm", async () => {
    vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
      resolved: { idle_timeout_secs: 900, encoder: "va", gop: 120 },
      overrides: { idle_timeout_secs: 900 },
      restart_triggered: false,
      effective: null,
    } as never);

    renderPage();
    await screen.findByText("Idle timeout");

    const input = screen.getByRole("spinbutton", { name: "idle_timeout_secs" });
    fireEvent.change(input, { target: { value: "900" } });
    expect(screen.getByRole("button", { name: "Save changes" })).not.toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateHostSettings).toHaveBeenCalledWith(
      "token", "host-1", { idle_timeout_secs: 900 }, false,
    ));
    expect(adminApi.restartHost).not.toHaveBeenCalled();
  });

  it("shows the proactive restart-class note as soon as a restart-class knob is dirty, worded from live sessions on this host", async () => {
    fleetSessions = [
      { id: "s1", host_id: "host-1" },
      { id: "s2", host_id: "host-1" },
      { id: "s3", host_id: "other-host" },
    ];
    renderPage();
    await screen.findByText("Idle timeout");

    const encoderSelect = screen.getByRole("combobox", { name: "encoder" });
    fireEvent.change(encoderSelect, { target: { value: "nvenc" } });

    await waitFor(() => expect(screen.getByText(/1 restart-class change is pending/)).toBeTruthy());
    // Only this host's two live sessions are counted, not the third host's.
    expect(screen.getByText(/ends 2 live sessions on this host/)).toBeTruthy();
  });

  it("requires a second Save click to confirm a restart-class change, then PATCHes with restart_confirm", async () => {
    vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
      resolved: { idle_timeout_secs: 1800, encoder: "nvenc", gop: 120 },
      overrides: { encoder: "nvenc" },
      restart_triggered: true,
      effective: null,
    } as never);

    renderPage();
    await screen.findByText("Idle timeout");

    fireEvent.change(screen.getByRole("combobox", { name: "encoder" }), { target: { value: "nvenc" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    // First click only opens the confirm — no PATCH yet.
    expect(adminApi.updateHostSettings).not.toHaveBeenCalled();
    const confirmButton = await screen.findByRole("button", { name: "Save & restart" });
    fireEvent.click(confirmButton);

    await waitFor(() => expect(adminApi.updateHostSettings).toHaveBeenCalledWith(
      "token", "host-1", { encoder: "nvenc" }, true,
    ));
  });

  it("reset to default clears the override and PATCHes it back to null", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse({
      overrides: { idle_timeout_secs: 60 },
    }) as never);
    vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
      resolved: { idle_timeout_secs: 1800, encoder: "va", gop: 120 },
      overrides: {},
      restart_triggered: false,
      effective: null,
    } as never);

    renderPage();
    await screen.findByText("Idle timeout");

    expect(screen.getByText("overridden")).toBeTruthy();
    fireEvent.click(screen.getByRole("link", { name: "reset to default" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateHostSettings).toHaveBeenCalledWith(
      "token", "host-1", { idle_timeout_secs: null }, false,
    ));
  });

  it("Discard clears the draft without saving", async () => {
    renderPage();
    await screen.findByText("Idle timeout");

    const input = screen.getByRole("spinbutton", { name: "idle_timeout_secs" });
    fireEvent.change(input, { target: { value: "5" } });
    expect(input).toHaveValue(5);

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(input).toHaveValue(1800);
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(adminApi.updateHostSettings).not.toHaveBeenCalled();
  });

  it("a host loaded with persisted overrides shows them as overridden but starts clean, not dirty", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse({
      overrides: { idle_timeout_secs: 60 },
    }) as never);

    renderPage();
    await screen.findByText("Idle timeout");

    // The override survived reload and renders as such...
    expect(screen.getByText("overridden")).toBeTruthy();
    expect(screen.getByRole("spinbutton", { name: "idle_timeout_secs" })).toHaveValue(60);
    // ...but it is the saved state, not an unsaved draft: gating must not
    // read "has any override" as dirty.
    expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("Discard on a host with persisted overrides reverts to the saved override, not the catalog default", async () => {
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse({
      overrides: { idle_timeout_secs: 60 },
    }) as never);

    renderPage();
    await screen.findByText("Idle timeout");

    const input = screen.getByRole("spinbutton", { name: "idle_timeout_secs" });
    fireEvent.change(input, { target: { value: "5" } });
    expect(screen.getByRole("button", { name: "Discard" })).not.toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(input).toHaveValue(60); // the persisted override, not the 1800 default
    expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(adminApi.updateHostSettings).not.toHaveBeenCalled();
  });

  it("polls getHostSettings after a triggered restart until pending_restart clears", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(adminApi.updateHostSettings).mockResolvedValue({
        resolved: { idle_timeout_secs: 1800, encoder: "nvenc", gop: 120 },
        overrides: { encoder: "nvenc" },
        restart_triggered: true,
        effective: null,
      } as never);
      // Initial load has no persisted override yet, so selecting "nvenc" below is
      // a genuine draft change (not a no-op re-selection of an already-saved
      // value, which the saved-vs-draft dirty check would correctly ignore).
      // First re-fetch after the restart still shows pending; second clears it.
      vi.mocked(adminApi.getHostSettings)
        .mockResolvedValueOnce(settingsResponse() as never)
        .mockResolvedValueOnce(settingsResponse({ overrides: { encoder: "nvenc" }, pending_restart: true }) as never)
        .mockResolvedValueOnce(settingsResponse({ overrides: { encoder: "nvenc" }, pending_restart: false }) as never);

      renderPage();
      await act(async () => { await vi.advanceTimersByTimeAsync(0); }); // flush the initial Promise.all load
      expect(screen.getByText("Idle timeout")).toBeTruthy();

      fireEvent.change(screen.getByRole("combobox", { name: "encoder" }), { target: { value: "nvenc" } });
      fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
      fireEvent.click(screen.getByRole("button", { name: "Save & restart" }));
      await act(async () => { await vi.advanceTimersByTimeAsync(0); }); // flush the save() PATCH

      expect(adminApi.updateHostSettings).toHaveBeenCalled();
      expect(adminApi.getHostSettings).toHaveBeenCalledTimes(1); // just the initial load so far

      await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
      expect(adminApi.getHostSettings).toHaveBeenCalledTimes(2);

      await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
      expect(adminApi.getHostSettings).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  it("the rail's Restart agent now button restarts the agent directly when no confirm is required", async () => {
    vi.mocked(adminApi.restartHost).mockResolvedValue({ restart_triggered: true } as never);

    renderPage();
    await screen.findByText("Idle timeout");

    fireEvent.click(screen.getByRole("button", { name: "Restart agent now" }));

    await waitFor(() => expect(adminApi.restartHost).toHaveBeenCalledWith("token", "host-1", false));
  });

  it("the rail's Restart agent now button asks for confirmation when sessions are live, wording the count", async () => {
    vi.mocked(adminApi.restartHost).mockRejectedValueOnce(
      new ApiError(409, "restart_required", "restart requires confirmation", 3),
    );

    renderPage();
    await screen.findByText("Idle timeout");

    fireEvent.click(screen.getByRole("button", { name: "Restart agent now" }));
    expect(await screen.findByText(/drop 3 live sessions on this host/)).toBeTruthy();
  });

  it("the rail reports Agent, Heartbeat and live-session counts, and Pending restart", async () => {
    fleetSessions = [{ id: "s1", host_id: "host-1" }];
    vi.mocked(adminApi.getHostSettings).mockResolvedValue(settingsResponse({ pending_restart: true }) as never);

    renderPage();
    await screen.findByText("Idle timeout");

    expect(screen.getByText("1.4.0")).toBeTruthy();
    const liveSessionsRow = screen.getByText("Live sessions").closest("div") as HTMLElement;
    expect(liveSessionsRow.textContent).toMatch(/1/);
    expect(screen.getByText("yes")).toBeTruthy();
  });

  it("#521: shows the ApiError message alone, never the machine code prefix", async () => {
    vi.mocked(adminApi.getHost).mockRejectedValue(
      new ApiError(400, "validation_failed", "host id is malformed"),
    );

    renderPage();

    expect(await screen.findByText("host id is malformed")).toBeTruthy();
    expect(screen.queryByText(/validation_failed:/)).toBeNull();
  });
});
