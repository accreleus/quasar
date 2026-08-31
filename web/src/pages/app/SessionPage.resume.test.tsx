// #524 — arriving at /app/session/{id} WITHOUT router state.
//
// Router state is written once, by the in-page launch flow. The home rail's
// live-card Resume, a bookmark, and a plain reload all carry none, and the page
// used to answer every one of them with a silent `navigate("/app")`. The
// building block for the right answer already existed:
// `POST /v1/sessions/{id}/signaling-token` mints a fresh single-use envelope
// for a session the caller may still attach to.
//
// What this file pins:
//   1. a session that is still alive is RESUMED — one GET, one mint, and the
//      runtime is handed the minted coordinates,
//   2. a session that is gone/terminal, or a mint the server refuses, bounces
//      WITH the server's own message,
//   3. the mint happens AT MOST ONCE. It is single-use, so a StrictMode double
//      effect that minted twice would burn the token the first mint is about
//      to connect with.

import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage } from "./SessionPage";
import { ApiError } from "../../api/client";
import { ToastProvider } from "../../components/Toast";
import { ThemeProvider } from "../../settings/ThemeContext";

const getSession = vi.fn();
const stopSession = vi.fn();
const mintSignalingToken = vi.fn();
const updateSessionDisplay = vi.fn();
vi.mock("../../api/library", () => ({
  getSession: (...a: unknown[]) => getSession(...a),
  stopSession: (...a: unknown[]) => stopSession(...a),
  mintSignalingToken: (...a: unknown[]) => mintSignalingToken(...a),
  updateSessionDisplay: (...a: unknown[]) => updateSessionDisplay(...a),
}));

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "t" }) }));

vi.mock("../../input/capture", () => ({
  setupCapture: () => ({
    cleanup: () => {},
    getMetrics: () => ({ pointerLocked: false, pads: [] }),
    syncKeyboardLock: () => {},
    engage: () => Promise.resolve({ mode: "pointer-lock" }),
    release: () => {},
  }),
  keyboardLockSupported: () => false,
  pointerLockSupported: () => true,
  touchLookSupported: () => false,
}));

vi.mock("../../webrtc/mic", () => ({
  MicCapture: class {
    stop() {}
    start() {
      return Promise.resolve({} as MediaStreamTrack);
    }
    onEnded: (() => void) | null = null;
  },
  MicCaptureError: class extends Error {
    detail = { title: "", message: "" };
  },
}));

// Only the measurement loops are stubbed; EMPTY_SNAPSHOT and the rest stay
// real, because the HUD's readouts import them.
vi.mock("../../webrtc/telemetry", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../webrtc/telemetry")>();
  return {
    ...actual,
    SessionTelemetry: class {
      onUpdate() {}
      start() {}
      stop() {}
      setDecodeFailed() {}
    },
  };
});

vi.mock("../../webrtc/traceEvents", () => ({
  TraceEventEmitter: class {
    start() {}
    stop() {}
    emitPlayoutChanged() {}
    emitWebRtcStateChanged() {}
    emitFreezeDetected() {}
  },
}));

/**
 * Every transport the runtime actually constructed. `requestOfferOnOpen` is the
 * 8th constructor argument and is recorded on purpose: a mock that dropped it
 * could not see the difference between a resume that asks the host to re-offer
 * and one that sits on a silent socket forever.
 */
const transports: Array<{ url: string; token: string; requestOfferOnOpen: boolean }> = [];
vi.mock("../../webrtc/session", () => ({
  QuasarSession: class {
    videoReceiver = null;
    onWebRtcStateChange: unknown;
    onAudioTrack: unknown;
    constructor(
      url: string,
      token: string,
      _onTrack: unknown,
      _onStatus: unknown,
      _onChannel: unknown,
      _initialPlayoutMs?: number,
      _onRecoveryState?: unknown,
      requestOfferOnOpen = false,
    ) {
      transports.push({ url, token, requestOfferOnOpen });
    }
    close() {}
    getStats() {
      return Promise.resolve({});
    }
    hasAbsCaptureTimeExtension() {
      return false;
    }
    hasMicSlot() {
      return false;
    }
    recoverMediaPath() {}
    mediaPathFlowing() {}
  },
}));

vi.mock("./SessionSwapController", () => ({
  SessionSwapController: ({
    children,
  }: {
    children: (p: { quickSwitch: null; swappingTo: null }) => React.ReactNode;
  }) => children({ quickSwitch: null, swappingTo: null }),
}));

function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    id: "s1",
    app_id: "a1",
    state: "running",
    state_detail: "app presented",
    error_message: null,
    failure_code: null,
    app_log_tail: null,
    started_at: new Date().toISOString(),
    stream: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 8000 },
    ...overrides,
  };
}

/** A path probe, so the bounce is asserted on the router rather than on a
 *  mocked navigate that would prove nothing about the real one. */
function PathProbe() {
  return <span data-testid="path">{useLocation().pathname}</span>;
}

/** Render at /app/session/s1 with NO router state — the #524 arrival. */
function renderResumed(strict = false) {
  const tree = (
    <MemoryRouter initialEntries={["/app/session/s1"]}>
      <ThemeProvider>
        <ToastProvider>
          <Routes>
            <Route
              path="/app/session/:id"
              element={
                <>
                  <SessionPage />
                  <PathProbe />
                </>
              }
            />
            <Route path="/app" element={<PathProbe />} />
          </Routes>
        </ToastProvider>
      </ThemeProvider>
    </MemoryRouter>
  );
  return render(strict ? <StrictMode>{tree}</StrictMode> : tree);
}

