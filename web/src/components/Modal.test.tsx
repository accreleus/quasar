/**
 * Modal focus-management tests (UI-04 a11y fix).
 * Covers: dialog role/aria-modal, focus moves into the dialog on open,
 * Tab is trapped within the dialog, and focus is restored to the
 * previously-focused element on close.
 */
import { useState } from "react";
import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { Modal } from "./Modal";

describe("Modal", () => {
  it("renders a dialog with role and aria-modal when open", () => {
    render(
      <Modal open onClose={() => {}} title="Test modal">
        <button>Inner action</button>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Test modal" });
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveAttribute("aria-modal", "true");
  });

  it("renders nothing when closed", () => {
    render(
      <Modal open={false} onClose={() => {}} title="Test modal">
        <button>Inner action</button>
      </Modal>,
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("moves focus into the dialog on open (first focusable element)", () => {
    render(
      <Modal open onClose={() => {}} title="Test modal">
        <button>Inner action</button>
      </Modal>,
    );
    // Close button is rendered first in modal-head, so it should receive focus.
    expect(document.activeElement).toHaveAttribute("aria-label", "Close modal");
  });

  it("restores focus to the previously-focused element on close", () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <div>
          <button onClick={() => setOpen(true)}>Open trigger</button>
          <Modal open={open} onClose={() => setOpen(false)} title="Test modal">
            <button>Inner action</button>
          </Modal>
        </div>
      );
    }
    render(<Harness />);
    const trigger = screen.getByRole("button", { name: "Open trigger" });
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    fireEvent.click(trigger);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(document.activeElement).not.toBe(trigger);

    // Close via Escape
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it("traps Tab focus within the dialog", () => {
    render(
      <Modal open onClose={() => {}} title="Test modal">
        <button>First</button>
        <button>Last</button>
      </Modal>,
    );
    const closeBtn = screen.getByRole("button", { name: "Close modal" });
    const last = screen.getByRole("button", { name: "Last" });

    // Focus the last focusable element and Tab forward — should wrap to the close button (first).
    last.focus();
    expect(document.activeElement).toBe(last);
    fireEvent.keyDown(document, { key: "Tab" });
    expect(document.activeElement).toBe(closeBtn);

    // Focus the first focusable element and Shift+Tab — should wrap to the last.
    closeBtn.focus();
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(last);
  });
});
