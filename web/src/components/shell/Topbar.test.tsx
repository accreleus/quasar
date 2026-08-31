/**
 * The console topbar is composition. The trigger's label is handed down (the
 * shell derives it from the lists it actually supplies — see
 * paletteSearch.test.ts `paletteScope`), so what is pinned here is only that
 * the topbar renders what it was given and nothing of its own.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { Topbar } from "./Topbar";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ThemeProvider } from "../../settings/ThemeContext";

function auth(role: "user" | "admin"): AuthContextValue {
  return {
    status: "authenticated",
    user: { id: "u1", email: "a@b.io", username: "salty2011", role } as AuthContextValue["user"],
    token: "t",
    isAdmin: role === "admin",
    login: vi.fn(),
    claim: vi.fn(),
    logout: vi.fn(),
  };
}

function renderTopbar(role: "user" | "admin", props: Partial<React.ComponentProps<typeof Topbar>> = {}) {
  return render(
    <MemoryRouter initialEntries={["/admin"]}>
      <AuthContext.Provider value={auth(role)}>
        <ThemeProvider>
          <Topbar onOpenPalette={vi.fn()} searchLabel="Search hosts, sessions, apps, users…" {...props} />
        </ThemeProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

describe("Topbar", () => {
  it("renders the promise the shell handed it, not one of its own", () => {
    renderTopbar("admin");
    expect(screen.getByRole("button", { name: /Search hosts, sessions, apps, users/ })).toBeInTheDocument();

    renderTopbar("user", { searchLabel: "Search games…" });
    expect(screen.getByRole("button", { name: /Search games/ })).toBeInTheDocument();
  });

  it("carries the brand and the ⌘K hint", () => {
    const { container } = renderTopbar("admin");
    expect(screen.getByRole("link", { name: "Quasar home" })).toHaveAttribute("href", "/app");
    expect(container.querySelector(".wordmark")).toHaveTextContent("Quasar");
    expect(container.querySelector(".cmdk kbd")).toHaveTextContent("⌘K");
  });

  it("offers the rail toggle only when the shell has a rail to slide over", () => {
    const { container } = renderTopbar("admin");
    expect(container.querySelector(".rail-btn")).toBeNull();
    renderTopbar("admin", { onToggleRail: vi.fn(), railOpen: false });
    expect(screen.getByRole("button", { name: "Sections" })).toHaveAttribute("aria-expanded", "false");
  });
});
