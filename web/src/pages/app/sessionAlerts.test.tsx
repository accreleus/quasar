import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionToastHost, useSessionToast } from "./sessionAlerts";

function Probe() {
  const { toast, push } = useSessionToast();
  return (
    <>
      <button onClick={() => push("Bitrate reduced to keep latency low.")}>push</button>
      <SessionToastHost toast={toast} />
    </>
  );
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("session toasts", () => {
  it("shows a pushed message and clears itself", () => {
    render(<Probe />);
    act(() => { screen.getByText("push").click(); });
    expect(screen.getByText(/Bitrate reduced/)).toBeTruthy();

    act(() => { vi.advanceTimersByTime(5000); });
    expect(screen.queryByText(/Bitrate reduced/)).toBeNull();
  });

  it("announces politely — it is informational, never an alert", () => {
    render(<Probe />);
    act(() => { screen.getByText("push").click(); });
    expect(screen.getByRole("status")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
