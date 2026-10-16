/**
 * The palette under the real design switch (CommandPalette.test.tsx pins the
 * shipped palette with a mock). Admin keeps the shipped game search whatever the
 * user chose, and gains the console's own pages and settings unless they chose
 * Classic; a player's chosen game opens its page.
 */
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { CommandPalette } from "./CommandPalette";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { OverlayPreferencesProvider } from "../../settings/OverlayPreferencesContext";
import { ThemeProvider } from "../../settings/ThemeContext";

vi.mock("../../api/me", () => ({
  getUIPreferences: () => new Promise(() => {}),
  patchUIPreferences: vi.fn(),
}));

const apps = [{ id: "a1", name: "Library Of Ruina", kind: "game" }];

const auth = (isAdmin: boolean): AuthContextValue => ({
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "x", role: isAdmin ? "admin" : "user" } as AuthContextValue["user"],
  token: "t",
  isAdmin,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
});

function Open() {
  const [open, setOpen] = useState(true);
  const { pathname, state } = useLocation();
  return (
    <>
      <CommandPalette open={open} onOpenChange={setOpen} apps={apps} />
      <span data-testid="where">{`${pathname} ${JSON.stringify(state)}`}</span>
    </>
  );
}

function mount(path: string, isAdmin: boolean, classic = false) {
  localStorage.setItem("quasar.ui.classic.u1", classic ? "1" : "0");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={auth(isAdmin)}>
        <ThemeProvider>
          <OverlayPreferencesProvider>
            <Open />
          </OverlayPreferencesProvider>
        </ThemeProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const type = (value: string) => fireEvent.change(screen.getByRole("combobox"), { target: { value } });
const options = () => screen.getAllByRole("option").map((o) => o.textContent ?? "");

describe("CommandPalette on /admin", () => {
  it("new design: the shipped game search, with the console's pages and settings ahead of the jumps", () => {
    mount("/admin/fleet", true);
    expect(screen.getByRole("combobox")).toHaveAttribute("placeholder", "Search hosts, sessions, apps, users, pages or settings");
    type("lib");
    const secs = [...document.querySelectorAll(".pal-sec")].map((e) => e.textContent);
    expect(secs[0]).not.toBe("Actions");
    expect(options().some((o) => /^Library Of Ruina/.test(o))).toBe(true);
    type("libary");
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)).toEqual(["No matches"]);
  });

  it("Classic layout: exactly the shipped palette, actions first", () => {
    mount("/admin/fleet", true, true);
    type("lib");
    expect(options()[0]).toMatch(/^Go to Library/);
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)[0]).toBe("Actions");
    expect(screen.getByRole("combobox")).toHaveAttribute("placeholder", "Search apps or jump to a page");
    expect(screen.queryByText("Elsewhere in the console")).toBeNull();
  });
});

describe("CommandPalette in the player area with the new design on", () => {
  it("a game row opens its page, saying where it came from so Back returns there", () => {
    mount("/app/library", false);
    type("ruina");
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    expect(screen.getByTestId("where")).toHaveTextContent('/app/game/a1 {"from":"palette"}');
  });

  it("a settings row carries no game-page hand-off state, so Play never takes the focus the setting is owed", () => {
    mount("/app/library", false);
    type("classic layout");
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    expect(screen.getByTestId("where")).toHaveTextContent("/app/account/appearance null");
  });
});