const settle = () => act(async () => void (await Promise.resolve()));

beforeEach(() => {
  transports.length = 0;
  getSession.mockResolvedValue({ session: makeSession() });
  mintSignalingToken.mockResolvedValue({
    signaling: { url: "wss://host/v1/signal", token: "fresh-token" },
  });
  stopSession.mockResolvedValue(undefined);
  updateSessionDisplay.mockResolvedValue({ session: makeSession() });
});

afterEach(() => {
  vi.clearAllMocks();
});

describe("resuming a session with no router state (#524)", () => {
  it("mints fresh coordinates and connects instead of bouncing", async () => {
    renderResumed();
    await waitFor(() => expect(mintSignalingToken).toHaveBeenCalledWith("t", "s1"));
    await waitFor(() => expect(transports).toHaveLength(1));
    expect(transports[0]).toMatchObject({ url: "wss://host/v1/signal", token: "fresh-token" });
    // The old behaviour, gone: the page stays on the session route.
    expect(screen.getByTestId("path").textContent).toBe("/app/session/s1");
  });

  it("ASKS THE HOST TO RE-OFFER — a resume that only listens hangs forever", async () => {
    // The host made its one offer to the peer that is now gone. Without
    // `requestOfferOnOpen` (which `replacement: true` is the only route to)
    // the fresh socket opens, nothing arrives, and the loader spins until the
    // user gives up — the headline #524 case, passing every other assertion
    // in this file.
    renderResumed();
    await waitFor(() => expect(transports).toHaveLength(1));
    expect(transports[0].requestOfferOnOpen).toBe(true);
  });

  it("shows the launch loader while the mint is in flight", async () => {
    mintSignalingToken.mockImplementation(() => new Promise(() => {}));
    renderResumed();
    await settle();
    expect(document.querySelector(".sl-root")).toBeTruthy();
    expect(screen.getByTestId("path").textContent).toBe("/app/session/s1");
  });

  it("mints AT MOST ONCE — the token is single-use", async () => {
    renderResumed(true); // StrictMode: every effect body runs twice
    await waitFor(() => expect(mintSignalingToken).toHaveBeenCalled());
    await settle();
    expect(mintSignalingToken).toHaveBeenCalledTimes(1);
  });

  it("bounces WITH a message when the server refuses to reconnect", async () => {
    mintSignalingToken.mockRejectedValue(
      new ApiError(409, "conflict", "session is not reconnectable"),
    );
    renderResumed();

    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/app"));
    // The server's own words — the point of the issue is that the old bounce
    // said nothing at all.
    expect(await screen.findByText(/session is not reconnectable/)).toBeInTheDocument();
  });

  it("bounces WITH a message when the session is already terminal", async () => {
    getSession.mockResolvedValue({ session: makeSession({ state: "stopped" }) });
    renderResumed();

    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/app"));
    expect(await screen.findByText(/already ended/i)).toBeInTheDocument();
    // No point burning a single-use token on a dead session.
    expect(mintSignalingToken).not.toHaveBeenCalled();
  });

  it("bounces WITH a message when the session is not there (or not yours)", async () => {
    getSession.mockRejectedValue(new ApiError(404, "not_found", "session not found"));
    renderResumed();

    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/app"));
    expect(await screen.findByText(/no longer exists/i)).toBeInTheDocument();
    expect(mintSignalingToken).not.toHaveBeenCalled();
  });

  it("still waits out a session that has not finished starting", async () => {
    // pending/assigned/starting are NOT dead — the loader already renders
    // launch progress, so a deep link waits exactly as the launching tab does.
    getSession.mockResolvedValue({ session: makeSession({ state: "starting" }) });
    renderResumed();
    await waitFor(() => expect(mintSignalingToken).toHaveBeenCalled());
    expect(screen.getByTestId("path").textContent).toBe("/app/session/s1");
  });

  it("leaves a FAILED session to the loader's own failure verdict", async () => {
    // The poller renders the failure code's headline, the operator message and
    // the app log tail. A bounce here would race it and win with a generic
    // toast — on a page the user is no longer looking at.
    getSession.mockResolvedValue({
      session: makeSession({ state: "failed", failure_code: "app_exited_early" }),
    });
    renderResumed();
    await settle();
    await settle();
    expect(mintSignalingToken).not.toHaveBeenCalled();
    expect(screen.getByTestId("path").textContent).toBe("/app/session/s1");
    expect(screen.queryByText(/Can't resume that session/)).not.toBeInTheDocument();
  });

  it("says nothing once the user has already left", async () => {
    // Back during the GET/mint used to fire a toast AND a navigate("/app")
    // from an unmounted page — yanking the user off wherever they went.
    let reject: (e: unknown) => void = () => {};
    getSession.mockImplementation(() => new Promise((_, rej) => (reject = rej)));
    const { unmount } = renderResumed();
    await settle();
    unmount();
    reject(new ApiError(404, "not_found", "session not found"));
    await settle();
    expect(screen.queryByText(/no longer exists/i)).not.toBeInTheDocument();
  });
});
