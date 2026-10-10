// #524 — a session that is over for this page releases the microphone and input
// capture; one that may still be streaming keeps both.
//
// Real MicCapture and real input capture (jsdom has no Pointer Lock, so capture
// runs in fallback mode); the transport, telemetry and HUD are doubles.

import { act, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage } from "./SessionPage";
import { ApiError } from "../../api/client";
import { ToastProvider } from "../../components/Toast";
import { ThemeProvider } from "../../settings/ThemeContext";

const getSession = vi.fn();
vi.mock("../../api/library", () => ({
  getSession: (...a: unknown[]) => getSession(...a),
  stopSession: vi.fn(),
  mintSignalingToken: (...a: unknown[]) => mintSignalingToken(...a),
  updateSessionDisplay: vi.fn(),
}));
const mintSignalingToken = vi.fn();

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "t" }) }));

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

type Channel = { readyState: string; onclose: (() => void) | null; send: () => void; bufferedAmount: number };
let lastOnChannel: ((ch: Channel) => void) | null = null;
let lastOnRecovery: ((state: Record<string, unknown>) => void) | null = null;
const attachMicTrack = vi.fn((_track: unknown) => Promise.resolve());
const detachMicTrack = vi.fn(() => Promise.resolve());
let constructed = 0;
vi.mock("../../webrtc/session", () => ({
  QuasarSession: class {
    videoReceiver = null;
    constructor(
      _url: string,
      _token: string,
      _onTrack: unknown,
      _onStatus: unknown,
      onChannel: (ch: Channel) => void,
      _initialPlayoutMs?: number,
      onRecoveryState?: (state: Record<string, unknown>) => void,
    ) {
      constructed++;
      lastOnChannel = onChannel;
      lastOnRecovery = onRecoveryState ?? null;
    }
    close() {}
    signalingUnrecoverable(message: string) {
      lastOnRecovery?.({ phase: "failed", attempt: 0, maxAttempts: 3, message });
    }
    getStats() {
      return Promise.resolve({});
    }
    hasAbsCaptureTimeExtension() {
      return false;
    }
    hasMicSlot() {
      return true;
    }
    attachMicTrack = attachMicTrack;
    detachMicTrack = detachMicTrack;
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

let hud: Record<string, unknown> = {};
vi.mock("./hud/Hud", async () => {
  const { forwardRef } = await import("react");
  return {
    Hud: forwardRef((p: Record<string, unknown>, _ref: unknown) => {
      hud = p;
      return null;
    }),
  };
});

function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    id: "s1",
    app_id: "a1",
    state: "running",
    state_detail: "app presented",
    error_message: null,
    failure_code: null,
    app_log_tail: null,
    started_at: null,
    stream: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 20000 },
    ...overrides,
  };
}
let currentSession = makeSession();

function renderPage() {
  return render(
    <MemoryRouter
      initialEntries={[
        {
          pathname: "/app/session/s1",
          state: {
            signalingUrl: "wss://host/v1/signal",
            signalingToken: "tok",
            appName: "Portal 2",
            appId: "a1",
            tier: "1920×1080@60",
            micGranted: true,
          },
        },
      ]}
    >
      <ThemeProvider>
        <ToastProvider>
          <Routes>
            <Route path="/app/session/:id" element={<SessionPage />} />
            <Route path="/app" element={null} />
          </Routes>
        </ToastProvider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

const track = { kind: "audio", stopped: false, stop() { this.stopped = true; }, addEventListener() {} };
const stream = { getAudioTracks: () => [track], getTracks: () => [track] } as unknown as MediaStream;
let grantMic: (s: MediaStream) => void;
const getUserMedia = vi.fn();

const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
const micIndicator = () => screen.queryByRole("status", { name: "Microphone is on" });

/** Key events the page leaves to the browser are not preventDefault()ed. */
function keyReachesBrowser(code: string): boolean {
  const e = new KeyboardEvent("keydown", { code, key: code, cancelable: true, bubbles: true });
  document.dispatchEvent(e);
  document.dispatchEvent(new KeyboardEvent("keyup", { code, key: code, cancelable: true, bubbles: true }));
  return !e.defaultPrevented;
}

/** Streaming, mic on, input captured. `appPresented` false leaves the launch poll running. */
async function streamingWithMicAndCapture(detail = "app presented"): Promise<Channel> {
  currentSession = makeSession({ state_detail: detail });
  renderPage();
  await advance(1_000);
  const ch: Channel = { readyState: "open", onclose: null, send() {}, bufferedAmount: 0 };
  await act(async () => lastOnChannel?.(ch));
  await advance(3_000);
  getUserMedia.mockResolvedValue(stream);
  await act(async () => (hud.onToggleMic as () => void)());
  await act(async () => (hud.onGrab as () => void)());
  expect(micIndicator()).not.toBeNull();
  expect(keyReachesBrowser("Tab")).toBe(false);
  return ch;
}

const releasedAfterVerdict = () => {
  expect(track.stopped).toBe(true);
  expect(micIndicator()).toBeNull();
  expect(keyReachesBrowser("Tab")).toBe(true);
  expect(keyReachesBrowser("Enter")).toBe(true);
};
const stillLive = () => {
  expect(track.stopped).toBe(false);
  expect(micIndicator()).not.toBeNull();
  expect(keyReachesBrowser("Tab")).toBe(false);
};
const refused = () =>
  mintSignalingToken.mockRejectedValue(
    new ApiError(409, "session_not_reconnectable", "session is not reconnectable"),
  );
const failed = { phase: "failed", attempt: 0, maxAttempts: 3, message: "signaling: session not found or already ended" };

beforeEach(() => {
  vi.clearAllMocks();
  lastOnChannel = null;
  lastOnRecovery = null;
  constructed = 0;
  hud = {};
  track.stopped = false;
  getUserMedia.mockImplementation(() => new Promise<MediaStream>((r) => (grantMic = r)));
  vi.stubGlobal("navigator", { ...navigator, mediaDevices: { getUserMedia } });
  Object.defineProperty(window, "isSecureContext", { value: true, configurable: true });
  currentSession = makeSession();
  getSession.mockImplementation(async () => ({ session: currentSession }));
  mintSignalingToken.mockResolvedValue({ signaling: { url: "wss://x", token: "y" } });
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  delete (Element.prototype as unknown as { requestPointerLock?: unknown }).requestPointerLock;
  delete (document as unknown as { exitPointerLock?: unknown }).exitPointerLock;
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("SessionPage — a session that is over for this page releases the mic and input (#524)", () => {
  it("access revoked: the banner's keys and the mic are released while the channel is still open", async () => {
    await streamingWithMicAndCapture();
    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    await advance(5_500);
    expect(screen.getByText("Your access to this app was removed")).not.toBeNull();
    releasedAfterVerdict();
    expect(detachMicTrack).toHaveBeenCalled();
    expect(hud.channelOpen).toBe(true);
  });

  it("host lost", async () => {
    await streamingWithMicAndCapture();
    currentSession = makeSession({ state: "failed", state_detail: "host_lost" });
    await advance(5_500);
    expect(screen.getByText("Host went offline")).not.toBeNull();
    expect(track.stopped).toBe(true);
    expect(keyReachesBrowser("Tab")).toBe(true);
  });

  it("taken over by another tab", async () => {
    await streamingWithMicAndCapture();
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    releasedAfterVerdict();
  });

  it("the control plane reports a failed launch", async () => {
    await streamingWithMicAndCapture("app booting");
    currentSession = makeSession({ state: "failed", state_detail: "boom", error_message: "boom" });
    await advance(1_500);
    releasedAfterVerdict();
  });

  it("recovery failed and the input channel closed: the media path is dead", async () => {
    const ch = await streamingWithMicAndCapture();
    refused();
    await act(async () => {
      ch.onclose?.();
      lastOnRecovery?.(failed);
    });
    await advance(100);
    expect(track.stopped).toBe(true);
    expect(micIndicator()).toBeNull();
  });

  it("a mic permission prompt open when the verdict lands cannot turn the mic on afterwards", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await act(async () => lastOnChannel?.({ readyState: "open", onclose: null, send() {}, bufferedAmount: 0 }));
    await act(async () => (hud.onToggleMic as () => void)());
    expect(getUserMedia).toHaveBeenCalledTimes(1);

    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    await advance(5_500);
    await act(async () => grantMic(stream));

    expect(track.stopped).toBe(true);
    expect(micIndicator()).toBeNull();
    expect(attachMicTrack).not.toHaveBeenCalled();
    expect(screen.queryByText("Microphone failed")).toBeNull();
  });

  it("the mic and Capture input cannot be turned on once the session is over", async () => {
    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    renderPage();
    await advance(1_000);
    await act(async () => lastOnChannel?.({ readyState: "open", onclose: null, send() {}, bufferedAmount: 0 }));
    await advance(6_000);
    expect(screen.getByText("Your access to this app was removed")).not.toBeNull();
    await act(async () => (hud.onToggleMic as () => void)());
    await act(async () => (hud.onGrab as () => void)());
    expect(getUserMedia).not.toHaveBeenCalled();
    expect(keyReachesBrowser("Tab")).toBe(true);
  });
});

describe("SessionPage — a session that may still be streaming keeps the mic and input (#524)", () => {
  it("signalling gave up (unreachable, recovery failed) with the input channel open", async () => {
    const ch = await streamingWithMicAndCapture();
    refused();
    await act(async () => lastOnRecovery?.(failed));
    await advance(100);
    expect(screen.getByText("Connection recovery stopped")).not.toBeNull();
    stillLive();

    // The channel closing is what says the media path went too.
    await act(async () => ch.onclose?.());
    await advance(100);
    expect(track.stopped).toBe(true);
    expect(micIndicator()).toBeNull();
  });

  it("recovery in flight (degraded, signaling-lost)", async () => {
    await streamingWithMicAndCapture();
    // Without a latched pcConnected the runtime would re-seat, which resets the mic by design.
    mintSignalingToken.mockReturnValue(new Promise(() => {}));
    await act(async () => lastOnRecovery?.({ ...failed, phase: "degraded", message: "Network path interrupted" }));
    stillLive();
    await act(async () => lastOnRecovery?.({ ...failed, phase: "signaling-lost", message: "signaling closed (1006)" }));
    stillLive();
  });
});

describe("SessionPage — work still in flight when the verdict lands cannot undo it (#524)", () => {
  const openChannel = () =>
    act(async () => lastOnChannel?.({ readyState: "open", onclose: null, send() {}, bufferedAmount: 0 }));
  const revoke = () => {
    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    return advance(5_500);
  };

  it("a takeover during a replacement-token mint discards the mint: no new transport", async () => {
    let resolveMint: (v: unknown) => void = () => {};
    mintSignalingToken.mockReturnValue(new Promise((r) => (resolveMint = r)));
    renderPage();
    await advance(1_000);
    // Before media connects, signalling loss re-seats through a mint.
    await act(async () => lastOnRecovery?.({ ...failed, phase: "signaling-lost", message: "signaling closed (1006)" }));
    expect(mintSignalingToken).toHaveBeenCalledTimes(1);
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    await act(async () => resolveMint({ signaling: { url: "wss://x", token: "y" } }));
    await advance(100);
    expect(constructed).toBe(1);
  });

  it("a takeover during the mint's backoff ends the retries", async () => {
    mintSignalingToken.mockRejectedValue(new ApiError(503, "unavailable", "control plane unavailable"));
    renderPage();
    await advance(1_000);
    await act(async () => lastOnRecovery?.({ ...failed, phase: "signaling-lost", message: "signaling closed (1006)" }));
    await advance(100);
    expect(mintSignalingToken).toHaveBeenCalledTimes(1);
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    await advance(30_000);
    expect(mintSignalingToken).toHaveBeenCalledTimes(1);
    expect(constructed).toBe(1);
  });

  it("a Pointer Lock grant that lands after the release is handed back", async () => {
    let grant: () => void = () => {};
    Object.defineProperty(Element.prototype, "requestPointerLock", {
      value: () => new Promise<void>((r) => (grant = r)),
      configurable: true,
      writable: true,
    });
    const exitPointerLock = vi.fn(() => {
      Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
      document.dispatchEvent(new Event("pointerlockchange"));
    });
    Object.defineProperty(document, "exitPointerLock", { value: exitPointerLock, configurable: true });

    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    await act(async () => (hud.onGrab as () => void)());
    await revoke();
    expect(screen.getByText("Your access to this app was removed")).not.toBeNull();

    await act(async () => {
      Object.defineProperty(document, "pointerLockElement", {
        value: document.querySelector("video"),
        configurable: true,
      });
      document.dispatchEvent(new Event("pointerlockchange"));
      grant();
    });

    expect(exitPointerLock).toHaveBeenCalled();
    expect(hud.inputCaptured).toBe(false);
    expect(keyReachesBrowser("Tab")).toBe(true);
    expect(keyReachesBrowser("Enter")).toBe(true);
  });

  it("a sender attach that resolves after the verdict leaves the mic off and detaches the track", async () => {
    let resolveAttach: () => void = () => {};
    attachMicTrack.mockImplementationOnce(() => new Promise<void>((r) => (resolveAttach = r)));
    getUserMedia.mockResolvedValue(stream);
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    await act(async () => (hud.onToggleMic as () => void)());
    expect(attachMicTrack).toHaveBeenCalledTimes(1);

    await revoke();
    expect(track.stopped).toBe(true);
    detachMicTrack.mockClear();
    await act(async () => resolveAttach());

    expect(micIndicator()).toBeNull();
    expect(hud.micOn).toBe(false);
    expect(detachMicTrack).toHaveBeenCalled();
  });
});
