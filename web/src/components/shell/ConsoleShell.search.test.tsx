// The console's search trigger follows the stored layout choice, not the
// /admin exclusion: the console is not redesigned, but its search finds pages.

import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ConsoleShell } from "./ConsoleShell";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { OverlayPreferencesProvider } from "../../settings/OverlayPreferencesContext";
import { ThemeProvider } from "../../settings/ThemeContext";

vi.mock("../../api/me", () => ({
  getUIPreferences: vi.fn().mockRejectedValue(new Error("offline")),
  patchUIPreferences: vi.fn().mockResolvedValue({}),
}));

const auth = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "sam", role: "admin" },
  token: "t",
  isAdmin: true,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
} as unknown as AuthContextValue;

function renderConsole(classic: boolean) {
  localStorage.setItem("quasar.ui.classic.u1", classic ? "1" : "0");
  return render(
    <MemoryRouter initialEntries={["/admin/fleet/hosts"]}>
      <AuthContext.Provider value={auth}>
        <OverlayPreferencesProvider>
          <ThemeProvider>
            <ConsoleShell sections={[]} railLabel="Admin sections" palette={{ hosts: [], sessions: [], apps: [], users: [] }}>
              <p>page</p>
            </ConsoleShell>
          </ThemeProvider>
        </OverlayPreferencesProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

beforeEach(() => localStorage.clear());

describe("ConsoleShell search trigger", () => {
  it("new design: promises the console's pages and settings, with a short phone label", () => {
    renderConsole(false);
    expect(screen.getByText("Search hosts, sessions, apps, users, settings…")).toBeInTheDocument();
    expect(screen.getByText("Search the console…")).toBeInTheDocument();
  });

  it("Classic layout: the shipped label, no phone variant", () => {
    renderConsole(true);
    expect(screen.queryByText("Search hosts, sessions, apps, users, settings…")).toBeNull();
    expect(screen.queryByText("Search the console…")).toBeNull();
  });
});
