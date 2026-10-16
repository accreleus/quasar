import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AccountLayout } from "./AccountLayout";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { OverlayPreferencesProvider } from "../../../settings/OverlayPreferencesContext";
import { ThemeProvider } from "../../../settings/ThemeContext";

vi.mock("../../../api/me", () => ({
  getUIPreferences: vi.fn().mockRejectedValue(new Error("offline")),
  patchUIPreferences: vi.fn().mockResolvedValue({}),
}));
vi.mock("../libraryCatalog", () => ({
  LibraryCatalogProvider: ({ children }: { children: React.ReactNode }) => children,
  useLibraryCatalog: () => ({ apps: [] }),
}));
vi.mock("./onepage/OneSettingsPage", () => ({ OneSettingsPage: () => <p>one page</p> }));

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "sam", role: "user" } as AuthContextValue["user"],
  token: "t",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function renderLayout(classic: boolean) {
  localStorage.setItem("quasar.ui.classic.u1", classic ? "1" : "0");
  return render(
    <MemoryRouter initialEntries={["/app/account/profile"]}>
      <AuthContext.Provider value={auth}>
        <OverlayPreferencesProvider>
          <ThemeProvider>
            <Routes>
              <Route path="/app/account" element={<AccountLayout />}>
                <Route path="profile" element={<p>tabbed page</p>} />
              </Route>
            </Routes>
          </ThemeProvider>
        </OverlayPreferencesProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

beforeEach(() => localStorage.clear());

describe("AccountLayout", () => {
  it("classic keeps the tabbed pages and the three-row rail", () => {
    renderLayout(true);
    expect(screen.getByText("tabbed page")).toBeInTheDocument();
    expect(screen.queryByText("one page")).toBeNull();
    expect(screen.getByRole("link", { name: "Preferences" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Appearance" })).toBeNull();
  });

  it("the new design renders the one page, with a rail row per section including Appearance", () => {
    renderLayout(false);
    expect(screen.getByText("one page")).toBeInTheDocument();
    expect(screen.queryByText("tabbed page")).toBeNull();
    expect(screen.getByRole("link", { name: "Appearance" })).toHaveAttribute("href", "/app/account/appearance");
  });
});
