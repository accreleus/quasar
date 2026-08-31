/**
 * What the admin shell hands the command palette.
 *
 * The palette advertises hosts, sessions, apps and users; hosts and sessions
 * ride on the shared fleet poll, and the other two exist only because this
 * layout reads them. A layout that stopped supplying either would leave the
 * palette promising a search it answers with "No matches", which is the defect
 * this pins.
 */

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

vi.mock("../../api/admin");
vi.mock("../../api/setup");

const fleet = {
  hosts: [{ id: "h1", node_name: "quasar-node-1", status: "online" }],
  sessions: [{ id: "s1", app_name: "Blender", username: "devon", state: "running" }],
  loading: false,
  lastFetchedAt: null,
  errors: { hosts: null, sessions: null },
  reload: vi.fn(),
};
vi.mock("../../lib/fleet/FleetContext", () => ({
  FleetProvider: ({ children }: { children: ReactNode }) => children,
  useFleetContext: () => fleet,
  useFleetBadges: () => ({ live: 1, fault: 0 }),
}));

import * as adminApi from "../../api/admin";
import * as setupApi from "../../api/setup";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { SetupStatusProvider } from "../../setup/useSetupStatus";
import { ThemeProvider } from "../../settings/ThemeContext";
import { AdminLayout } from "./AdminLayout";

const mocked = vi.mocked(adminApi);

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "root", role: "admin" } as AuthContextValue["user"],
  token: "tok",
  isAdmin: true,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(setupApi.getSetupStatus).mockResolvedValue({
    admin_exists: true,
    setup_completed: true,
  } as never);
  mocked.listAdminApps.mockResolvedValue({
    items: [{ id: "a1", name: "Blender", kind: "desktop" }],
    next_cursor: null,
  } as never);
  mocked.listUsers.mockResolvedValue({
    items: [{ id: "u9", username: "devon", email: "devon@x.io" }],
    next_cursor: null,
  } as never);
  mocked.listSecrets.mockResolvedValue({ items: [] } as never);
});

function renderAdmin() {
  return render(
    <MemoryRouter initialEntries={["/admin"]}>
      <ThemeProvider>
        <AuthContext.Provider value={auth}>
          <SetupStatusProvider>
            <AdminLayout />
          </SetupStatusProvider>
        </AuthContext.Provider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("AdminLayout", () => {
  it("supplies all four palette sources, so the trigger's promise is honest", async () => {
    renderAdmin();
    expect(
      await screen.findByRole("button", { name: /Search hosts, sessions, apps, users/ }),
    ).toBeInTheDocument();
    await waitFor(() => expect(mocked.listAdminApps).toHaveBeenCalled());
    await waitFor(() => expect(mocked.listUsers).toHaveBeenCalled());

    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    const box = await screen.findByRole("combobox");
    fireEvent.change(box, { target: { value: "devon" } });

    // The user hit and the session hit both match "devon"; the app hit does not.
    expect(await screen.findByRole("option", { name: /devon@x.io/ })).toBeInTheDocument();
  });

  it("finds an app by name and opens its editor", async () => {
    renderAdmin();
    await waitFor(() => expect(mocked.listAdminApps).toHaveBeenCalled());

    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(await screen.findByRole("combobox"), { target: { value: "Blend" } });

    const apps = await screen.findByRole("group", { name: "Apps" });
    expect(apps).toHaveTextContent("Blender");
  });
});
