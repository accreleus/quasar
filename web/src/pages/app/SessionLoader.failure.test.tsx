// UX assessment §2.2 — the loader's failure path.
//
// The defect this guards: `phase` only ever advanced on channelOpen, so a launch
// that never connected showed "Establishing connection" indefinitely (held 118 s
// in the audit) with ZERO focusable elements on screen. The load-bearing test
// here is the last one in the file — the loader must never be able to render a
// terminal state with no way out.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act, fireEvent } from "@testing-library/react";
import { SessionLoader, PHASE_STALL_MS } from "./SessionLoader";
import {
  accessRevokedFailure,
  launchFailureFromSession,
  takenOverFailure,
  unreachableFailure,
} from "./sessionFailure";

describe("SessionLoader — terminal failure", () => {
  it("replaces the progress copy with the failure and a way out", () => {
    const onExit = vi.fn();
    render(
      <SessionLoader
        statusMsg="scheduling host"
        streaming={false}
        onExit={onExit}
        failure={launchFailureFromSession({
          state: "failed",
          state_detail: "image_pull_failed",
          error_message: "could not pull quasar-steam:latest",
          failure_code: null,
          app_log_tail: null,
        })}
      />,
    );

    expect(screen.getByText("The launch failed")).toBeInTheDocument();
    // The server's own words, surfaced as small print.
    expect(screen.getByText("could not pull quasar-steam:latest")).toBeInTheDocument();
    // Progress theatre is gone — nothing claims work is still happening.
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /back to library/i }));
    expect(onExit).toHaveBeenCalledTimes(1);
  });

  // First-run-experience §S5.
  it("renders the app_exited_early headline and the log tail, collapsed when long", () => {
    const longLog = Array.from({ length: 12 }, (_, i) => `line ${i}`).join("\n");
    render(
      <SessionLoader
        statusMsg="container started"
        streaming={false}
        onExit={vi.fn()}
        failure={launchFailureFromSession({
          state: "failed",
          state_detail: "app_container_exited",
          error_message: "Steam needs to be online to update.",
          failure_code: "app_exited_early",
          app_log_tail: longLog,
        } as never)}
      />,
    );

    expect(screen.getByText("The app exited before producing any video")).toBeInTheDocument();
    expect(screen.getByText("Steam needs to be online to update.")).toBeInTheDocument();
    // Collapsed by default — the log itself isn't in the DOM yet.
    expect(screen.queryByTestId("failure-log-tail")).not.toBeInTheDocument();
    const expandBtn = screen.getByRole("button", { name: /show app log/i });
    fireEvent.click(expandBtn);
    expect(screen.getByTestId("failure-log-tail")).toHaveTextContent("line 0");
    expect(screen.getByTestId("failure-log-tail")).toHaveTextContent("line 11");
  });

  it("shows a short log tail expanded by default with no collapse control", () => {
    render(
      <SessionLoader
        statusMsg="container started"
        streaming={false}
        onExit={vi.fn()}
        failure={launchFailureFromSession({
          state: "failed",
          state_detail: null,
          error_message: null,
          failure_code: "app_exited_early",
          app_log_tail: "exit code 1",
        } as never)}
      />,
    );
    expect(screen.getByTestId("failure-log-tail")).toHaveTextContent("exit code 1");
    expect(screen.queryByRole("button", { name: /hide app log/i })).not.toBeInTheDocument();
  });

  it("announces itself as an alert and focuses the way out", () => {
    render(
      <SessionLoader
        statusMsg="scheduling host"
        streaming={false}
        onExit={vi.fn()}
        failure={unreachableFailure()}
      />,
    );
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /back to library/i })).toHaveFocus();
  });
});

describe("SessionLoader — stall timeout", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

  it("says nothing while the launch is inside its phase budget", () => {
    render(<SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} />);
    advance(PHASE_STALL_MS.host - 1_000);
    expect(screen.queryByText(/still looking for a host/i)).not.toBeInTheDocument();
  });

  it("names the phase that stalled rather than stalling generically", () => {
    render(<SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} />);
    advance(PHASE_STALL_MS.host + 1_000);
    expect(screen.getByText(/still looking for a host/i)).toBeInTheDocument();
    expect(screen.queryByText(/still starting the game/i)).not.toBeInTheDocument();
  });

  it("attributes a game-boot stall to the game, not to host allocation", () => {
    render(
      <SessionLoader statusMsg="container started" streaming={false} onExit={vi.fn()} />,
    );
    advance(PHASE_STALL_MS.game + 1_000);
    expect(screen.getByText(/still starting the game/i)).toBeInTheDocument();
  });

  it("does not stall a launch that keeps advancing through phases", () => {
    const { rerender } = render(
      <SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} />,
    );
    advance(PHASE_STALL_MS.host - 2_000);
    // Progress: the phase changed, so the budget restarts from zero.
    rerender(
      <SessionLoader statusMsg="container started" streaming={false} onExit={vi.fn()} />,
    );
    advance(PHASE_STALL_MS.host);
    expect(screen.queryByText(/still/i)).not.toBeInTheDocument();
  });

  it("offers both keep-waiting and leave, and keep-waiting arms a fresh budget", () => {
    const onExit = vi.fn();
    render(<SessionLoader statusMsg="scheduling host" streaming={false} onExit={onExit} stallMs={5_000} />);
    advance(6_000);

    expect(screen.getByRole("button", { name: /cancel and go back/i })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /keep waiting/i }));
    expect(screen.queryByText(/still looking for a host/i)).not.toBeInTheDocument();
    // ...and it comes back if it stalls again, rather than going quiet forever.
    advance(6_000);
    expect(screen.getByText(/still looking for a host/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /cancel and go back/i }));
    expect(onExit).toHaveBeenCalledTimes(1);
  });

  it("clears the stall once the stream opens", () => {
    const { rerender } = render(
      <SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} stallMs={5_000} />,
    );
    advance(6_000);
    expect(screen.getByText(/still looking for a host/i)).toBeInTheDocument();
    rerender(
      <SessionLoader statusMsg="connected" streaming onExit={vi.fn()} stallMs={5_000} />,
    );
    expect(screen.queryByText(/still looking for a host/i)).not.toBeInTheDocument();
  });
});

