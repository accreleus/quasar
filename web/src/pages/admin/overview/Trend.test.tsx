import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { Trend } from "./Trend";

class MockResizeObserver {
  observe() {}
  disconnect() {}
}
globalThis.ResizeObserver = MockResizeObserver as unknown as typeof ResizeObserver;

describe("Trend", () => {
  it("draws no path for a single sample — one poll is a level, not a trend", () => {
    const { container } = render(<Trend points={[60]} color="var(--success)" />);
    expect(container.querySelector(".spark")).toBeTruthy();
    expect(container.querySelector("path")).toBeNull();
  });

  it("draws a line once a second sample lands", () => {
    const { container } = render(<Trend points={[60, 59]} color="var(--success)" />);
    expect(container.querySelectorAll("path")).toHaveLength(1);
  });
});
