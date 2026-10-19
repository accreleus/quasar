// The boot line on the launch screen: Steam's figure when it is above 0, a
// rotating phrase while the app boots otherwise, nothing when it is not booting.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { SessionLoader } from "./SessionLoader";
import { GETTING_READY_MS, GETTING_READY_PHRASES } from "./gettingReady";

const load = (shaderPercent?: number | null, booting = true) =>
  render(
    <SessionLoader
      statusMsg="app booting"
      streaming={false}
      onExit={vi.fn()}
      appName="Portal 2"
      sessionState="running"
      appLaunchState={booting ? "starting" : undefined}
      wsOpen
      pcConnected
      firstFrame
      inputOpen
      shaderPercent={shaderPercent}
    />,
  );
const line = (c: HTMLElement) => c.querySelector(".sl-shaders") as HTMLElement;

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("SessionLoader — boot line", () => {
  it("shows the percentage when Steam reports one above 0", () => {
    const { container } = load(42);
    expect(screen.getByText("Processing shaders — 42%")).toBeInTheDocument();
    expect(line(container)).not.toHaveAttribute("aria-hidden");
    expect(line(container)).toHaveAttribute("aria-live", "off");
  });

  it.each([0, null, undefined])("shows a phrase, hidden from assistive tech, when progress is %s", (v) => {
    const { container } = load(v);
    expect(screen.queryByText(/Processing shaders/)).toBeNull();
    expect(GETTING_READY_PHRASES).toContain(line(container).textContent);
    expect(line(container)).toHaveAttribute("aria-hidden", "true");
    expect(line(container)).toHaveAttribute("aria-live", "off");
  });

  it("changes the phrase on a timer", () => {
    const { container } = load(null);
    const seen = new Set([line(container).textContent]);
    for (let i = 0; i < 12; i++) {
      act(() => {
        vi.advanceTimersByTime(GETTING_READY_MS);
      });
      seen.add(line(container).textContent);
    }
    expect(seen.size).toBeGreaterThan(1);
  });

  it("keeps the line's slot but shows nothing when the app is not booting", () => {
    const { container } = load(42, false);
    expect(line(container)).toBeEmptyDOMElement();
  });
});
