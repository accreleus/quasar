// #647 — "Play goes straight in": at the reveal, SessionPage captures input once
// (or offers one "Click to play" layer). Harness copied from SessionPage.test.tsx;
// the capture mock's engage is a spy and the HUD double exposes its props.

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage } from "./SessionPage";
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

vi.mock("../../auth/context", () => ({
  useAuth: () => ({ token: "t" }),
}));

const engageSpy = vi.hoisted(() => vi.fn());
vi.mock("../../input/capture", () => ({
  setupCapture: () => ({
    cleanup: () => {},
    getMetrics: () => ({
      pointerLocked: false,
      coalescedSupported: true,
      inputMsgPerSec: 0,
      coalescedSamplesPerSec: 0,
      channelBufferedAmount: 0,
      backpressureDetected: false,
      gamepadCount: 0,
      pads: [],
      gamepadSendPerSec: 0,
      mmSentPerSec: 0,
      inputTrace: false,
    }),
    syncKeyboardLock: () => {},
    engage: () => engageSpy(),
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

/** Captured from the last QuasarSession constructor call so tests can flip
 *  channelOpen by hand — exactly the ondatachannel/onopen path SessionPage
 *  itself drives, without a real RTCPeerConnection. */
let lastOnChannel: ((ch: { readyState: string; onclose: (() => void) | null }) => void) | null = null;
/** The runtime's recovery sink, same capture: a test plays the transport's
 *  verdict (e.g. the 4404 close) without a WebSocket. */
let lastOnRecovery: ((state: Record<string, unknown>) => void) | null = null;
vi.mock("../../webrtc/session", () => ({
  QuasarSession: class {
    videoReceiver = null;
    onWebRtcStateChange: unknown;
    onAudioTrack: unknown;
    constructor(
      _url: string,
      _token: string,
      _onTrack: unknown,
      _onStatus: unknown,
      onChannel: (ch: { readyState: string; onclose: (() => void) | null }) => void,
      _initialPlayoutMs?: number,
      onRecoveryState?: (state: Record<string, unknown>) => void,
    ) {
      lastOnChannel = onChannel;
      lastOnRecovery = onRecoveryState ?? null;
    }
    close() {}
    // As QuasarSession does: abandoning the reconnect terminalises recovery.
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
      return false;
    }
    detachMicTrack() {
      return Promise.resolve();
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
// Captures the HUD's props (releaseHint) without rendering the HUD.
let lastDrawerProps: Record<string, unknown> | null = null;
vi.mock("./hud/Hud", async () => {
  const { forwardRef } = await import("react");
  return {
    Hud: forwardRef((p: Record<string, unknown>, _ref: unknown) => {
      lastDrawerProps = p;
      return null;
    }),
  };
});

/** Returns the channel so a test can end it (`ch.onclose?.()`). */
function openChannel() {
  const ch: { readyState: string; onclose: (() => void) | null } = { readyState: "open", onclose: null };
  lastOnChannel?.(ch);
  return ch;
}

function makeSession(overrides: Record<string, unknown> = {}) {
  return {
    id: "s1",
    app_id: "a1",
    state: "starting",
    state_detail: null,
    error_message: null,
    failure_code: null,
    app_log_tail: null,
    started_at: null,
    // Required by the contract and the authoritative source for the display
    // controls' ceiling — deliberately DIFFERENT from the router-state tier
    // (1920×1080) so a test can tell which one the page actually read.
    stream: { width: 2560, height: 1440, fps: 60, bitrate_kbps: 20000 },
    health_state: undefined,
    health_reason: undefined,
    ...overrides,
  };
}

let currentSession: Record<string, unknown> = makeSession();

/** Where handleStop lands. The post-session summary travels in router state,
 *  so this is the only place a test can read what the page actually reported. */
function SummaryProbe() {
  const state = useLocation().state as { sessionSummary?: unknown } | null;
  return <pre data-testid="summary">{JSON.stringify(state?.sessionSummary ?? null)}</pre>;
}

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
          },
        },
      ]}
    >
      <ThemeProvider>
        <ToastProvider>
          <Routes>
            <Route path="/app/session/:id" element={<SessionPage />} />
            <Route path="/app" element={<SummaryProbe />} />
          </Routes>
        </ToastProvider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}


const PREFS_KEY = "quasar.display.preferences";
let fsEl: Element | null = null;
const requestFullscreen = vi.fn(() => Promise.resolve());

beforeEach(() => {
  vi.clearAllMocks();
  lastOnChannel = null;
  lastOnRecovery = null;
  lastDrawerProps = null;
  updateSessionDisplay.mockResolvedValue({ session: makeSession({ state: "running" }) });
  getSession.mockImplementation(async () => ({ session: currentSession }));
  stopSession.mockResolvedValue(undefined);
  mintSignalingToken.mockResolvedValue({ signaling: { url: "wss://x", token: "y" } });
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
  engageSpy.mockReset();
  engageSpy.mockImplementation(() => Promise.resolve({ mode: "pointer-lock" }));
  localStorage.clear();
  fsEl = null;
  Object.defineProperty(document, "fullscreenElement", { configurable: true, get: () => fsEl });
  document.documentElement.requestFullscreen = requestFullscreen;
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  localStorage.clear();
  // @ts-expect-error: drop the instance override so the next test starts clean
  delete document.fullscreenElement;
  // @ts-expect-error: jsdom has no requestFullscreen
  delete document.documentElement.requestFullscreen;
});

const advance = (ms: number) =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });

/** Page up with the app presented, channel open, and the loader's lock animation (1180 ms) done. */
async function reveal() {
  currentSession = makeSession({ state: "running", state_detail: "app presented" });
  renderPage();
  await advance(1_000);
  await act(async () => {
    openChannel();
  });
  await advance(1_500);
}

const playLayer = () => screen.queryByText("Click to play");

describe("SessionPage — #647 straight-in capture", () => {
  it("engages once at the reveal when already full screen, with no Click to play", async () => {
    fsEl = document.documentElement;
    await reveal();
    expect(engageSpy).toHaveBeenCalledTimes(1);
    expect(playLayer()).toBeNull();
    await advance(5_000);
    expect(engageSpy).toHaveBeenCalledTimes(1);
  });

  it("shows Click to play, without engaging, when not full screen; the click engages and goes full screen", async () => {
    await reveal();
    expect(engageSpy).not.toHaveBeenCalled();
    expect(playLayer()).not.toBeNull();

    await act(async () => {
      fireEvent.click(screen.getByText("Click to play"));
    });
    expect(requestFullscreen).toHaveBeenCalledTimes(1);
    expect(engageSpy).toHaveBeenCalledTimes(1);
    expect(playLayer()).toBeNull();
  });

  it("Click to play captures before it asks for full screen (Firefox/Safari spend the click on full screen)", async () => {
    const order: string[] = [];
    engageSpy.mockImplementation(() => (order.push("engage"), Promise.resolve({ mode: "pointer-lock" })));
    requestFullscreen.mockImplementation(() => (order.push("fullscreen"), Promise.resolve()));
    await reveal();
    await act(async () => {
      fireEvent.click(screen.getByText("Click to play"));
    });
    expect(order).toEqual(["engage", "fullscreen"]);
  });

  it("leaving the session leaves the full screen Click to play entered", async () => {
    requestFullscreen.mockImplementation(() => {
      fsEl = document.documentElement;
      return Promise.resolve();
    });
    document.exitFullscreen = vi.fn(() => Promise.resolve());
    await reveal();
    await act(async () => {
      fireEvent.click(screen.getByText("Click to play"));
    });
    cleanup(); // unmounts the page: the player left the session
    await advance(10);
    expect(document.exitFullscreen).toHaveBeenCalledTimes(1);
  });

  it("shows Click to play when engage reports failed, and never retries by itself", async () => {
    fsEl = document.documentElement;
    engageSpy.mockImplementation(() => Promise.resolve({ mode: "failed" }));
    await reveal();
    expect(engageSpy).toHaveBeenCalledTimes(1);
    expect(playLayer()).not.toBeNull();
    await advance(5_000);
    expect(engageSpy).toHaveBeenCalledTimes(1);
  });

  const rawToast = () => screen.queryAllByText("Raw mouse input isn’t available");

  it("raw mouse refused: the straight-in capture says so once", async () => {
    fsEl = document.documentElement;
    engageSpy.mockImplementation(() => Promise.resolve({ mode: "pointer-lock", rawUnavailable: true }));
    await reveal();
    expect(engageSpy).toHaveBeenCalledTimes(1);
    expect(rawToast()).toHaveLength(1);
  });

  it("raw mouse refused: Click to play says so too, and the two capture paths say it once between them", async () => {
    engageSpy.mockImplementation(() => Promise.resolve({ mode: "pointer-lock", rawUnavailable: true }));
    await reveal();
    expect(rawToast()).toHaveLength(0);
    await act(async () => {
      fireEvent.click(screen.getByText("Click to play"));
    });
    expect(rawToast()).toHaveLength(1);
  });

  it("raw mouse refused on both paths in one view: one notice", async () => {
    fsEl = document.documentElement;
    engageSpy.mockImplementation(() => Promise.resolve({ mode: "failed", rawUnavailable: true }));
    await reveal();
    expect(rawToast()).toHaveLength(1);
    expect(playLayer()).not.toBeNull(); // the straight-in capture failed, so the layer offers a retry
    engageSpy.mockImplementation(() => Promise.resolve({ mode: "pointer-lock", rawUnavailable: true }));
    await act(async () => {
      fireEvent.click(screen.getByText("Click to play"));
    });
    expect(rawToast()).toHaveLength(1);
  });

  it("raw mouse granted: no notice", async () => {
    fsEl = document.documentElement;
    await reveal();
    expect(rawToast()).toHaveLength(0);
  });

  it("with the setting off, never engages at the reveal, shows no layer, and passes no release hint", async () => {
    localStorage.setItem(PREFS_KEY, JSON.stringify({ scalingMode: "contain", launchImmersive: false }));
    fsEl = document.documentElement;
    await reveal();
    expect(engageSpy).not.toHaveBeenCalled();
    expect(playLayer()).toBeNull();
    expect(lastDrawerProps?.releaseHint).toBeNull();
  });

  it("does not take the pointer while the loader is still up", async () => {
    fsEl = document.documentElement;
    currentSession = makeSession({ state: "running", state_detail: "app booting" });
    renderPage();
    await advance(1_000);
    await act(async () => {
      openChannel();
    });
    await advance(2_000);
    expect(document.querySelector(".sl-root")).not.toBeNull();
    expect(engageSpy).not.toHaveBeenCalled();
    expect(playLayer()).toBeNull();
  });

  it.each([
    [true, false, true],
    [false, true, false],
  ])("secure=%s: insecure wording %s, no-Keyboard-Lock wording %s (#631)", async (secure, insecure, noLock) => {
    Object.defineProperty(window, "isSecureContext", { value: secure, configurable: true });
    try {
      fsEl = document.documentElement;
      await reveal();
      expect(lastDrawerProps?.escInsecureContext).toBe(insecure);
      expect(lastDrawerProps?.escNoKeyboardLock).toBe(noLock);
    } finally {
      Reflect.deleteProperty(window, "isSecureContext");
    }
  });

  it("with the setting on, hands the HUD a release hint", async () => {
    fsEl = document.documentElement;
    await reveal();
    expect(typeof lastDrawerProps?.releaseHint).toBe("string");
    expect((lastDrawerProps?.releaseHint as string).length).toBeGreaterThan(0);
  });
});

describe("SessionPage — Match cursor compositor size (#898)", () => {
  const compositor = () => {
    const v = document.querySelector("video")!;
    return [v.dataset.compositorW, v.dataset.compositorH];
  };

  it("is the launch size, moves with the render size, and ignores a stream-size change", async () => {
    await reveal();
    // The launch stream is 2560x1440; the router-state tier (1920x1080) is only the seed.
    expect(compositor()).toEqual(["2560", "1440"]);

    // The external stream size moves the stream, not the compositor.
    await act(async () => {
      (lastDrawerProps?.onStreamSizeChange as (v: { w: number; h: number }) => void)({ w: 1920, h: 1080 });
    });
    await advance(500);
    expect(compositor()).toEqual(["2560", "1440"]);

    await act(async () => {
      (lastDrawerProps?.onRenderSizeChange as (v: { w: number; h: number }) => void)({ w: 1280, h: 720 });
    });
    await advance(500);
    expect(compositor()).toEqual(["1280", "720"]);
  });
});
