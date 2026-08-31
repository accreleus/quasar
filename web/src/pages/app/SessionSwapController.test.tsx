// Integration test for the ACTUAL wiring SessionPage uses — this renders
// SessionSwapController itself (the one thing SessionPage imports for the
// entire quick-switch flow), not a hand-copied stand-in for it. A real click
// on a real rendered tile has to reach the real transition DOM through this
// exact component for these tests to pass.
//
// This exists because isolated unit tests of the pieces (SessionQuickSwitch
// alone, useSwapTransition via renderHook, SessionSwapTransition given a
// hand-built prop) all independently passed while a live-hardware run showed
// the transition screen never appearing and the strip updating identity
// optimistically instead — the wiring BETWEEN the pieces was where it broke,
// and none of those tests exercised that boundary. Testing SessionSwapController
// directly closes that gap: there is now exactly one implementation of the
// wiring (SessionPage.tsx renders this component and nothing else for the
// swap flow), and this file drives it the same way a user does — click a
// tile, read the DOM.
//
// The "two swaps in one session" describe block below exists because a FIRST
// round of this fix (single-swap tests only) shipped a real defect that no
// single-swap test could ever exercise: the control plane's swapper.go does
// not commit state_detail="swapping" as the first thing it does on a swap
// request, so a poll tick for a SECOND swap can — and, on live hardware,
// reliably did — read the FIRST swap's own leftover terminal state_detail
// and misreport it as the second swap's outcome. A harness that only ever
// drives one swap per mount can never observe "leftover state from the
// PREVIOUS swap", because there isn't a previous swap.
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionSwapController } from "./SessionSwapController";

const listApps = vi.fn();
const swapSession = vi.fn();
const getSession = vi.fn();
vi.mock("../../api/library", () => ({
  listApps: (...a: unknown[]) => listApps(...a),
  swapSession: (...a: unknown[]) => swapSession(...a),
  getSession: (...a: unknown[]) => getSession(...a),
}));
vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "t" }) }));

const apps = [
  { id: "a1", name: "Snow" },
  { id: "a2", name: "Redout: Enhanced Edition" },
];

const session = (state: string, state_detail: string | null, app_id = "a1") => ({
  session: { id: "s1", app_id, state, state_detail, error_message: null },
});

function renderController(overrides: Partial<Parameters<typeof SessionSwapController>[0]> = {}) {
  const onCommitted = vi.fn();
  const onToast = vi.fn();
  const onSwapStart = vi.fn();
  render(
    <SessionSwapController
      sessionId="s1"
      authToken="t"
      currentApp={{ id: "a1", name: "Snow" }}
      onCommitted={onCommitted}
      onToast={onToast}
      onSwapStart={onSwapStart}
      {...overrides}
    >
      {({ quickSwitch, swappingTo }) => (
        <>
          {/* Stand-in for SessionStrip's identity text: the exact value
              SessionPage feeds it (appTitle when not swapping). */}
          <div data-testid="strip-identity">{swappingTo ? `Switching to ${swappingTo}` : "Snow"}</div>
          {quickSwitch}
        </>
      )}
    </SessionSwapController>,
  );
  return { onCommitted, onToast, onSwapStart };
}

/** Mirrors what SessionPage actually does: owns `currentApp` as real state,
 *  fed back into SessionSwapController via `onCommitted`, so a SECOND swap in
 *  the same mount sees the first swap's real, adopted identity — exactly
 *  like the live session that surfaced this bug. `renderController` above
 *  (a fixed `currentApp` prop) cannot represent this; it is kept for the
 *  single-swap tests that don't need it. */
function StatefulHarness({
  initialApp,
  onCommittedSpy,
}: {
  initialApp: { id?: string; name: string };
  onCommittedSpy: (appId: string, appName: string) => void;
}) {
  const [currentApp, setCurrentApp] = useState(initialApp);
  return (
    <SessionSwapController
      sessionId="s1"
      authToken="t"
      currentApp={currentApp}
      onCommitted={(appId, appName) => {
        setCurrentApp({ id: appId, name: appName });
        onCommittedSpy(appId, appName);
      }}
      onToast={() => {}}
    >
      {({ quickSwitch, swappingTo }) => (
        <>
          <div data-testid="strip-identity">{swappingTo ? `Switching to ${swappingTo}` : currentApp.name}</div>
          {quickSwitch}
        </>
      )}
    </SessionSwapController>
  );
}

beforeEach(() => {
  listApps.mockReset().mockResolvedValue({ items: apps });
  swapSession.mockReset();
  getSession.mockReset();
});

