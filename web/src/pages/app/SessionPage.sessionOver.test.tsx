// #524 — a session that is over for this page releases the microphone and input
// capture; one that may still be streaming keeps both.
// #529 — and draws no HUD, summon button or swap overlay; one that may still be
// streaming keeps them.
//
// Real MicCapture and real input capture (jsdom has no Pointer Lock, so capture
// runs in fallback mode); the transport, telemetry and HUD are doubles.

import { act, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage } from "./SessionPage";
import { ApiError } from "../../api/client";
import { ToastProvider } from "../../components/Toast";
import { ThemeProvider } from "../../settings/ThemeContext";
import { RecoveryController } from "../../webrtc/recovery";

const getSession = vi.fn();
vi.mock("../../api/library", () => ({
  getSession: (...a: unknown[]) => getSession(...a),
  stopSession: (...a: unknown[]) => stopSession(...a),
  mintSignalingToken: (...a: unknown[]) => mintSignalingToken(...a),
  updateSessionDisplay: vi.fn(),
}));
const mintSignalingToken = vi.fn();
const stopSession = vi.fn();

vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: "t" }) }));

vi.mock("../../webrtc/telemetry", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../webrtc/telemetry")>();
  return {
    ...actual,
    SessionTelemetry: class {
      onUpdate(l: (snap: Record<string, unknown>) => void) {
        telemetryListener = l;
      }
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

let telemetryListener: ((snap: Record<string, unknown>) => void) | null = null;
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
    sessionOver,
    onToast,
  }: {
    children: (p: { quickSwitch: null; swappingTo: null }) => React.ReactNode;
    sessionOver?: boolean;
    onToast: (n: React.ReactNode) => void;
  }) => {
    swapToast = onToast;
    return sessionOver ? null : children({ quickSwitch: null, swappingTo: null });
  },
}));

let swapToast: ((n: React.ReactNode) => void) | null = null;
let hud: Record<string, unknown> = {};
const hudOpen = vi.fn();
vi.mock("./hud/Hud", async () => {
  const { forwardRef, useImperativeHandle } = await import("react");
  return {
    Hud: forwardRef((p: Record<string, unknown>, ref: React.Ref<unknown>) => {
      hud = p;
      useImperativeHandle(ref, () => ({ open: hudOpen, close() {}, stageClick() {} }));
      // The real HUD's Exit session button is wired to onStop, which stops the server session.
      return (
        <div data-testid="hud">
          <button type="button" onClick={p.onStop as () => void}>
            Exit session
          </button>
        </div>
      );
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
  telemetryListener = null;
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
    expect(detachMicTrack).toHaveBeenCalled();
  });
});

describe("SessionPage — a session that is over for this page draws no HUD (#529)", () => {
  const summonChord = () =>
    document.dispatchEvent(
      new KeyboardEvent("keydown", { code: "KeyQ", key: "Q", ctrlKey: true, altKey: true, shiftKey: true, bubbles: true }),
    );
  const openChannel = () =>
    act(async () => lastOnChannel?.({ readyState: "open", onclose: null, send() {}, bufferedAmount: 0 }));
  const gone = () => {
    expect(screen.queryByTestId("hud")).toBeNull();
    expect(screen.queryByRole("button", { name: "Session menu" })).toBeNull();
    hudOpen.mockClear();
    summonChord();
    expect(hudOpen).not.toHaveBeenCalled();
  };
  const present = () => {
    expect(screen.queryByTestId("hud")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Session menu" })).not.toBeNull();
    hudOpen.mockClear();
    summonChord();
    expect(hudOpen).toHaveBeenCalled();
  };

  it("streaming: the HUD, the summon button and the chord are there", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    present();
  });

  it("access revoked mid-stream: only the banner's button is left", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    present();
    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    await advance(5_500);
    expect(screen.getByText("Your access to this app was removed")).not.toBeNull();
    gone();
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual(["Back to library"]);
  });

  const TAKEN = "This session moved to another tab";
  const onlyWayBack = async () => {
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual(["Back to library"]);
    fireEvent.click(screen.getByRole("button", { name: "Back to library" }));
    await advance(100);
    // The other tab owns the session: nothing here may stop it.
    expect(stopSession).not.toHaveBeenCalled();
  };

  it("taken over by another tab, after the loader is gone: the notice and the way back, no HUD", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    present();
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    await advance(100);
    expect(screen.getByText(TAKEN)).not.toBeNull();
    gone();
    await onlyWayBack();
  });

  it("taken over after signalling gave up with the channel open: the HUD goes, the notice replaces the recovery banner", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    refused();
    const rc = new RecoveryController({ onRetry: () => {}, onState: (st) => lastOnRecovery?.(st as never) });
    await act(async () => rc.terminal("signaling: session not found or already ended"));
    await advance(100);
    expect(screen.getByText("Connection recovery stopped")).not.toBeNull();
    present();

    await act(async () => rc.superseded("opened in another tab"));
    await advance(100);
    expect(screen.getByText(TAKEN)).not.toBeNull();
    expect(screen.queryByText("Connection recovery stopped")).toBeNull();
    gone();
    await onlyWayBack();
  });

  it("signalling gave up with the input channel open: still streaming, so the HUD stays", async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    await openChannel();
    await advance(3_000);
    refused();
    await act(async () => lastOnRecovery?.(failed));
    await advance(100);
    expect(screen.getByText("Connection recovery stopped")).not.toBeNull();
    present();
  });
});

