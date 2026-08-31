import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "token" }) }));
vi.mock("../../../components/Toast", () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock("../../../api/admin", () => ({
  getHost: vi.fn(), getConsoleConfig: vi.fn(), listAdminApps: vi.fn(),
  listUsers: vi.fn(), updateConsoleConfig: vi.fn(),
}));

import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import { HostConsole } from "./HostConsole";

describe("HostConsole truthful topology", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(adminApi.getHost).mockResolvedValue({
      host: { id: "host-1", node_name: "Tower", status: "online" },
    } as never);
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      config: {
        enabled: false, connector: "auto", compositor: "weston", audio_output: null,
        stream: false, stream_audio: false, input_devices: "auto", grab: true,
        auto_start_on_display: false, auto_connect_controller: false,
        default_app: null, default_user: null, fullscreen: true,
      },
      capabilities: {
        connectors: ["DP-4"],
        audio_sinks: [{ id: "hw:0,3", label: "NVIDIA HDA — HDMI 0" }],
        input_devices: [
          { path: "/dev/input/event3", label: "Keychron K2 Keyboard" },
          { path: "/dev/input/event5", label: "Logitech G502 Mouse" },
        ],
        outputs: [{
          id: "card1:DP-4", card: "card1", render_node: "/dev/dri/renderD128",
          connector: "DP-4", connected: true, active_mode: null,
          modes: [{ name: "2560x1440", width: 2560, height: 1440, refresh_millihz: 119880,
            preferred: true, interlaced: false, clock_khz: 497750, htotal: 2720, vtotal: 1526 }],
        }],
      },
    } as never);
    vi.mocked(adminApi.listAdminApps).mockResolvedValue({ items: [] } as never);
    vi.mocked(adminApi.listUsers).mockResolvedValue({ items: [] } as never);
  });

  function renderPage() {
    return render(
      <MemoryRouter initialEntries={["/admin/fleet/hosts/host-1/console"]}>
        <Routes><Route path="/admin/fleet/hosts/:id/console" element={<HostConsole />} /></Routes>
      </MemoryRouter>,
    );
  }

  it("offers local-only and dual-output controls while retaining fixed display choices", async () => {
    renderPage();

    await waitFor(() => expect(screen.getByText("Video topology")).toBeTruthy());
    expect(screen.getByText("Weston · Static mode · Fullscreen")).toBeTruthy();
    expect(screen.getAllByText("card1:DP-4")).toHaveLength(2);
    expect(screen.getByText("2560×1440 @ 119.880 Hz")).toBeTruthy();
    expect(screen.getByText("Physical output")).toBeTruthy();
    expect(screen.getByText("Physical mode")).toBeTruthy();
    expect(screen.queryByText("Display connector")).toBeNull();
    expect(screen.queryByText("Compositor")).toBeNull();
    expect(screen.getByText("Also stream")).toBeTruthy();
    expect(screen.getByText("Stream audio")).toBeTruthy();
    expect(screen.queryByText("Fullscreen")).toBeNull();
  });

  it("crumbs to Fleet and the host, and heads with the mock's title/sub", async () => {
    renderPage();

    await waitFor(() => expect(screen.getByRole("heading", { name: "Local console" })).toBeTruthy());
    expect(screen.getByText("Fleet")).toBeTruthy();
    expect(screen.getByText(/Local display on Tower with an explicit per-session output topology/)).toBeTruthy();
  });

  it("disables Discard and Save changes while the draft is clean, and enables them once dirty", async () => {
    renderPage();

    const enabled = await screen.findByRole("switch", { name: "Enabled" });
    expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();

    fireEvent.click(enabled);
    expect(screen.getByRole("button", { name: "Discard" })).not.toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).not.toBeDisabled();
  });

  it("Discard resets the draft without saving", async () => {
    renderPage();

    const enabled = await screen.findByRole("switch", { name: "Enabled" });
    fireEvent.click(enabled);
    expect(enabled.getAttribute("aria-checked")).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(enabled.getAttribute("aria-checked")).toBe("false");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(adminApi.updateConsoleConfig).not.toHaveBeenCalled();
  });

  it("disables console mode and persists the change", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      ...current,
      config: { ...current.config, enabled: true, auto_start_on_display: true },
    } as never);
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue({
      ...current,
      config: { ...current.config, enabled: false, auto_start_on_display: true },
    } as never);

    renderPage();

    const enabled = await screen.findByRole("switch", { name: "Enabled" });
    expect(enabled.getAttribute("aria-checked")).toBe("true");
    fireEvent.click(enabled);
    expect(enabled.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { enabled: false },
    ));
    await waitFor(() => expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled());
  });

  it("selects a reported sound card for console-only playback", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue({
      ...current,
      config: { ...current.config, audio_output: "hw:0,3" },
    } as never);

    renderPage();

    const output = await screen.findByRole("combobox", { name: "Local audio output" });
    expect(screen.getByRole("option", { name: "NVIDIA HDA — HDMI 0" })).toBeTruthy();
    fireEvent.change(output, { target: { value: "hw:0,3" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { audio_output: "hw:0,3" },
    ));
  });

  it("#521: shows the ApiError message alone, never the machine code prefix", async () => {
    vi.mocked(adminApi.getHost).mockRejectedValue(
      new ApiError(400, "validation_failed", "host id is malformed"),
    );

    renderPage();

    expect(await screen.findByText("host id is malformed")).toBeTruthy();
    expect(screen.queryByText(/validation_failed:/)).toBeNull();
  });

  it("shows the input-device segmented control, class chips and device table", async () => {
    renderPage();

    await screen.findByText("Input devices");
    expect(screen.getByRole("tab", { name: "Auto · by class" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "Specific devices" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "None" })).toBeTruthy();
    // Auto mode passes every reported device through.
    expect(screen.getByText("Keychron K2 Keyboard")).toBeTruthy();
    expect(screen.getAllByText("passed through")).toHaveLength(2);
  });

  it("switching to Specific devices lets an individual device be deselected, and the change is saved as an explicit path list", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);

    renderPage();

    await screen.findByText("Input devices");
    fireEvent.click(screen.getByRole("tab", { name: "Specific devices" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Pass through Logitech G502 Mouse" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { input_devices: ["/dev/input/event3"] },
    ));
  });

  it("the capabilities rail reports how many devices are passed through", async () => {
    renderPage();

    await waitFor(() => expect(screen.getByText(/Input devices: 2 reported/)).toBeTruthy());
    expect(screen.getByText(/2 passed through/)).toBeTruthy();
  });

  it("switching to None passes through no devices and saves an empty path list", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);

    renderPage();

    await screen.findByText("Input devices");
    fireEvent.click(screen.getByRole("tab", { name: "None" }));
    expect(screen.getAllByText("not passed")).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { input_devices: [] },
    ));
  });

  it("switching to Specific and back to Auto saves the wire value \"auto\", not a device list", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);

    renderPage();

    await screen.findByText("Input devices");
    fireEvent.click(screen.getByRole("tab", { name: "Specific devices" }));
    fireEvent.click(screen.getByRole("tab", { name: "Auto · by class" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { input_devices: "auto" },
    ));
  });

  it("a class chip bulk-toggles every device of that class in Specific mode", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      ...current,
      capabilities: {
        ...current.capabilities,
        input_devices: [
          ...current.capabilities.input_devices,
          { path: "/dev/input/event9", label: "Xbox Wireless Controller" },
          { path: "/dev/input/event10", label: "8BitDo Controller" },
        ],
      },
    } as never);
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);

    renderPage();

    await screen.findByText("Input devices");
    fireEvent.click(screen.getByRole("tab", { name: "Specific devices" }));
    // Specific mode starts seeded with every reported device passed through.
    expect(screen.getAllByText("passed through")).toHaveLength(4);

    fireEvent.click(screen.getByRole("button", { name: /Controllers/ }));
    expect(screen.getAllByText("passed through")).toHaveLength(2);
    expect(screen.getByRole("checkbox", { name: "Pass through Xbox Wireless Controller" })).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Pass through 8BitDo Controller" })).not.toBeChecked();

    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { input_devices: ["/dev/input/event3", "/dev/input/event5"] },
    ));
  });
});
