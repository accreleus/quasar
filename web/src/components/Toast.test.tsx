/**
 * Toast provider/host tests: API surface, severity-appropriate live
 * semantics (danger = assertive alert; success/info = polite status),
 * action button, and dismissal.
 */
import { act, fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { ToastProvider, useToast, type ToastVariant } from "./Toast";

function Trigger({ variant, body }: { variant: ToastVariant; body?: string }) {
  const { addToast } = useToast();
  return (
    <button
      onClick={() =>
        addToast({
          variant,
          title: `${variant} title`,
          body,
          action:
            variant === "info"
              ? { label: "Go to session", onClick: () => {} }
              : undefined,
        })
      }
    >
      fire
    </button>
  );
}

function setup(variant: ToastVariant, body?: string) {
  render(
    <ToastProvider>
      <Trigger variant={variant} body={body} />
    </ToastProvider>,
  );
  fireEvent.click(screen.getByText("fire"));
}

describe("Toast", () => {
  it("renders danger toasts as assertive alerts", () => {
    setup("danger");
    const el = screen.getByRole("alert");
    expect(el.getAttribute("aria-live")).toBe("assertive");
    expect(el.className).toContain("danger");
  });

  it("renders success toasts as polite status", () => {
    setup("success", "saved");
    const el = screen.getByRole("status");
    expect(el.getAttribute("aria-live")).toBe("polite");
    expect(screen.getByText("saved")).toBeTruthy();
  });

  it("renders info toasts as polite status with a working action", () => {
    setup("info");
    expect(screen.getByRole("status").className).toContain("info");
    // Action click also dismisses.
    fireEvent.click(screen.getByRole("button", { name: "Go to session" }));
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("auto-dismisses after the duration", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Trigger variant="success" />
      </ToastProvider>,
    );
    fireEvent.click(screen.getByText("fire"));
    expect(screen.getByRole("status")).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(4100);
    });
    expect(screen.queryByRole("status")).toBeNull();
    vi.useRealTimers();
  });
});
