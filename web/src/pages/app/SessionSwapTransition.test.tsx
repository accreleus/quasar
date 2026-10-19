import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { GETTING_READY_MS, GETTING_READY_PHRASES } from "./gettingReady";
import { SessionSwapTransition } from "./SessionSwapTransition";

describe("SessionSwapTransition", () => {
  afterEach(() => vi.useRealTimers());

  it("renders hidden (no .show, aria-hidden) when there is no transition", () => {
    const { container } = render(<SessionSwapTransition transition={null} />);
    const el = container.querySelector(".switcher");
    expect(el).not.toBeNull();
    expect(el?.classList.contains("show")).toBe(false);
    expect(el?.getAttribute("aria-hidden")).toBe("true");
  });

  it("puts no text in the document while idle, and drops it once the fade-out is over (#529)", () => {
    vi.useFakeTimers();
    const { container, rerender } = render(<SessionSwapTransition transition={null} />);
    expect(container.textContent).toBe("");
    // #544: the animated mark stays; removing it made frame presentation read less even.
    expect(container.querySelector(".switcher svg")).not.toBeNull();

    rerender(<SessionSwapTransition transition={{ phase: "switching", appName: "Purple App" }} />);
    expect(container.textContent).toContain("Starting…");

    rerender(<SessionSwapTransition transition={null} />);
    const el = container.querySelector(".switcher");
    expect(el?.classList.contains("show")).toBe(false);
    expect(container.textContent).toContain("Starting…"); // still fading out
    act(() => {
      vi.advanceTimersByTime(400);
    });
    expect(container.textContent).toBe("");
  });

  it("shows Steam's shader figure under 'Starting…' only when it is above 0", () => {
    const { container, rerender } = render(
      <SessionSwapTransition transition={{ phase: "booting", appName: "Purple App", shaderPercent: 42 }} />,
    );
    expect(container.textContent).toContain("Starting…");
    expect(screen.getByText("Processing shaders — 42%")).toBeInTheDocument();

    rerender(<SessionSwapTransition transition={{ phase: "booting", appName: "Purple App", shaderPercent: 0 }} />);
    expect(screen.queryByText(/Processing shaders/)).toBeNull();
    expect(GETTING_READY_PHRASES).toContain(container.querySelector(".sw-shaders")?.textContent);
    expect(container.querySelector(".sw-shaders")).toHaveAttribute("aria-hidden", "true");
  });

  it("rotates the phrase while booting, and shows no line before the commit", () => {
    vi.useFakeTimers();
    const { container, rerender } = render(
      <SessionSwapTransition transition={{ phase: "switching", appName: "Purple App" }} />,
    );
    expect(container.querySelector(".sw-shaders")).toBeEmptyDOMElement();

    rerender(<SessionSwapTransition transition={{ phase: "booting", appName: "Purple App", shaderPercent: null }} />);
    const seen = new Set([container.querySelector(".sw-shaders")?.textContent]);
    for (let i = 0; i < 12; i++) {
      act(() => {
        vi.advanceTimersByTime(GETTING_READY_MS);
      });
      seen.add(container.querySelector(".sw-shaders")?.textContent);
    }
    expect(seen.size).toBeGreaterThan(1);
  });

  it("shows the target app name and 'Starting…' while switching", () => {
    const { container } = render(
      <SessionSwapTransition transition={{ phase: "switching", appName: "Purple App" }} />,
    );
    const el = container.querySelector(".switcher");
    expect(el?.classList.contains("show")).toBe(true);
    expect(el?.getAttribute("aria-hidden")).toBeNull();
    expect(screen.getByText("Purple App")).toBeTruthy();
    expect(screen.getByText("Starting…")).toBeTruthy();
    expect(container.querySelector(".sw-err")).toBeNull();
  });

  it("shows the failure reason with the .sw-err treatment on error", () => {
    const { container } = render(
      <SessionSwapTransition
        transition={{ phase: "error", appName: "Purple App", message: "The switch could not be started." }}
      />,
    );
    expect(container.querySelector(".sw-sub")).toBeNull();
    expect(container.querySelector(".sw-err")?.textContent).toBe("The switch could not be started.");
  });

  it("shows a plain message on timeout, distinct from the switching subtitle", () => {
    render(<SessionSwapTransition transition={{ phase: "timeout", appName: "Purple App" }} />);
    expect(screen.queryByText("Starting…")).toBeNull();
    expect(screen.getByText(/still waiting on a confirmation from the host/i)).toBeTruthy();
    // An unresolved switch can end the session (control-api.md amendment 23).
    expect(screen.queryByText(/unaffected/i)).toBeNull();
  });

  it("carries no focusable content in any phase", () => {
    const { container } = render(
      <SessionSwapTransition transition={{ phase: "error", appName: "Purple App", message: "nope" }} />,
    );
    expect(container.querySelectorAll("button, a, input, select, textarea, [tabindex]").length).toBe(0);
  });
});