describe("SessionSwapController — click through to the DOM", () => {
  it("shows the transition overlay and does NOT update identity the instant a tile is clicked", async () => {
    swapSession.mockResolvedValue({ session: { id: "s1", app_id: "a1" } });
    getSession.mockResolvedValue(session("running", "swapping"));

    const { onSwapStart } = renderController();
    await waitFor(() => expect(screen.getByText("Redout: Enhanced Edition")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Redout: Enhanced Edition/ }));

    // The transition DOM — a real screen sample mid-swap, not an internal state read.
    const overlay = document.querySelector(".switcher");
    expect(overlay?.classList.contains("show")).toBe(true);
    expect(document.querySelector(".sw-nm")?.textContent).toBe("Redout: Enhanced Edition");
    // Identity must not have jumped to the target yet.
    expect(screen.getByTestId("strip-identity").textContent).not.toBe("Redout: Enhanced Edition");
    expect(onSwapStart).toHaveBeenCalled();
  });

  it("clears the overlay and reports the committed app only once the server confirms", async () => {
    swapSession.mockResolvedValue({ session: { id: "s1", app_id: "a1" } });
    getSession
      .mockResolvedValueOnce(session("running", "swapping"))
      .mockResolvedValueOnce(session("running", "swap complete", "a2"));

    const { onCommitted } = renderController();
    await waitFor(() => expect(screen.getByText("Redout: Enhanced Edition")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: /Redout: Enhanced Edition/ }));

    await waitFor(() => expect(onCommitted).toHaveBeenCalledWith("a2", "Redout: Enhanced Edition"));
    await waitFor(() => expect(document.querySelector(".switcher.show")).toBeNull());
  });

  it("never shows the overlay and never renders a rail when there is no current app id yet", () => {
    renderController({ currentApp: { name: "Snow" } });
    expect(document.querySelector(".sd-rail")).toBeNull();
    expect(document.querySelector(".switcher.show")).toBeNull();
  });
});

describe("SessionSwapController — two swaps in one session", () => {
  beforeEach(() => {
    // shouldAdvanceTime: true matches useSwapTransition.test.tsx and the
    // established AdminSteamLibrary pattern — plain useFakeTimers() here
    // deadlocks against React's scheduler.
    vi.useFakeTimers({ shouldAdvanceTime: true });
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("the second swap's poll ignores the first swap's leftover state_detail, and the rail badge never disagrees with the strip", async () => {
    const threeApps = [
      { id: "a1", name: "Snow" },
      { id: "a2", name: "Redout: Enhanced Edition" },
      { id: "a3", name: "Ball" },
    ];
    listApps.mockResolvedValue({ items: threeApps });
    swapSession.mockResolvedValue({ session: { id: "s1" } });

    getSession
      // Swap A (Snow -> Redout): genuine progress.
      .mockResolvedValueOnce(session("running", "swapping"))
      .mockResolvedValueOnce(session("running", "swap complete", "a2"))
      // Swap B (Redout -> Ball)'s FIRST tick races the fresh POST and reads
      // swap A's own leftover terminal detail — the exact live defect. This
      // MUST be treated as "keep polling", not as swap B's own answer.
      .mockResolvedValueOnce(session("running", "swap complete", "a2"))
      // Swap B's real progress: swapping, then genuinely completes into a3.
      .mockResolvedValueOnce(session("running", "swapping"))
      .mockResolvedValueOnce(session("running", "swap complete", "a3"));

    const onCommitted = vi.fn();
    render(<StatefulHarness initialApp={{ id: "a1", name: "Snow" }} onCommittedSpy={onCommitted} />);
    await waitFor(() => expect(screen.getByText("Redout: Enhanced Edition")).toBeTruthy());

    // ── Swap A: Snow -> Redout ──────────────────────────────────────────
    fireEvent.click(screen.getByRole("button", { name: "Redout: Enhanced Edition" }));
    await vi.advanceTimersByTimeAsync(1_000);
    await waitFor(() => expect(onCommitted).toHaveBeenCalledWith("a2", "Redout: Enhanced Edition"));
    await waitFor(() => expect(document.querySelector(".switcher.show")).toBeNull());

    expect(screen.getByTestId("strip-identity").textContent).toBe("Redout: Enhanced Edition");
    expect(screen.getByText("PLAYING").closest("button")?.getAttribute("aria-label")).toBe(
      "Redout: Enhanced Edition",
    );

    // ── Swap B: Redout -> Ball ───────────────────────────────────────────
    fireEvent.click(screen.getByRole("button", { name: "Ball" }));

    // Immediately: the overlay is up, naming the NEW target.
    expect(document.querySelector(".switcher.show")).not.toBeNull();
    expect(document.querySelector(".sw-nm")?.textContent).toBe("Ball");

    // Let the stale-read tick resolve. It must NOT commit anything — the
    // reported defect was exactly this tick firing onCommitted with a
    // WRONG pairing (the stale server app_id "a2" alongside the fresh local
    // target name "Ball"), which would read as identity moving to a hybrid
    // of two different apps.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(onCommitted).toHaveBeenCalledTimes(1); // still just swap A's commit
    expect(document.querySelector(".switcher.show")).not.toBeNull(); // overlay still up
    expect(screen.getByTestId("strip-identity").textContent).not.toBe("Ball");
    // The rail must not have moved either — still badging Redout, matching
    // the strip (this is the exact "rail disagrees with the strip" defect).
    expect(screen.getByText("PLAYING").closest("button")?.getAttribute("aria-label")).toBe(
      "Redout: Enhanced Edition",
    );

    // The real progress resolves the swap.
    await vi.advanceTimersByTimeAsync(1_000); // "swapping" observed, armed
    await vi.advanceTimersByTimeAsync(1_000); // "swap complete" -> a3
    await waitFor(() => expect(onCommitted).toHaveBeenCalledWith("a3", "Ball"));
    await waitFor(() => expect(document.querySelector(".switcher.show")).toBeNull());

    // Strip and rail agree on the new identity — no divergence.
    expect(screen.getByTestId("strip-identity").textContent).toBe("Ball");
    expect(screen.getByText("PLAYING").closest("button")?.getAttribute("aria-label")).toBe("Ball");
  });
});