// ── The regression guard ────────────────────────────────────────────────────
//
// Any state in which the loader has stopped making progress must contain at
// least one enabled control. The audit's 118-second dead launch had none.
describe("SessionLoader — never a dead end", () => {
  const enabledButtons = () =>
    screen.queryAllByRole("button").filter((b) => !(b as HTMLButtonElement).disabled);

  it.each([
    [
      "server-reported failure",
      { state: "failed", state_detail: null, error_message: null, failure_code: null, app_log_tail: null },
    ],
    [
      "session ended before it started",
      { state: "stopped", state_detail: null, error_message: null, failure_code: null, app_log_tail: null },
    ],
  ] as const)("offers a way out on a %s", (_label, session) => {
    render(
      <SessionLoader
        statusMsg="scheduling host"
        streaming={false}
        onExit={vi.fn()}
        failure={launchFailureFromSession(session)}
      />,
    );
    expect(enabledButtons().length).toBeGreaterThan(0);
  });

  it("offers a way out on an unreachable transport", () => {
    render(
      <SessionLoader
        statusMsg="answer sent — awaiting ICE"
        streaming={false}
        onExit={vi.fn()}
        failure={unreachableFailure()}
      />,
    );
    expect(enabledButtons().length).toBeGreaterThan(0);
  });

  // #516: the handoff latches, so a verdict that lands after it began renders
  // in a transparent, inert scene. 300 ms is mid-lock, 2000 ms is past the fade.
  describe.each([300, 2_000])("a verdict %d ms into the handoff", (afterMs) => {
    const handedOff = (failure: ReturnType<typeof unreachableFailure> | null) => {
      const props = { statusMsg: "pipeline live", onExit: vi.fn() };
      const { rerender } = render(<SessionLoader {...props} streaming />);
      act(() => void vi.advanceTimersByTime(afterMs));
      const root = document.querySelector(".sl-root") as HTMLElement;
      expect(root.className).toMatch(/is-locking|is-streaming/);
      rerender(<SessionLoader {...props} streaming={false} failure={failure} />);
      // The lock timer still fires after the verdict.
      act(() => void vi.advanceTimersByTime(2_000));
      return root;
    };
    beforeEach(() => void vi.useFakeTimers());
    afterEach(() => void vi.useRealTimers());

    it("takes the scene back for an entitlement revoke, with a way out", () => {
      const failure = accessRevokedFailure({ stop_reason: "entitlement_revoked" });
      const root = handedOff(failure);
      expect(root.className).not.toMatch(/is-locking|is-streaming/);
      expect(root.hasAttribute("inert")).toBe(false);
      expect(root.getAttribute("aria-hidden")).toBeNull();
      expect(root.getAttribute("role")).toBe("alert");
      expect(screen.getByText(failure!.title)).toBeInTheDocument();
      expect(enabledButtons().length).toBeGreaterThan(0);
    });

    // The client's own verdicts: signalling can end while the picture is live,
    // so the handoff completes and the loader stays out of the way.
    it.each([
      ["an unreachable transport", unreachableFailure()],
      ["a takeover", takenOverFailure()],
    ])("completes the handoff and stays out of the way for %s", (_label, failure) => {
      const root = handedOff(failure);
      expect(root.className).toContain("is-streaming");
      expect(root.hasAttribute("inert")).toBe(true);
      expect(root.getAttribute("aria-hidden")).toBe("true");
      expect(screen.queryAllByRole("alert")).toHaveLength(0);
    });
  });

  it("offers a way out on a launch that simply never advances", () => {
    vi.useFakeTimers();
    try {
      render(<SessionLoader statusMsg="scheduling host" streaming={false} onExit={vi.fn()} />);
      // The original defect, reproduced: no status change, ever.
      act(() => void vi.advanceTimersByTime(120_000));
      expect(enabledButtons().length).toBeGreaterThan(0);
    } finally {
      vi.useRealTimers();
    }
  });
});
