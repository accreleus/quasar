// Steam's shader progress on the launch screen (amendment 39): shown only when
// the poll hands the loader a percentage, otherwise today's copy.

import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { SessionLoader } from "./SessionLoader";

const boot = (shaderPercent?: number | null) =>
  render(
    <SessionLoader
      statusMsg="app booting"
      streaming={false}
      onExit={vi.fn()}
      appName="Portal 2"
      sessionState="running"
      appLaunchState="starting"
      wsOpen
      pcConnected
      firstFrame
      inputOpen
      shaderPercent={shaderPercent}
    />,
  );

describe("SessionLoader — shader line", () => {
  it("shows the percentage while Steam processes shaders", () => {
    boot(42);
    expect(screen.getByText("Processing shaders — 42%")).toBeInTheDocument();
    expect(screen.getByText("Portal 2")).toBeInTheDocument();
  });

  it("shows 0%", () => {
    boot(0);
    expect(screen.getByText("Processing shaders — 0%")).toBeInTheDocument();
  });

  it.each([undefined, null])("shows today's copy when progress is %s", (v) => {
    boot(v);
    expect(screen.queryByText(/Processing shaders/)).toBeNull();
    expect(screen.getByText("Portal 2")).toBeInTheDocument();
  });
});
