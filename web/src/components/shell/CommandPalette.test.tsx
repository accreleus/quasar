/**
 * The palette's search is tested pure in paletteSearch.test.ts. What is left
 * here is the dialog: the shortcuts, the selection cursor and what Enter does.
 *
 * The harness mirrors how a shell wires it — the shell owns `open` so the
 * topbar's `.cmdk` trigger and the ⌘K shortcut cannot disagree about it.
 */
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { CommandPalette } from "./CommandPalette";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { ThemeProvider } from "../../settings/ThemeContext";

const apps = [{ id: "a1", name: "Blender", kind: "desktop" }];
const hosts = [{ id: "h1", node_name: "quasar-node-1", status: "online" }];

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.io", username: "salty2011", role: "admin" } as AuthContextValue["user"],
  token: "t",
  isAdmin: true,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function Where() {
  return <span data-testid="where">{useLocation().pathname + useLocation().search}</span>;
}

/** The shell wiring, in miniature: a `.cmdk` trigger plus the palette. */
function Harness() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button className="cmdk" onClick={() => setOpen(true)}>
        Search hosts, sessions, apps, users…
      </button>
      <input aria-label="a page field" />
      <CommandPalette open={open} onOpenChange={setOpen} apps={apps} hosts={hosts} />
      <Routes>
        <Route path="*" element={<Where />} />
      </Routes>
    </>
  );
}

function renderPalette() {
  return render(
    <MemoryRouter initialEntries={["/admin"]}>
      <AuthContext.Provider value={auth}>
        <ThemeProvider>
          <Harness />
        </ThemeProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const dialog = () => screen.queryByRole("dialog");
const input = () => screen.getByRole("combobox");

describe("CommandPalette opening", () => {
  it("is closed until asked for", () => {
    renderPalette();
    expect(dialog()).toBeNull();
  });

  it("opens on Ctrl+K", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    expect(dialog()).toBeInTheDocument();
    expect(input()).toHaveFocus();
  });

  it("opens on the topbar trigger", () => {
    const { container } = renderPalette();
    fireEvent.click(container.querySelector(".cmdk") as HTMLButtonElement);
    expect(dialog()).toBeInTheDocument();
  });

  it("opens on a bare slash", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "/" });
    expect(dialog()).toBeInTheDocument();
  });

  it("leaves a slash typed into a field alone", () => {
    renderPalette();
    fireEvent.keyDown(screen.getByLabelText("a page field"), { key: "/" });
    expect(dialog()).toBeNull();
  });

  it("closes on Escape and on a click outside the panel", () => {
    const { container } = renderPalette();
    fireEvent.keyDown(document, { key: "k", metaKey: true });
    fireEvent.keyDown(document, { key: "Escape" });
    expect(dialog()).toBeNull();

    fireEvent.keyDown(document, { key: "k", metaKey: true });
    fireEvent.mouseDown(container.querySelector(".scrim") as HTMLElement);
    expect(dialog()).toBeNull();
  });
});

