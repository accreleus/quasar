// The new design's one account page: the stream default, the devices section
// and the section links. The classic sections it embeds are stubbed.

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { OneSettingsPage } from "./OneSettingsPage";
import { AuthContext, type AuthContextValue } from "../../../../auth/context";
import { ToastProvider } from "../../../../components/Toast";

const api = vi.hoisted(() => ({
  listDevices: vi.fn(),
  updateDevice: vi.fn(),
  revokeDevice: vi.fn(),
  getProfiles: vi.fn(),
  getProfilePreferences: vi.fn(),
  updateProfilePreferences: vi.fn(),
  getMyStorage: vi.fn(),
}));
vi.mock("../../../../api/auth", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../../api/auth")>()),
  listDevices: api.listDevices,
  updateDevice: api.updateDevice,
  revokeDevice: api.revokeDevice,
}));
vi.mock("../../../../api/library", () => ({
  getProfiles: api.getProfiles,
  getProfilePreferences: api.getProfilePreferences,
  updateProfilePreferences: api.updateProfilePreferences,
}));
vi.mock("../../../../api/storage", () => ({ getMyStorage: api.getMyStorage }));
vi.mock("./RegionSection", () => ({ RegionSection: () => <p>region stub</p> }));
vi.mock("../AccountOverlay", () => ({ AccountOverlay: () => <p>overlay stub</p> }));
vi.mock("../AccountAppearance", () => ({ AccountAppearance: () => <p>appearance stub</p> }));
vi.mock("../AccountSessions", () => ({ AccountSessions: () => <p>sessions stub</p> }));
vi.mock("../AccountStorage", () => ({ AccountStorage: () => <p>storage stub</p> }));
vi.mock("../PasswordCard", () => ({ PasswordCard: () => <p>password stub</p> }));
vi.mock("./InputSection", () => ({ InputSection: () => <p>input stub</p> }));

const logout = vi.fn();
const auth = {
  status: "authenticated",
  user: { id: "u1", email: "sam@example.io", username: "sam", role: "user" },
  token: "t0k3n",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout,
} as unknown as AuthContextValue;

const DAY = 864e5;
const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
const device = (id: string, name: string | null, lastSeen: string, over: Record<string, unknown> = {}) => ({
  id,
  device_key: `${id}-key-0000`,
  name,
  trusted: false,
  first_seen_at: "2026-01-01T00:00:00Z",
  last_seen_at: lastSeen,
  current: false,
  active_session_id: null,
  capabilities: {},
  ...over,
});

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function renderPage(path = "/app/account") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={auth}>
        <ToastProvider>
          <Routes>
            <Route path="/login" element={<p>login page</p>} />
            <Route path="*" element={<OneSettingsPage />} />
          </Routes>
          <Where />
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const select = () => screen.getByLabelText("Stream default") as HTMLSelectElement;
const status = () => screen.getByRole("status");

beforeEach(() => {
  vi.clearAllMocks();
  api.getProfiles.mockResolvedValue({
    recommended_id: "p-fast",
    profiles: [
      { id: "p-fast", display_name: "Fast" },
      { id: "p-sharp", display_name: "Sharp" },
    ],
  });
  api.getProfilePreferences.mockResolvedValue({
    default_profile_id: null,
    global_default_profile_id: "p-fast",
    user_overrides_allowed: true,
  });
  api.updateProfilePreferences.mockImplementation(async (_t: string, id: string | null) => ({ default_profile_id: id }));
  api.getMyStorage.mockResolvedValue({ items: [] });
  api.listDevices.mockResolvedValue({ devices: [] });
  api.updateDevice.mockImplementation(async (_t: string, id: string, patch: { name?: string }) => ({
    device: device(id, patch.name ?? null, ago(DAY), {}),
  }));
  api.revokeDevice.mockResolvedValue(undefined);
});

