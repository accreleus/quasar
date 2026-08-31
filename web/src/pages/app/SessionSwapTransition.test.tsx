import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { SessionSwapTransition } from "./SessionSwapTransition";

describe("SessionSwapTransition", () => {
  it("renders hidden (no .show, aria-hidden) when there is no transition", () => {
    const { container } = render(<SessionSwapTransition transition={null} />);
    const el = container.querySelector(".switcher");
    expect(el).not.toBeNull();
    expect(el?.classList.contains("show")).toBe(false);
    expect(el?.getAttribute("aria-hidden")).toBe("true");
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
    expect(screen.getByText(/taking longer than expected|check back/i)).toBeTruthy();
  });

  it("carries no focusable content in any phase", () => {
    const { container } = render(
      <SessionSwapTransition transition={{ phase: "error", appName: "Purple App", message: "nope" }} />,
    );
    expect(container.querySelectorAll("button, a, input, select, textarea, [tabindex]").length).toBe(0);
  });
});
