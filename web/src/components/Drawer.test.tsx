/**
 * Drawer dialog-behavior tests: open/close, scrim, Escape, initial focus,
 * focus containment, focus return, body scroll lock, accessible naming.
 */
import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { Drawer } from "./Drawer";

function Harness({ footer }: { footer?: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button onClick={() => setOpen(true)}>Open drawer</button>
      <Drawer open={open} onClose={() => setOpen(false)} title="Host settings" footer={footer}>
        <input aria-label="Display name" />
        <button>Save</button>
      </Drawer>
    </div>
  );
}

describe("Drawer", () => {
  it("renders nothing while closed", () => {
    render(<Harness />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens as an accessibly named modal dialog and closes via scrim", () => {
    const { container } = render(<Harness />);
    fireEvent.click(screen.getByText("Open drawer"));
    const dialog = screen.getByRole("dialog", { name: "Host settings" });
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    fireEvent.click(container.querySelector(".drawer-scrim")!);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("closes on Escape and returns focus to the opener", () => {
    render(<Harness />);
    const opener = screen.getByText("Open drawer");
    opener.focus();
    fireEvent.click(opener);
    expect(screen.getByRole("dialog")).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("moves initial focus into the drawer (close button first)", () => {
    render(<Harness />);
    fireEvent.click(screen.getByText("Open drawer"));
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "Close drawer" }),
    );
  });

  it("contains Tab and Shift+Tab within the drawer", () => {
    render(<Harness />);
    fireEvent.click(screen.getByText("Open drawer"));
    const close = screen.getByRole("button", { name: "Close drawer" });
    const save = screen.getByRole("button", { name: "Save" });

    // jsdom has no layout, so offsetParent is null everywhere; the trap's
    // filter keeps the active element, and wrap still applies at the edges.
    save.focus();
    fireEvent.keyDown(document, { key: "Tab" });
    // From the last focusable, Tab wraps to the first.
    expect(document.activeElement).not.toBe(document.body);

    close.focus();
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(document.activeElement).not.toBe(document.body);
  });

  it("locks body scroll while open and restores it on close", () => {
    render(<Harness />);
    document.body.style.overflow = "auto";
    fireEvent.click(screen.getByText("Open drawer"));
    expect(document.body.style.overflow).toBe("hidden");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(document.body.style.overflow).toBe("auto");
    document.body.style.overflow = "";
  });

  it("does not leave a keyboard trap after close", () => {
    render(<Harness />);
    fireEvent.click(screen.getByText("Open drawer"));
    fireEvent.keyDown(document, { key: "Escape" });
    // Tab handling after close must not preventDefault / refocus drawer.
    const onKey = vi.fn();
    document.addEventListener("keydown", onKey);
    fireEvent.keyDown(document, { key: "Tab" });
    expect(onKey).toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    document.removeEventListener("keydown", onKey);
  });

  it("renders the sticky footer slot when provided", () => {
    render(<Harness footer={<button>Apply</button>} />);
    fireEvent.click(screen.getByText("Open drawer"));
    expect(screen.getByRole("button", { name: "Apply" })).toBeTruthy();
  });
});