describe("stream default", () => {
  it("shows the pick at once, holds the select shut while saving, and says what was saved", async () => {
    let settle!: (v: unknown) => void;
    api.updateProfilePreferences.mockReturnValue(new Promise((r) => { settle = r; }));
    renderPage();
    await waitFor(() => expect(select()).not.toBeDisabled());

    fireEvent.change(select(), { target: { value: "p-sharp" } });
    expect(select().value).toBe("p-sharp");
    expect(select()).toBeDisabled();
    fireEvent.change(select(), { target: { value: "p-fast" } });
    expect(api.updateProfilePreferences).toHaveBeenCalledTimes(1);

    await act(async () => settle({ default_profile_id: "p-sharp" }));
    expect(select().value).toBe("p-sharp");
    expect(select()).not.toBeDisabled();
    expect(status()).toHaveTextContent("Saved. Your next launch asks for Sharp.");
  });

  it("goes back to the saved value and says so when the save fails", async () => {
    api.updateProfilePreferences.mockRejectedValue(new Error("down"));
    renderPage();
    await waitFor(() => expect(select()).not.toBeDisabled());

    fireEvent.change(select(), { target: { value: "p-sharp" } });
    await waitFor(() => expect(status()).toHaveTextContent("Could not save the stream default."));
    expect(select().value).toBe("");
    expect(select()).not.toBeDisabled();
  });

  it("names what the empty choice launches: the admin's default when one is set (#814)", async () => {
    // The device recommends Sharp; the admin's global default is Fast, which is what an empty choice launches.
    api.getProfiles.mockResolvedValue({
      recommended_id: "p-sharp",
      profiles: [
        { id: "p-fast", display_name: "Fast" },
        { id: "p-sharp", display_name: "Sharp" },
      ],
    });
    api.getProfilePreferences.mockResolvedValue({
      default_profile_id: "p-sharp",
      global_default_profile_id: "p-fast",
      user_overrides_allowed: true,
    });
    renderPage();
    await waitFor(() => expect(select()).not.toBeDisabled());
    const empty = within(select()).getByRole("option", { name: /^Admin default/ });
    expect(empty).toHaveTextContent("Admin default (Fast)");
    expect(within(select()).queryByRole("option", { name: /Recommended/ })).toBeNull();

    api.updateProfilePreferences.mockResolvedValue({ default_profile_id: null });
    fireEvent.change(select(), { target: { value: "" } });
    await waitFor(() => expect(status()).toHaveTextContent("Saved. Launches use the admin's default (Fast) again."));
  });

  it("calls the empty choice the recommendation when no global default is set (#814)", async () => {
    api.getProfiles.mockResolvedValue({
      recommended_id: "p-sharp",
      profiles: [
        { id: "p-fast", display_name: "Fast" },
        { id: "p-sharp", display_name: "Sharp" },
      ],
    });
    api.getProfilePreferences.mockResolvedValue({
      default_profile_id: "p-fast",
      global_default_profile_id: null,
      user_overrides_allowed: true,
    });
    renderPage();
    await waitFor(() => expect(select()).not.toBeDisabled());
    expect(within(select()).getByRole("option", { name: "Recommended (Sharp)" })).toBeInTheDocument();

    api.updateProfilePreferences.mockResolvedValue({ default_profile_id: null });
    fireEvent.change(select(), { target: { value: "" } });
    await waitFor(() => expect(status()).toHaveTextContent("Saved. Launches use the recommendation again."));
  });

  it("says the admin's default, not the stored one, while overrides are off (#721)", async () => {
    api.getProfilePreferences.mockResolvedValue({
      default_profile_id: "p-sharp",
      global_default_profile_id: "p-fast",
      user_overrides_allowed: false,
    });
    renderPage();
    await waitFor(() => expect(status()).toHaveTextContent("Now: Fast."));
  });

  it("says the stored default while overrides are on", async () => {
    api.getProfilePreferences.mockResolvedValue({
      default_profile_id: "p-sharp",
      global_default_profile_id: "p-fast",
      user_overrides_allowed: true,
    });
    renderPage();
    await waitFor(() => expect(status()).toHaveTextContent("Now: Sharp."));
  });
});

