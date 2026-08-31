// #482 — the loader renders the transport verdict, not the scheduling one.
//
// The decision itself is unit-tested in launchStall.test.ts; this file only
// proves the component is wired to it: the clocks run, the props reach the
// decision, and the stall block shows what the decision returned.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { SessionLoader, PHASE_STALL_MS, TRANSPORT_STALL_MS } from "./SessionLoader";

describe("SessionLoader — transport vs scheduling (#482)", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

  // The live report: session running, host_id set, pipeline live and offering,
  // and the status string is one the stage table does not recognise.
  it("never claims no host picked up a session the host is running", () => {
    render(
      <SessionLoader
        statusMsg="ICE: checking"
        streaming={false}
        onExit={vi.fn()}
        hostAssigned
        sessionRunning
        iceState="checking"
      />,
    );
    advance(PHASE_STALL_MS.host + 2_000);

    expect(screen.queryByText(/no host has picked this session up/i)).not.toBeInTheDocument();
    expect(screen.getByText(/could not establish a media connection/i)).toBeInTheDocument();
  });

  it("names the transport well before any phase budget would have expired", () => {
    render(
      <SessionLoader
        statusMsg="answer sent — awaiting ICE"
        streaming={false}
        onExit={vi.fn()}
        hostAssigned
        sessionRunning
        iceState="checking"
      />,
    );
    advance(TRANSPORT_STALL_MS - 2_000);
    expect(screen.queryByText(/video path/i)).not.toBeInTheDocument();

    advance(4_000);
    expect(screen.getByText(/the host is ready, but the video path is not/i)).toBeInTheDocument();
    // The three things the operator can actually check.
    expect(screen.getByText(/different networks/i)).toBeInTheDocument();
  });

  it("reports a failed ICE path at once rather than waiting out a budget", () => {
    render(
      <SessionLoader
        statusMsg="ICE: failed"
        streaming={false}
        onExit={vi.fn()}
        hostAssigned
        sessionRunning
        iceState="failed"
      />,
    );
    advance(1_000);
    expect(screen.getByText(/the host is ready, but the video path is not/i)).toBeInTheDocument();
    // Still not a dead end.
    expect(screen.getByRole("button", { name: /cancel and go back/i })).toBeInTheDocument();
    // ...but "Keep waiting" is not offered: the verdict reappears on sight, so
    // the wait it promises would end the instant it started.
    expect(screen.queryByRole("button", { name: /keep waiting/i })).not.toBeInTheDocument();
  });

  // The transport clock must NOT re-arm on a status-string change. ICE
  // negotiation moves the derived phase several times ("answer sent",
  // "ICE: checking", "connected"), and a phase-keyed transport clock would push
  // the verdict out indefinitely on exactly the launches it exists to catch.
  it("keeps the transport clock running across a phase change", () => {
    const props = {
      streaming: false,
      onExit: vi.fn(),
      hostAssigned: true,
      sessionRunning: true,
      iceState: "checking" as const,
    };
    const { rerender } = render(<SessionLoader statusMsg="media pipeline" {...props} />);
    advance(TRANSPORT_STALL_MS - 4_000);
    // A phase flip: "media pipeline" and "answer sent" are different stages.
    rerender(<SessionLoader statusMsg="answer sent — awaiting ICE" {...props} />);
    advance(5_000);
    expect(screen.getByText(/the host is ready, but the video path is not/i)).toBeInTheDocument();
  });

  // A WiFi blip must not steal focus onto a button that tears the session down.
  it("does not re-steal focus onto the destructive action when a verdict flaps", () => {
    const props = {
      streaming: false,
      onExit: vi.fn(),
      hostAssigned: true,
      sessionRunning: true,
    };
    const { rerender } = render(
      <SessionLoader statusMsg="ICE: checking" iceState="checking" {...props} />,
    );
    advance(TRANSPORT_STALL_MS + 1_000);
    const exit = screen.getByRole("button", { name: /cancel and go back/i });
    expect(exit).toHaveFocus();

    // The path comes back, the verdict retracts, focus moves elsewhere...
    rerender(<SessionLoader statusMsg="connected" iceState="connected" {...props} />);
    advance(1_000);
    expect(screen.queryByRole("button", { name: /cancel and go back/i })).not.toBeInTheDocument();
    act(() => document.body.focus());

    // ...and it dips again. Same generation, so no second focus grab.
    rerender(<SessionLoader statusMsg="ICE: checking" iceState="checking" {...props} />);
    advance(1_000);
    expect(screen.getByRole("button", { name: /cancel and go back/i })).not.toHaveFocus();
  });

  it("keeps the scheduling copy while the session really is unplaced", () => {
    render(<SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} />);
    advance(PHASE_STALL_MS.host + 1_000);
    expect(screen.getByText(/no host has picked this session up/i)).toBeInTheDocument();
  });

  it("says a host has it and the game is starting when placed but not yet running", () => {
    render(
      <SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} hostAssigned />,
    );
    advance(PHASE_STALL_MS.host + 1_000);
    expect(screen.queryByText(/no host has picked this session up/i)).not.toBeInTheDocument();
    expect(screen.getByText(/still starting the game/i)).toBeInTheDocument();
  });

  it("stays quiet once ICE connects", () => {
    render(
      <SessionLoader
        statusMsg="connected"
        streaming={false}
        onExit={vi.fn()}
        hostAssigned
        sessionRunning
        iceState="connected"
      />,
    );
    advance(TRANSPORT_STALL_MS + 5_000);
    expect(screen.queryByText(/video path/i)).not.toBeInTheDocument();
  });
});