describe("CommandPalette results", () => {
  it("shows the six default actions and the keyboard hints", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    expect(screen.getAllByRole("option")).toHaveLength(6);
    expect(screen.getByText("↑↓ navigate")).toBeInTheDocument();
    expect(screen.getByText("↵ open")).toBeInTheDocument();
    expect(screen.getByText("esc close")).toBeInTheDocument();
  });

  it("filters into groups as you type", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(input(), { target: { value: "blender" } });
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)).toEqual(["Apps"]);
    expect(screen.getByRole("option", { name: /Blender/ })).toBeInTheDocument();

    fireEvent.change(input(), { target: { value: "zzz" } });
    expect([...document.querySelectorAll(".pal-sec")].map((e) => e.textContent)).toEqual(["No matches"]);
    expect(screen.queryAllByRole("option")).toHaveLength(0);
  });

  it("moves the selection with the arrow keys, clamped at both ends", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    const selected = () => screen.getAllByRole("option").findIndex((o) => o.getAttribute("aria-selected") === "true");

    expect(selected()).toBe(0);
    fireEvent.keyDown(input(), { key: "ArrowUp" });
    expect(selected()).toBe(0);
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    expect(selected()).toBe(2);
    for (let i = 0; i < 20; i += 1) fireEvent.keyDown(input(), { key: "ArrowDown" });
    expect(selected()).toBe(5);
  });

  it("hovering a row selects it", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.mouseEnter(screen.getAllByRole("option")[3]);
    expect(screen.getAllByRole("option")[3]).toHaveAttribute("aria-selected", "true");
  });

  it("Enter navigates to the selected row and closes", async () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    // Actions[1] is "Go to Sessions".
    fireEvent.keyDown(input(), { key: "ArrowDown" });
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(dialog()).toBeNull();
    expect(await screen.findByTestId("where")).toHaveTextContent("/admin/sessions");
  });

  it("clicking a host row opens it in Fleet", async () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(input(), { target: { value: "node-1" } });
    fireEvent.click(screen.getByRole("option", { name: /quasar-node-1/ }));
    expect(await screen.findByTestId("where")).toHaveTextContent("/admin/fleet/hosts/h1");
  });

  it("runs an action instead of navigating when the row has no route", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(input(), { target: { value: "toggle appearance" } });
    fireEvent.click(screen.getByRole("option", { name: /Toggle appearance/ }));
    expect(dialog()).toBeNull();
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("forgets the previous query on reopen", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(input(), { target: { value: "blender" } });
    fireEvent.keyDown(document, { key: "Escape" });
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    expect(input()).toHaveValue("");
  });
});

// It declares itself `aria-modal`, so it owes the whole modal focus contract:
// take focus, keep it, give it back. Losing any one of the three strands a
// keyboard user at the top of the document with the page still frozen.
describe("CommandPalette focus contract", () => {
  it("returns focus to whatever opened it, on Escape", () => {
    const { container } = renderPalette();
    const cmdk = container.querySelector(".cmdk") as HTMLButtonElement;
    cmdk.focus();
    fireEvent.click(cmdk);
    expect(input()).toHaveFocus();

    fireEvent.keyDown(document, { key: "Escape" });
    expect(dialog()).toBeNull();
    expect(cmdk).toHaveFocus();
  });

  it("returns focus after running a row too, not only after a cancel", () => {
    const { container } = renderPalette();
    const cmdk = container.querySelector(".cmdk") as HTMLButtonElement;
    cmdk.focus();
    fireEvent.click(cmdk);
    fireEvent.keyDown(input(), { key: "Enter" });
    expect(cmdk).toHaveFocus();
  });

  it("cycles Tab and Shift+Tab inside the panel", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    const options = () => screen.getAllByRole("option");

    // Forward from the field lands on the first row, never on the page behind.
    fireEvent.keyDown(input(), { key: "Tab" });
    expect(options()[0]).toHaveFocus();
    expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);

    // Backward from the field wraps to the last row rather than escaping.
    input().focus();
    fireEvent.keyDown(input(), { key: "Tab", shiftKey: true });
    expect(options()[options().length - 1]).toHaveFocus();
    expect(screen.getByRole("dialog")).toContainElement(document.activeElement as HTMLElement);
  });

  it("freezes the page while open and unfreezes on close", () => {
    renderPalette();
    expect(document.body.style.overflow).toBe("");
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    expect(document.body.style.overflow).toBe("hidden");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(document.body.style.overflow).toBe("");
  });
});

describe("CommandPalette result semantics", () => {
  // A listbox may only contain options and groups; a bare heading div between
  // them is what makes a screen reader announce the wrong set size.
  it("wraps each section in a labelled group, with the heading as its visual label", () => {
    renderPalette();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    fireEvent.change(input(), { target: { value: "blender" } });

    const group = screen.getByRole("group", { name: "Apps" });
    expect(screen.getByRole("listbox")).toContainElement(group);
    expect(group.querySelector(".pal-sec")).toHaveTextContent("Apps");
    expect(group.querySelector(".pal-sec")).toHaveAttribute("aria-hidden", "true");
    // Every option lives inside a group, so the listbox has no loose children.
    for (const option of screen.getAllByRole("option")) {
      expect(option.parentElement).toHaveAttribute("role", "group");
    }
  });
});
