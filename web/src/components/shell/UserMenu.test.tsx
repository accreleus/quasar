/**
 * The user popover's open/closed switch is CSS, and CSS is where it can be
 * lost: a sheet trim once left the menu permanently open on every
 * authenticated route — displacing the trigger and putting every item, Sign
 * out included, in the Tab order.
 *
 * So these load the real shell.css into the document and assert the computed
 * visibility, rather than asserting the class name and trusting the sheet.
 * jsdom applies plain class selectors, which is all this rule needs.
 */
import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { UserMenu, type UserMenuMode } from "./UserMenu";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ThemeProvider } from "../../settings/ThemeContext";

const logout = vi.fn();

function auth(role: "user" | "admin"): AuthContextValue {
  return {
    status: "authenticated",
    user: { id: "u1", email: "test@test.com", username: "salty2011", role } as AuthContextValue["user"],
    token: "t",
    isAdmin: role === "admin",
    login: vi.fn(),
    claim: vi.fn(),
    logout,
  };
}

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

function renderMenu(mode: UserMenuMode, role: "user" | "admin" = "user") {
  return render(
    <MemoryRouter initialEntries={["/app"]}>
      <AuthContext.Provider value={auth(role)}>
        <ThemeProvider>
          <UserMenu mode={mode} />
          <Routes>
            <Route path="*" element={<Where />} />
          </Routes>
        </ThemeProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const trigger = () => screen.getByRole("button", { name: /salty2011/ });
const pop = () => screen.getByRole("menu", { hidden: true });

beforeAll(() => {
  const style = document.createElement("style");
  style.textContent = readFileSync(resolve(__dirname, "../../styles/shell.css"), "utf8");
  document.head.appendChild(style);
});

beforeEach(() => {
  localStorage.clear();
  document.body.removeAttribute("data-density");
  document.documentElement.removeAttribute("data-theme");
  logout.mockReset();
});

describe("UserMenu open/closed", () => {
  it("keeps the closed popover invisible and out of the tab order", () => {
    renderMenu("home");
    expect(pop()).not.toHaveClass("open");
    expect(getComputedStyle(pop()).visibility).toBe("hidden");
    expect(getComputedStyle(pop()).pointerEvents).toBe("none");
    // The trigger sits above it, not below it: an un-hidden popover pushes the
    // topbar apart, which is how the regression showed up on screen.
    expect(trigger()).toHaveAttribute("aria-expanded", "false");
    expect(trigger()).toHaveAttribute("aria-haspopup", "true");
  });

  it("reveals it when the trigger is clicked", () => {
    renderMenu("home");
    fireEvent.click(trigger());
    expect(pop()).toHaveClass("open");
    expect(getComputedStyle(pop()).visibility).toBe("visible");
    expect(getComputedStyle(pop()).pointerEvents).toBe("auto");
    expect(trigger()).toHaveAttribute("aria-expanded", "true");
  });

  it("closes on Escape and on an outside click", () => {
    renderMenu("home");
    fireEvent.click(trigger());
    fireEvent.keyDown(document, { key: "Escape" });
    expect(pop()).not.toHaveClass("open");

    fireEvent.click(trigger());
    fireEvent.pointerDown(document.body);
    expect(pop()).not.toHaveClass("open");
  });
});

describe("UserMenu items", () => {
  it("flips the theme label and value with the theme", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    // Default is dark, so the item offers the other one.
    const light = screen.getByRole("menuitem", { name: /Light mode/ });
    expect(light.querySelector(".up-val")).toHaveTextContent("☀");
    fireEvent.click(light);
    expect(screen.getByRole("menuitem", { name: /Dark mode/ }).querySelector(".up-val")).toHaveTextContent("☾");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("flips the density label with the density", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole("menuitem", { name: "Dense mode" }));
    expect(document.body.getAttribute("data-density")).toBe("dense");
    expect(screen.getByRole("menuitem", { name: "Comfortable mode" })).toBeInTheDocument();
  });

  it("offers Game library in console mode only", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    expect(screen.getByRole("menuitem", { name: "Game library" })).toHaveAttribute("href", "/app");
  });

  it("does not offer Game library on the home shell — you are already there", () => {
    renderMenu("home");
    fireEvent.click(trigger());
    expect(screen.queryByRole("menuitem", { name: "Game library" })).toBeNull();
  });

  it("offers Admin console with the Admin chip, in home mode, to admins only", () => {
    renderMenu("home", "admin");
    fireEvent.click(trigger());
    const item = screen.getByRole("menuitem", { name: /Admin console/ });
    expect(item).toHaveAttribute("href", "/admin");
    expect(item.querySelector(".chip.chip-accent")).toHaveTextContent("Admin");
  });

  it("hides Admin console from a non-admin, and from the console shell", () => {
    renderMenu("home", "user");
    fireEvent.click(trigger());
    expect(screen.queryByRole("menuitem", { name: /Admin console/ })).toBeNull();

    renderMenu("console", "admin");
    fireEvent.click(screen.getAllByRole("button", { name: /salty2011/ })[1]);
    expect(screen.queryByRole("menuitem", { name: /Admin console/ })).toBeNull();
  });

  it("routes Account at the v3 profile path", () => {
    renderMenu("home");
    fireEvent.click(trigger());
    expect(screen.getByRole("menuitem", { name: "Account" })).toHaveAttribute(
      "href",
      "/app/account/profile",
    );
  });

  it("signs out and lands on the login route", async () => {
    renderMenu("home");
    fireEvent.click(trigger());
    fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
    expect(logout).toHaveBeenCalledTimes(1);
    expect(await screen.findByTestId("where")).toHaveTextContent("/login");
  });
});

// `role="menu"` is a promise about the keyboard. These are that promise: the
// menu takes focus when it opens, the arrows rove it, and the two ways out
// that are not a click both put focus back on the trigger.
describe("UserMenu keyboard", () => {
  const menuItems = () => screen.getAllByRole("menuitem");

  it("moves focus to the first item when it opens", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    expect(menuItems()[0]).toHaveFocus();
  });

  it("roves with the arrow keys, wrapping at both ends", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    const items = menuItems();

    fireEvent.keyDown(pop(), { key: "ArrowDown" });
    expect(items[1]).toHaveFocus();
    fireEvent.keyDown(pop(), { key: "ArrowUp" });
    expect(items[0]).toHaveFocus();
    // Up from the first wraps to the last rather than falling out of the menu.
    fireEvent.keyDown(pop(), { key: "ArrowUp" });
    expect(items[items.length - 1]).toHaveFocus();
    fireEvent.keyDown(pop(), { key: "ArrowDown" });
    expect(items[0]).toHaveFocus();
  });

  it("jumps to the ends with Home and End", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    const items = menuItems();

    fireEvent.keyDown(pop(), { key: "End" });
    expect(items[items.length - 1]).toHaveFocus();
    fireEvent.keyDown(pop(), { key: "Home" });
    expect(items[0]).toHaveFocus();
  });

  it("Escape closes and returns focus to the trigger", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    fireEvent.keyDown(document, { key: "Escape" });
    expect(pop()).not.toHaveClass("open");
    expect(trigger()).toHaveFocus();
  });

  it("Tab closes and returns focus to the trigger", () => {
    renderMenu("console");
    fireEvent.click(trigger());
    fireEvent.keyDown(pop(), { key: "Tab" });
    expect(pop()).not.toHaveClass("open");
    expect(trigger()).toHaveFocus();
  });
});
