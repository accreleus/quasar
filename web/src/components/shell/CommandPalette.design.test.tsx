/**
 * The palette under the real design switch (CommandPalette.test.tsx pins the
 * shipped palette with a mock). Admin must keep the shipped search whatever the
 * user chose; a player's chosen game opens its page.
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

function mount(path: string, isAdmin: boolean) {
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

describe("CommandPalette on /admin with the new design on", () => {
  it("is the shipped palette: actions lead, no ranked game rows, no typo hits", () => {
    mount("/admin/fleet", true);
    type("lib");
    expect(options()[0]).toMatch(/^Go to Library/);
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)[0]).toBe("Actions");
    type("libary");
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)).toEqual(["No matches"]);
  });
});

describe("CommandPalette in the player area with the new design on", () => {
  it("a game row opens its page, saying where it came from so Back returns there", () => {
    mount("/app/library", false);
    type("ruina");
    fireEvent.keyDown(screen.getByRole("combobox"), { key: "Enter" });
    expect(screen.getByTestId("where")).toHaveTextContent('/app/game/a1 {"from":"palette"}');
  });
});