// #527 — a takeover that lands after the page already gave up on the transport.
describe("SessionPage — a late takeover (#527)", () => {
  const TAKEN = "This session moved to another tab";
  const banners = () => document.querySelectorAll(".banner");
  /** The real controller feeding the page, so `failed` -> `superseded` is the real sequence. */
  const controller = () => new RecoveryController({ onRetry: () => {}, onState: (st) => lastOnRecovery?.(st as never) });

  it("after signalling gave up with the channel open: mic and input are released and the takeover is the verdict", async () => {
    await streamingWithMicAndCapture();
    refused();
    const rc = controller();
    await act(async () => rc.terminal("signaling: session not found or already ended"));
    await advance(100);
    expect(screen.getByText("Connection recovery stopped")).not.toBeNull();
    stillLive();

    await act(async () => rc.superseded("opened in another tab"));
    await act(async () => rc.superseded("opened in another tab"));
    await advance(100);

    expect(screen.getByText(TAKEN)).not.toBeNull();
    expect(screen.queryByText("Connection recovery stopped")).toBeNull();
    expect(banners().length).toBe(1);
    releasedAfterVerdict();
  });

  it("after a server verdict: the server's verdict stands", async () => {
    await streamingWithMicAndCapture();
    currentSession = makeSession({ state: "stopped", stop_reason: "entitlement_revoked" });
    await advance(5_500);
    await act(async () => controller().superseded("opened in another tab"));

    expect(screen.getByText("Your access to this app was removed")).not.toBeNull();
    expect(screen.queryByText(TAKEN)).toBeNull();
    expect(banners().length).toBe(1);
  });

  it("a plain mid-stream takeover, loader already gone, shows the notice and a way back", async () => {
    await streamingWithMicAndCapture();
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    await advance(100);

    expect(screen.getByText(TAKEN)).not.toBeNull();
    expect(banners().length).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "Back to library" }));
    await advance(100);
    expect(screen.queryByText(TAKEN)).toBeNull();
  });

  it("never leaves a terminal state with no notice: failed, then superseded", async () => {
    await streamingWithMicAndCapture();
    refused();
    const rc = controller();
    await act(async () => rc.terminal("Peer connection failed (DTLS)"));
    await advance(100);
    expect(banners().length).toBe(1);
    await act(async () => rc.superseded("opened in another tab"));
    await advance(100);
    expect(banners().length).toBe(1);
  });
});

// #529 — a held verdict (takeover, access removed) is the only banner and the only
// action: a Stop from the decoder or health banner would end a session another tab
// owns. Without a verdict (recovery merely failed) the session is still the user's.
describe("SessionPage — a held verdict is the only banner (#529)", () => {
  const TAKEN = "This session moved to another tab";
  const DECODER = "This stream isn’t supported on your device";
  const HEALTH = "Stream quality is unsustainable on your network";
  const stream = async () => {
    currentSession = makeSession();
    renderPage();
    await advance(1_000);
    const ch: Channel = { readyState: "open", onclose: null, send() {}, bufferedAmount: 0 };
    await act(async () => lastOnChannel?.(ch));
    await advance(3_000);
    return ch;
  };
  const decoderLatches = async () => {
    await act(async () => telemetryListener?.({ clientHealth: "client_unsupported", framesDecodedTotal: 0, bytesReceivedTotal: 0 }));
    await advance(100);
    expect(screen.getByText(DECODER)).not.toBeNull();
  };
  const takeover = async () => {
    await act(async () => lastOnRecovery?.({ ...failed, phase: "superseded", message: "opened in another tab" }));
    await advance(100);
  };
  const onlyTheVerdict = async () => {
    expect(screen.getByText(TAKEN)).not.toBeNull();
    expect(screen.queryByText(DECODER)).toBeNull();
    expect(screen.queryByText(HEALTH)).toBeNull();
    expect(document.querySelectorAll(".banner").length).toBe(1);
    expect(screen.getAllByRole("button").map((b) => b.textContent)).toEqual(["Back to library"]);
    expect(stopSession).not.toHaveBeenCalled();
  };

  it("decoder banner latched, then a takeover: only the takeover banner", async () => {
    await stream();
    await decoderLatches();
    await takeover();
    await onlyTheVerdict();
  });

  it("decoder and health notices up, then a takeover: only the takeover banner", async () => {
    await stream();
    await decoderLatches();
    currentSession = makeSession({ health_state: "unsustainable" });
    await advance(5_500);
    expect(screen.getByText(HEALTH)).not.toBeNull();
    await takeover();
    await onlyTheVerdict();
  });

  it("no verdict, recovery failed with the channel gone: the decoder banner keeps its Stop", async () => {
    const ch = await stream();
    await decoderLatches();
    refused();
    await act(async () => {
      ch.onclose?.();
      lastOnRecovery?.(failed);
    });
    await advance(100);
    expect(screen.getByText("Connection recovery stopped")).not.toBeNull();
    expect(screen.getByText(DECODER)).not.toBeNull();
    expect(screen.getAllByRole("button", { name: "Stop" }).length).toBeGreaterThan(0);
  });

  it("a swap finishing under a held verdict raises no toast next to the notice", async () => {
    await stream();
    await act(async () => swapToast?.("Now playing Elsewhere."));
    expect(screen.queryByText(/Now playing/)).not.toBeNull();
    await takeover();
    expect(screen.queryByText(/Now playing/)).toBeNull();
    await act(async () => swapToast?.("Now playing Again."));
    expect(screen.queryByText(/Now playing/)).toBeNull();
  });
});
