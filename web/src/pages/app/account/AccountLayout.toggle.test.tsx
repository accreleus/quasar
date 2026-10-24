// Flipping Classic layout swaps the account page's tree under the switch. The
// user must stay on the Appearance path with the switch on screen and focused,
// from either layout; the classic outlet's redirects to Profile must not fire.

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { AccountLayout } from "./AccountLayout";
import { AccountAppearance } from "./AccountAppearance";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { ToastProvider } from "../../../components/Toast";
import { OverlayPreferencesProvider } from "../../../settings/OverlayPreferencesContext";
import { ThemeProvider } from "../../../settings/ThemeContext";

const patchUIPreferences = vi.hoisted(() => vi.fn());
vi.mock("../../../api/me", () => ({
  getUIPreferences: vi.fn().mockRejectedValue(new Error("offline")),
  patchUIPreferences,
}));
vi.mock("../libraryCatalog", () => ({
  LibraryCatalogProvider: ({ children }: { children: React.ReactNode }) => children,
  useLibraryCatalog: () => ({ apps: [] }),
  useOptionalLibraryCatalog: () => null,
}));
vi.mock("./onepage/OneSettingsPage", () => ({
  OneSettingsPage: () => (
    <>
      <p>one page</p>
      <AccountAppearance />
    </>
  ),
}));

const auth = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "sam", role: "user" },
  token: "t",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
} as unknown as AuthContextValue;

function Where() {
  return <p data-testid="where">{useLocation().pathname}</p>;
}

function renderAt(path: string, classic: boolean) {
  localStorage.setItem("quasar.ui.classic.u1", classic ? "1" : "0");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={auth}>
        <ToastProvider>
          <OverlayPreferencesProvider>
            <ThemeProvider>
              <Routes>
                <Route path="/app/account" element={<AccountLayout />}>
                  <Route index element={<Navigate to="/app/account/profile" replace />} />
                  <Route path="profile" element={<p>profile page</p>} />
                  <Route path="appearance" element={<AccountAppearance />} />
                  <Route path="security" element={<Navigate to="/app/account/profile" replace />} />
                </Route>
              </Routes>
              <Where />
            </ThemeProvider>
          </OverlayPreferencesProvider>
        </ToastProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const sw = () => screen.getByLabelText("Classic layout") as HTMLInputElement;
const where = () => screen.getByTestId("where").textContent;

beforeEach(() => {
  localStorage.clear();
  patchUIPreferences.mockReset().mockResolvedValue({});
});

describe("flipping Classic layout from the Appearance switch", () => {
  it.each(["/app/account", "/app/account/security"])("new design to Classic from %s: stays on Appearance, switch focused", async (start) => {
    renderAt(start, false);
    await waitFor(() => expect(sw()).not.toBeDisabled());
    fireEvent.click(sw());

    await waitFor(() => expect(screen.queryByText("one page")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(sw()));
    expect(sw().checked).toBe(true);
    expect(where()).toBe("/app/account/appearance");
    expect(screen.queryByText("profile page")).toBeNull();
    expect(patchUIPreferences).toHaveBeenCalledWith("t", { classic_layout: true });
  });

  it("Classic to the new design: lands on the one page, switch focused", async () => {
    renderAt("/app/account/appearance", true);
    await waitFor(() => expect(sw()).not.toBeDisabled());
    fireEvent.click(sw());

    expect(await screen.findByText("one page")).toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(sw()));
    expect(sw().checked).toBe(false);
    expect(where()).toBe("/app/account/appearance");
  });

  it("a failed save puts the page back with the switch focused again", async () => {
    patchUIPreferences.mockRejectedValue(new Error("down"));
    renderAt("/app/account", false);
    await waitFor(() => expect(sw()).not.toBeDisabled());
    fireEvent.click(sw());

    expect(await screen.findByText("Could not save layout")).toBeInTheDocument();
    await waitFor(() => expect(document.activeElement).toBe(sw()));
    expect(screen.getByText("one page")).toBeInTheDocument();
    expect(sw().checked).toBe(false);
    expect(where()).toBe("/app/account/appearance");
  });
});
