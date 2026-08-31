// The v3 launch screen (handoff-v3 §D): the status block, the glyph rail and
// the handoff. The phase decision itself is unit-tested in loaderPhases.test.ts
// and the stall decision in launchStall.test.ts; this file proves the component
// is wired to both, and that the headline and the rail are painted from the
// same index.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { SessionLoader } from "./SessionLoader";
import { unreachableFailure } from "./sessionFailure";

const railStates = () =>
  screen.getAllByRole("listitem").map((li) => li.getAttribute("data-state"));

describe("SessionLoader — status block and rail", () => {
  it("shows the verb and the phase word for the live signals", () => {
    render(
      <SessionLoader statusMsg="ws open" streaming={false} onExit={vi.fn()} sessionState="running" wsOpen />,
    );
    expect(screen.getByText("Establishing")).toBeInTheDocument();
    expect(screen.getByText("secure path")).toBeInTheDocument();
    expect(railStates()).toEqual(["done", "active", "idle", "idle"]);
  });

  it("paints the rail and the headline from the same step", () => {
    render(
      <SessionLoader
        statusMsg="connected"
        streaming={false}
        onExit={vi.fn()}
        sessionState="running"
        wsOpen
        pcConnected
        firstFrame
      />,
    );
    expect(screen.getByText("input capture")).toBeInTheDocument();
    expect(railStates()).toEqual(["done", "done", "done", "active"]);
  });

  it("keeps the app-boot arc: the app's name while it is still starting", () => {
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
      />,
    );
    expect(screen.getByText("Starting")).toBeInTheDocument();
    expect(screen.getByText("Portal 2")).toBeInTheDocument();
    // Transport is fully up, but the app has not drawn: no handoff yet (#484 §3.2).
    expect(railStates()).toEqual(["done", "done", "done", "active"]);
  });

  describe("with fake timers", () => {
    beforeEach(() => vi.useFakeTimers());
    afterEach(() => vi.useRealTimers());
    const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

    it("swaps the phase word out and back rather than cutting it", () => {
      const props = { statusMsg: "ws open", streaming: false, onExit: vi.fn(), sessionState: "running" };
      const { rerender, container } = render(<SessionLoader {...props} wsOpen />);
      rerender(<SessionLoader {...props} wsOpen pcConnected />);

      // Mid-swap the old word is still on screen, marked as leaving.
      expect(container.querySelector(".sl-stage.changing")).not.toBeNull();
      expect(screen.getByText("secure path")).toBeInTheDocument();

      advance(120);
      expect(screen.getByText("video channel")).toBeInTheDocument();
      expect(container.querySelector(".sl-stage.changing")).toBeNull();
    });

    it("locks, then hands off to the stream", () => {
      const props = {
        statusMsg: "connected",
        onExit: vi.fn(),
        sessionState: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
      };
      const { rerender, container } = render(<SessionLoader {...props} streaming={false} />);
      const root = () => container.querySelector(".sl-root")!;
      expect(root().className).not.toMatch(/is-locking/);

      // Every step done — the handoff sequence starts.
      rerender(<SessionLoader {...props} streaming inputOpen />);
      expect(root().className).toMatch(/is-locking/);
      // The rail and the headline turn over together, after the word swap.
      advance(120);
      expect(screen.getByText("Opening")).toBeInTheDocument();
      expect(screen.getByText("your stream")).toBeInTheDocument();
      expect(railStates()).toEqual(["done", "done", "done", "done"]);
      expect(root()).not.toHaveAttribute("inert");

      advance(1_100);
      expect(root().className).toMatch(/is-streaming/);
      // Nothing behind the fade may take focus or be read out.
      expect(root()).toHaveAttribute("inert");
      expect(root()).toHaveAttribute("aria-hidden", "true");
    });

    // #484 §3.2 — the transport can be fully up a minute before a cold app draws
    // anything, and the 1 Hz poll lags it. Handing off on step 4 revealed an
    // empty compositor scene; retracting to step 3 when the poll caught up then
    // stranded the scene mid-lock (`forwards` animations at opacity 0).
    it("never hands off on the transport signals alone", () => {
      const props = {
        statusMsg: "connected",
        onExit: vi.fn(),
        appName: "Portal 2",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
        inputOpen: true,
      };
      // Every signal true, but the poll still reports the pre-running state.
      const { rerender, container } = render(
        <SessionLoader {...props} streaming={false} sessionState="starting" />,
      );
      const root = () => container.querySelector(".sl-root")!;
      expect(root().className).not.toMatch(/is-locking|is-streaming/);

      // The poll catches up: running, but the app has not presented.
      rerender(
        <SessionLoader
          {...props}
          streaming={false}
          statusMsg="app booting"
          sessionState="running"
          appLaunchState="starting"
        />,
      );
      advance(1_400);
      expect(root().className).not.toMatch(/is-locking|is-streaming/);
      expect(screen.getByText("Starting")).toBeInTheDocument();
      expect(screen.getByText("Portal 2")).toBeInTheDocument();

      // Only the page's reveal gate starts the handoff.
      rerender(
        <SessionLoader
          {...props}
          streaming
          statusMsg="app booting"
          sessionState="running"
          appLaunchState="starting"
        />,
      );
      expect(root().className).toMatch(/is-locking/);
      advance(1_180);
      expect(root().className).toMatch(/is-streaming/);
    });

    // The gate can retract: a poll in flight when it opened lands afterwards
    // still saying "app booting". The lock must not be torn down under the
    // half-played animation.
    it("keeps the handoff once it has started, even if the reveal gate retracts", () => {
      const props = {
        statusMsg: "app booting",
        onExit: vi.fn(),
        appName: "Portal 2",
        sessionState: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
        inputOpen: true,
      };
      const { rerender, container } = render(<SessionLoader {...props} streaming />);
      const root = () => container.querySelector(".sl-root")!;
      expect(root().className).toMatch(/is-locking/);

      rerender(<SessionLoader {...props} streaming={false} appLaunchState="starting" />);
      advance(1_180);
      expect(root().className).toMatch(/is-streaming/);
    });

    it("puts the stall copy in the status block without dropping the progress copy", () => {
      render(
        <SessionLoader
          statusMsg="scheduling host"
          streaming={false}
          onExit={vi.fn()}
          sessionState="assigned"
          stallMs={5_000}
        />,
      );
      advance(6_000);
      expect(screen.getByText(/no host has picked this session up/i)).toBeInTheDocument();
      // The rail and the phase word are still there: a stall is not terminal.
      expect(screen.getByText("connection")).toBeInTheDocument();
      expect(railStates()).toEqual(["active", "idle", "idle", "idle"]);
    });

    // #482: the transport verdict must survive the step advancing, which is what
    // a status-keyed clock could not do.
    it("names the transport once a running session has no media path", () => {
      render(
        <SessionLoader
          statusMsg="ICE: checking"
          streaming={false}
          onExit={vi.fn()}
          sessionState="running"
          hostAssigned
          sessionRunning
          iceState="checking"
          wsOpen
        />,
      );
      advance(13_000);
      expect(screen.getByText(/the host is ready, but the video path is not/i)).toBeInTheDocument();
      expect(screen.queryByText(/no host has picked this session up/i)).not.toBeInTheDocument();
    });
  });

  it("replaces the whole block with the failure and a way out", () => {
    const { container } = render(
      <SessionLoader
        statusMsg="answer sent"
        streaming={false}
        onExit={vi.fn()}
        sessionState="failed"
        wsOpen
        failure={{
          kind: "failed",
          title: "The app exited before producing any video",
          message: "Steam needs to be online to update.",
          logTail: "exit code 1",
        }}
      />,
    );
    expect(screen.getByText("The app exited before producing any video")).toBeInTheDocument();
    expect(screen.getByText("Steam needs to be online to update.")).toBeInTheDocument();
    expect(screen.getByTestId("failure-log-tail")).toHaveTextContent("exit code 1");
    // The log tail sits in the shared warning note, not in bare prose.
    expect(container.querySelector(".note.warn")).not.toBeNull();
    // No progress theatre once the launch is over.
    expect(screen.queryByRole("list", { name: "Connection progress" })).not.toBeInTheDocument();
    expect(container.querySelector(".sl-quasar")).toBeNull();
  });

  it("still offers a way out on an unreachable transport", () => {
    render(
      <SessionLoader
        statusMsg="answer sent"
        streaming={false}
        onExit={vi.fn()}
        failure={unreachableFailure()}
      />,
    );
    expect(screen.getByRole("button", { name: /back to library/i })).toHaveFocus();
  });
});