describe("devices", () => {
  it("groups by use: ninety days or more unused is its own group, and the rest are in use", async () => {
    api.listDevices.mockResolvedValue({
      devices: [
        device("d1", "Desk", ago(60_000), { current: true }),
        device("d2", "Laptop", ago(89 * DAY + 23 * 36e5)),
        device("d3", "Old phone", ago(90 * DAY + 36e5)),
      ],
    });
    renderPage();
    const inUse = await screen.findByRole("group", { name: "In use" });
    expect(within(inUse).getByText("Desk")).toBeInTheDocument();
    expect(within(inUse).getByText("Laptop")).toBeInTheDocument();
    const stale = screen.getByRole("group", { name: "Not used in 90 days" });
    expect(within(stale).getByText("Old phone")).toBeInTheDocument();
    expect(within(stale).queryByText("Laptop")).toBeNull();
    expect(within(stale).getByText("Last used 90 days ago")).toBeInTheDocument();
  });

  it("names the device in the revoke dialog and revokes only on confirmation", async () => {
    api.listDevices.mockResolvedValue({ devices: [device("d2", "Old phone", ago(100 * DAY))] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke Old phone" }));

    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText("Revoke Old phone?")).toBeInTheDocument();
    expect(api.revokeDevice).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke Old phone" }));

    await waitFor(() => expect(api.revokeDevice).toHaveBeenCalledWith("t0k3n", "d2"));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(logout).not.toHaveBeenCalled();
  });

  it("revoking this device signs out and goes to the login page", async () => {
    api.listDevices.mockResolvedValue({ devices: [device("d1", "Desk", ago(1000), { current: true })] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Revoke Desk" }));
    const dialog = screen.getByRole("dialog");
    expect(within(dialog).getByText(/this device/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Revoke Desk" }));

    expect(await screen.findByText("login page")).toBeInTheDocument();
    expect(api.revokeDevice).toHaveBeenCalledWith("t0k3n", "d1");
    expect(logout).toHaveBeenCalledTimes(1);
  });

  it("renames on Enter and leaves the name alone on Escape", async () => {
    api.listDevices.mockResolvedValue({ devices: [device("d2", "Laptop", ago(DAY))] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Rename Laptop" }));
    let box = screen.getByLabelText("New name for Laptop");
    fireEvent.keyDown(box, { key: "Escape" });
    expect(screen.queryByLabelText("New name for Laptop")).toBeNull();
    expect(api.updateDevice).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Rename Laptop" }));
    box = screen.getByLabelText("New name for Laptop");
    fireEvent.change(box, { target: { value: "Den laptop" } });
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(api.updateDevice).toHaveBeenCalledWith("t0k3n", "d2", { name: "Den laptop" }));
    expect(await screen.findByText("Den laptop")).toBeInTheDocument();
  });

  it("keeps a long unbroken name inside its row's text block", async () => {
    const long = "x".repeat(120);
    api.listDevices.mockResolvedValue({ devices: [device("d2", long, ago(DAY))] });
    renderPage();
    const name = await screen.findByText(long);
    expect(name).toHaveClass("os-dev-name");
    expect(name.parentElement).toHaveClass("os-dev-text");
  });
});

describe("section links", () => {
  it("a path lights its section, and clicking the current one scrolls to it again", async () => {
    const scroll = vi.spyOn(Element.prototype, "scrollIntoView");
    renderPage("/app/account/devices");
    const nav = screen.getByRole("navigation", { name: "Settings sections" });
    expect(within(nav).getByRole("link", { name: "Devices" })).toHaveAttribute("aria-current", "location");
    await waitFor(() => expect(scroll.mock.contexts.some((el) => (el as HTMLElement).id === "set-devices")).toBe(true));

    scroll.mockClear();
    fireEvent.click(within(nav).getByRole("link", { name: "Devices" }));
    await waitFor(() => expect(scroll.mock.contexts.some((el) => (el as HTMLElement).id === "set-devices")).toBe(true));
    expect(screen.getByTestId("where")).toHaveTextContent("/app/account/devices");
    scroll.mockRestore();
  });

  it("the bare path and /security render the page without a redirect, /security lighting Password", () => {
    const { unmount } = renderPage("/app/account");
    expect(screen.getByRole("heading", { name: "Account", level: 1 })).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/app\/account$/);
    unmount();
    renderPage("/app/account/security");
    expect(within(screen.getByRole("navigation", { name: "Settings sections" })).getByRole("link", { name: "Password" })).toHaveAttribute("aria-current", "location");
    expect(screen.getByTestId("where")).toHaveTextContent("/app/account/security");
  });
});
