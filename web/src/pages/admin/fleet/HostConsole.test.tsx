import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { addToastMock } = vi.hoisted(() => ({ addToastMock: vi.fn() }));
vi.mock("../../../auth/context", () => ({ useAuth: () => ({ token: "token" }) }));
vi.mock("../../../components/Toast", () => ({ useToast: () => ({ addToast: addToastMock }) }));
vi.mock("../../../api/admin", () => ({
  getHost: vi.fn(), getConsoleConfig: vi.fn(),
  listUsers: vi.fn(), updateConsoleConfig: vi.fn(),
}));

import * as adminApi from "../../../api/admin";
import { ApiError } from "../../../api/client";
import { clockTime } from "../../../lib/format/clockTime";
import { HostConsole } from "./HostConsole";

const KDE_ID = "6f1c0000-0000-0000-0000-000000000001";
const STEAM_ID = "9a070000-0000-0000-0000-000000000002";
const NESTED_ID = "3c4d0000-0000-0000-0000-000000000003";

describe("HostConsole direct display (amendment 19)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    addToastMock.mockClear();
    vi.mocked(adminApi.getHost).mockResolvedValue({
      host: { id: "host-1", node_name: "lab-host", status: "online", capacity: { active_sessions: 1 } },
    } as never);
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      config: {
        enabled: false, output_id: null, input_devices: "auto",
        auto_start_on_display: false, default_app: null, default_user: null,
      },
      capabilities: {
        connectors: ["DP-4"],
        input_devices: [
          { path: "/dev/input/event3", label: "Keychron K2 Keyboard" },
          { path: "/dev/input/event5", label: "Logitech G502 Mouse" },
        ],
        outputs: [{
          id: "card1:DP-4", card: "card1", render_node: "/dev/dri/renderD128",
          connector: "DP-4", connected: true, active_mode: null,
          modes: [{ name: "2560x1440", width: 2560, height: 1440, refresh_millihz: 119880,
            preferred: true, interlaced: false, clock_khz: 497750, htotal: 2720, vtotal: 1526 }],
        }, {
          id: "card1:HDMI-A-1", card: "card1", render_node: "/dev/dri/renderD128",
          connector: "HDMI-A-1", connected: false, active_mode: null, modes: [],
        }],
      },
      default_apps: [{ id: KDE_ID, name: "KDE Plasma" }, { id: STEAM_ID, name: "Steam" }],
      readiness: [{
        id: "console_default_app", status: "skip", source: "operator",
        summary: "No default app is set, so console mode has nothing to run.", remediation: "",
      }],
    } as never);
    vi.mocked(adminApi.listUsers).mockResolvedValue({ items: [] } as never);
  });

  function renderPage() {
    return render(
      <MemoryRouter initialEntries={["/admin/fleet/hosts/host-1/console"]}>
        <Routes><Route path="/admin/fleet/hosts/:id/console" element={<HostConsole />} /></Routes>
      </MemoryRouter>,
    );
  }

  it("edits only the six settings that survive direct display", async () => {
    renderPage();

    await screen.findByRole("switch", { name: "Enabled" });
    expect(screen.getByRole("combobox", { name: "Physical output" })).toBeTruthy();
    expect(screen.getByText("Input devices")).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Default app" })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "Default user" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "Auto-start on display" })).toBeTruthy();
    // Retired by amendment 19: the desktop owns these now.
    for (const gone of ["Video topology", "Physical mode", "Also stream", "Stream audio",
      "Local audio output", "Grab local input", "Auto-connect controller"]) {
      expect(screen.queryByText(gone)).toBeNull();
    }
    expect(screen.queryByText(/Audio sinks/)).toBeNull();
  });

  it("offers every reported output, a monitorless one included, and saves only output_id", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);
    renderPage();

    const output = await screen.findByRole("combobox", { name: "Physical output" });
    expect(screen.getByRole("option", { name: "Automatic" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "card1:DP-4" })).toBeTruthy();
    expect(screen.getByRole("option", { name: "card1:HDMI-A-1 · no monitor" })).toBeTruthy();
    fireEvent.change(output, { target: { value: "card1:HDMI-A-1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { output_id: "card1:HDMI-A-1" },
    ));
  });

  it("the default-app select offers only the apps the server says can run direct", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    vi.mocked(adminApi.updateConsoleConfig).mockResolvedValue(current as never);
    renderPage();

    const select = await screen.findByRole("combobox", { name: "Default app" });
    const options = Array.from((select as HTMLSelectElement).options).map((o) => o.textContent);
    expect(options).toEqual(["None", "KDE Plasma", "Steam"]);
    fireEvent.change(select, { target: { value: STEAM_ID } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { default_app: STEAM_ID },
    ));
  });

  it("a saved default app that cannot run direct shows the readiness failure as the row's help", async () => {
    const current = await adminApi.getConsoleConfig("token", "host-1");
    const summary = "The console's default app Old Desktop cannot run direct: its runtime spec does " +
      "not declare direct_display, so console mode will not launch it.";
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      ...current,
      config: { ...current.config, default_app: NESTED_ID },
      readiness: [{
        id: "console_default_app", status: "fail", source: "operator", summary,
        remediation: "Pick a default app from the console page's list.",
      }],
    } as never);
    renderPage();

    expect(await screen.findByText(summary)).toBeTruthy();
    const select = screen.getByRole("combobox", { name: "Default app" }) as HTMLSelectElement;
    expect(select.value).toBe(NESTED_ID);
    expect(screen.getByRole("option", { name: "Current app (cannot run direct)" })).toBeTruthy();

    // Picking a direct app replaces the failure with the ordinary help.
    fireEvent.change(select, { target: { value: KDE_ID } });
    expect(screen.queryByText(summary)).toBeNull();
  });

  it("crumbs to Fleet and the host, and heads with the mock's title/sub", async () => {
    renderPage();

    await waitFor(() => expect(screen.getByRole("heading", { name: "Local console" })).toBeTruthy());
    expect(screen.getByText("Fleet")).toBeTruthy();
    expect(screen.getByText(/The console desktop drives lab-host's own display/)).toBeTruthy();
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

// Amendment 18 — console access on owned hosts (RH07 #395). A host whose
// agent reports `capabilities.access` gets a confirm-then-PATCH switch, a
// note above the panel, and (on a failed, restored attempt) a Try again
// button — none of which a host with no `access` sees (covered above).
describe("HostConsole console access (amendment 18)", () => {
  const BASE_CONFIG = {
    enabled: false, output_id: null, input_devices: "auto",
    auto_start_on_display: false, default_app: null, default_user: null,
  };
  const BASE_CAPS = {
    connectors: [], input_devices: [],
  };

  function mockAccess(access: Record<string, unknown>, configOverrides: Record<string, unknown> = {}) {
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      config: { ...BASE_CONFIG, ...configOverrides },
      capabilities: { ...BASE_CAPS, access },
      default_apps: [],
      readiness: [],
    } as never);
  }

  beforeEach(() => {
    vi.clearAllMocks();
    addToastMock.mockClear();
    vi.mocked(adminApi.getHost).mockResolvedValue({
      host: { id: "host-1", node_name: "lab-host", status: "online", capacity: { active_sessions: 2 } },
    } as never);
    vi.mocked(adminApi.listUsers).mockResolvedValue({ items: [] } as never);
  });

  function renderPage() {
    return render(
      <MemoryRouter initialEntries={["/admin/fleet/hosts/host-1/console"]}>
        <Routes><Route path="/admin/fleet/hosts/:id/console" element={<HostConsole />} /></Routes>
      </MemoryRouter>,
    );
  }

  it("off: shows the Off chip, an off note, and opens the confirm modal naming live sessions", async () => {
    mockAccess({
      state: "off", target: null, request_id: null, reason: null,
      started_at: null, finished_at: null, summary: "Console mode is off.",
    });
    renderPage();

    await screen.findByText("Console mode is off.");
    expect(screen.getByText("Off")).toBeTruthy();
    expect(screen.getByText("This machine shows games on its own screen.")).toBeTruthy();

    fireEvent.click(await screen.findByRole("switch", { name: "Enabled" }));

    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.getByText("Turn on console mode on lab-host?")).toBeTruthy();
    expect(screen.getByText("2 live sessions")).toBeTruthy();
    expect(adminApi.updateConsoleConfig).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Turn on console mode" }));
    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { enabled: true },
    ));
  });

  it("confirm modal: Cancel closes without PATCHing", async () => {
    mockAccess({
      state: "off", target: null, request_id: null, reason: null,
      started_at: null, finished_at: null, summary: "Console mode is off.",
    });
    renderPage();

    fireEvent.click(await screen.findByRole("switch", { name: "Enabled" }));
    await screen.findByRole("dialog");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).toBeNull();
    expect(adminApi.updateConsoleConfig).not.toHaveBeenCalled();
  });

  it("applying: locks the switch, shows the started time, and locks every other control", async () => {
    mockAccess(
      {
        state: "applying", target: true, request_id: "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: null, started_at: "2026-09-29T14:12:03Z", finished_at: null,
        summary: "Replacing the node agent.",
      },
      { enabled: true },
    );
    renderPage();

    await screen.findByText("Turning on console mode.");
    expect(screen.getByText("Applying")).toBeTruthy();
    const started = clockTime("2026-09-29T14:12:03Z", { seconds: false });
    expect(screen.getByText(new RegExp(`\\(started ${started}\\)`))).toBeTruthy();

    const sw = await screen.findByRole("switch", { name: "Enabled" });
    expect(sw).toBeDisabled();
    fireEvent.click(sw);
    expect(screen.queryByRole("dialog")).toBeNull();

    // The mockup's README: "applying ... settings locked" — every control,
    // not just the switch.
    expect(screen.getByRole("button", { name: "Discard" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Physical output" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Default app" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Default user" })).toBeDisabled();
    expect(screen.getByRole("switch", { name: "Auto-start on display" })).toBeDisabled();
    await screen.findByText("Input devices");
    expect(screen.getByRole("tab", { name: "Specific devices" })).toBeDisabled();
  });

  it("applying with no started_at: omits the parenthetical", async () => {
    mockAccess({
      state: "applying", target: true, request_id: "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2",
      reason: null, started_at: null, finished_at: null, summary: "Replacing the node agent.",
    });
    renderPage();

    await screen.findByText("Turning on console mode.");
    expect(screen.queryByText(/\(started/)).toBeNull();
  });

  it("on: reads as on when enabled and the host has access, with no note", async () => {
    mockAccess(
      {
        state: "on", target: true, request_id: "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: null, started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:00Z",
        summary: "Console access is on.",
      },
      { enabled: true },
    );
    renderPage();

    await waitFor(() => expect(screen.getByText("On")).toBeTruthy());
    expect(screen.queryByText(/did not turn/)).toBeNull();
    expect(screen.queryByText("Console mode is off.")).toBeNull();
  });

  it("restored, target true: a failed turn-on shows the mapped reason and a Try again that PATCHes true", async () => {
    mockAccess(
      {
        state: "restored", target: true, request_id: "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: "unhealthy", started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:40Z",
        summary: "The node agent with console access did not become healthy.",
      },
      { enabled: false },
    );
    renderPage();

    await screen.findByText(/did not turn on/);
    expect(screen.getByText(/started but never became healthy/)).toBeTruthy();
    expect(screen.getByText("Details")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { enabled: true },
    ));
  });

  it("restored, target false: a failed turn-off keeps access on and Try again PATCHes false", async () => {
    mockAccess(
      {
        state: "restored", target: false, request_id: "9c1a2b3c-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: "recreate_failed", started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:40Z",
        summary: "The replacement agent could not be recreated.",
      },
      { enabled: true },
    );
    renderPage();

    await screen.findByText(/did not turn off/);
    expect(screen.getByText("On")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { enabled: false },
    ));
  });

  it("unsupported: disables the switch and shows access.summary", async () => {
    mockAccess({
      state: "unsupported", target: null, request_id: null, reason: null,
      started_at: null, finished_at: null,
      summary: "This host's engine is rootless; console access needs RH07-15.",
    });
    renderPage();

    await screen.findByText("This host's engine is rootless; console access needs RH07-15.");
    expect(await screen.findByRole("switch", { name: "Enabled" })).toBeDisabled();
  });

  it("no access reported: today's page, no chip and no note", async () => {
    vi.mocked(adminApi.getConsoleConfig).mockResolvedValue({
      config: BASE_CONFIG,
      capabilities: BASE_CAPS,
      default_apps: [],
      readiness: [],
    } as never);
    renderPage();

    await screen.findByRole("switch", { name: "Enabled" });
    expect(screen.queryByText("Off")).toBeNull();
    expect(screen.queryByText("On")).toBeNull();
    expect(screen.queryByText("Console mode is off.")).toBeNull();
    expect(screen.getByText("The desktop drives this host's own display, with its own resolution, sound and input.")).toBeTruthy();
    expect(screen.queryByText(/shows games on its own screen/)).toBeNull();
  });

  it("restored: display held by another process leads with the summary's named holder, not the mapped reason", async () => {
    mockAccess(
      {
        state: "restored", target: true, request_id: "3f0812c4-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: "unhealthy", started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:40Z",
        summary: "gdm, the login screen holds the display Turning console mode on did not complete " +
          "(unhealthy), so the recovery actor put the previous node agent back; console mode is off.",
      },
      { enabled: false },
    );
    renderPage();

    await screen.findByText(/did not turn on/);
    expect(screen.getByText(/gdm, the login screen holds the display/)).toBeTruthy();
    expect(screen.getByText(
      /Stop the login screen on this display, or choose another output, then try again\./,
    )).toBeTruthy();
    // The generic reason-mapped sentence no longer swallows the named cause.
    expect(screen.queryByText(/started but never became healthy/)).toBeNull();
    expect(screen.queryByTestId("console-access-prepare-snippet")).toBeNull();
    expect(screen.getByText("Details")).toBeTruthy();
  });

  it("restored: host not prepared shows the copyable device-rules snippet and Details", async () => {
    mockAccess(
      {
        state: "restored", target: true, request_id: "9c1a2b3c-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: "unhealthy", started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:40Z",
        summary: "host not prepared for console mode: no display device is visible to this agent; " +
          "install the console device rules (docs: Install, Device rules), then try again Turning console " +
          "mode on did not complete (unhealthy), so the recovery actor put the previous node agent back; " +
          "console mode is off.",
      },
      { enabled: false },
    );
    renderPage();

    await screen.findByText(/is not prepared for it/);
    expect(screen.getByText("Run on lab-host as root")).toBeTruthy();
    expect(screen.getByTestId("console-access-prepare-snippet")).toHaveTextContent(
      "sudo cp deploy/udev/71-quasar-console.rules /etc/udev/rules.d/",
    );
    expect(screen.getByText("Details")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(adminApi.updateConsoleConfig).toHaveBeenCalledWith(
      "token", "host-1", { enabled: true },
    ));
  });

  it("restored: host not prepared shows the same device-rules command whatever the engine", async () => {
    vi.mocked(adminApi.getHost).mockResolvedValue({
      host: {
        id: "host-1", node_name: "lab-host", status: "online", capacity: { active_sessions: 2 },
        engine: "podman", engine_version: "5.6.2", engine_mode: "rootless",
      },
    } as never);
    mockAccess(
      {
        state: "restored", target: true, request_id: "9c1a2b3c-6e1d-4f7a-9d55-8c1b0e2d44a2",
        reason: "unhealthy", started_at: "2026-09-29T14:12:03Z", finished_at: "2026-09-29T14:13:40Z",
        summary: "host not prepared for console mode: no display device is visible to this agent; " +
          "install the console device rules (docs: Install, Device rules), then try again.",
      },
      { enabled: false },
    );
    renderPage();

    await screen.findByTestId("console-access-prepare-snippet");
    expect(screen.getByTestId("console-access-prepare-snippet")).toHaveTextContent(
      "sudo systemctl mask getty@tty8.service autovt@tty8.service",
    );
  });

  it("409 surfaced: a conflicting PATCH toasts the server's message", async () => {
    mockAccess({
      state: "off", target: null, request_id: null, reason: null,
      started_at: null, finished_at: null, summary: "Console mode is off.",
    });
    vi.mocked(adminApi.updateConsoleConfig).mockRejectedValue(
      new ApiError(409, "conflict", "a replacement is already applying"),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("switch", { name: "Enabled" }));
    fireEvent.click(await screen.findByRole("button", { name: "Turn on console mode" }));

    await waitFor(() => expect(addToastMock).toHaveBeenCalledWith(
      expect.objectContaining({ variant: "danger", title: "a replacement is already applying" }),
    ));
  });
});
