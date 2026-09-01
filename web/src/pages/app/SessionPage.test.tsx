// #484 §3.2 — the app-boot loader gate.
//
// SessionPage owns a lot (WebRTC, telemetry, quick-switch, mic, the drawer…):
// this test mocks every side channel down to what's needed to drive the
// loader-reveal state machine through fetch-driven `state_detail` polling,
// and stubs the heavy child components (SessionSwapController and the HUD —
// neither is what's under test here).
// SessionLoader itself is the REAL component: assertions read its actual
// `.sl-root` presence, the same DOM contract the design doc's devbox PASS
// criteria (§3.5) check on a live stack.
//
// Covers the four cases design §3.2 calls out:
//   1. loader stays mounted after channelOpen while detail is "app booting"
//   2. unmounts on "app presented"
//   3. unmounts on the reveal-cap timer (fail-open rule 2)
//   4. unmounts immediately when running arrives with no "app booting" at
//      all (fail-open rule 1 — an older agent, or no app container)

import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SessionPage, parseTierSize } from "./SessionPage";
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

vi.mock("../../auth/context", () => ({
  useAuth: () => ({ token: "t" }),
}));

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
    engage: () => Promise.resolve({ mode: "pointer-lock" }),
    release: () => {},
  }),
  keyboardLockSupported: () => false,
  // A desktop-shaped default: this suite exercises the display/resolution
  // controls, not the capture modality. The touch and no-Pointer-Lock paths
  // have their own coverage in capture.touch.test.ts / capture.test.ts.
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
    ) {
      lastOnChannel = onChannel;
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
// Captures the HUD's props so the display-PATCH debounce/revert can be driven
// through the real handlers without rendering the HUD (its own rendering is
// covered in hud/Hud.test.tsx). The strip and the drawer were two components
// with two prop sets; they are one now, so `lastStripProps` is the same object
// with the bar's badge input under the name the badge assertions use.
let lastStripProps: Record<string, unknown> | null = null;
let lastDrawerProps: Record<string, unknown> | null = null;
vi.mock("./hud/Hud", async () => {
  const { forwardRef } = await import("react");
  return {
    Hud: forwardRef((p: Record<string, unknown>, _ref: unknown) => {
      lastDrawerProps = p;
      lastStripProps = { ...p, externalSize: p.badgeExternalSize };
      return null;
    }),
  };
});

function openChannel() {
  lastOnChannel?.({ readyState: "open", onclose: null });
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

beforeEach(() => {
  // `restoreAllMocks` (afterEach) only restores `vi.spyOn` spies since vitest 3;
  // the module doubles above are plain `vi.fn()`s, so their call history now
  // survives into the next test unless it is cleared here. The PATCH-count
  // assertions below are per-test counts and read as cumulative without this.
  vi.clearAllMocks();
  lastOnChannel = null;
  lastDrawerProps = null;
  lastStripProps = null;
  updateSessionDisplay.mockResolvedValue({ session: makeSession({ state: "running" }) });
  currentSession = makeSession();
  getSession.mockImplementation(async () => ({ session: currentSession }));
  stopSession.mockResolvedValue(undefined);
  mintSignalingToken.mockResolvedValue({ signaling: { url: "wss://x", token: "y" } });
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("SessionPage — display updates (render resolution + interface size)", () => {
  /** Renders, settles the pollers, and returns the drawer's captured props. */
  async function mountAndSettle() {
    currentSession = makeSession({ state: "running", state_detail: "app presented" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    return () => lastDrawerProps as Record<string, unknown>;
  }

  type Size = { w: number; h: number };
  const drawer = <T,>(props: Record<string, unknown>, key: string) => props[key] as T;

  it("takes the stream size from the session poll, not the tier string", async () => {
    // Fixture stream is 2560×1440; the router-state tier says 1920×1080. The
    // poll wins — `session.stream` is the contract's truth for a running
    // session, the tier is a launch-time display string.
    const props = await mountAndSettle();
    expect(drawer<Size>(props(), "streamSize")).toEqual({ w: 2560, h: 1440 });
    // Null render size = "match the stream" (the session default), scale 1.
    expect(drawer<Size | null>(props(), "renderSize")).toBeNull();
    expect(drawer<number>(props(), "uiScale")).toBe(1);
  });

  it("seeds the stream size from the tier before the first poll answers", async () => {
    // The poll is authoritative but not instant. Until it answers, the
    // launch-time tier keeps the controls usable rather than hidden.
    getSession.mockImplementation(() => new Promise(() => {})); // never resolves
    currentSession = makeSession({ state: "running", state_detail: "app presented" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(drawer<Size>(lastDrawerProps as Record<string, unknown>, "streamSize")).toEqual({
      w: 1920,
      h: 1080,
    });
  });

  it("still yields a stream size when the session carries no tier at all", async () => {
    // A resumed / deep-linked session: no router state tier, so the tier parse
    // gives null and the poll is the ONLY source. This is the case the rows
    // used to hide for.
    expect(parseTierSize(undefined)).toBeNull();
    const props = await mountAndSettle();
    expect(drawer<Size>(props(), "streamSize")).toEqual({ w: 2560, h: 1440 });
  });

  it("keeps the health banner and timer working if a poll body lacks stream", async () => {
    // The whole poll tick sits in a best-effort catch: an absent `stream` must
    // skip only this one read, not take started_at down with it.
    const started = new Date().toISOString();
    const noStream = makeSession({ state: "running", started_at: started });
    delete (noStream as Record<string, unknown>).stream;
    currentSession = noStream;
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    const props = lastDrawerProps as Record<string, unknown>;
    expect(drawer<string | null>(props, "startedAt")).toBe(started);
    // Falls back to the tier seed rather than crashing or blanking.
    expect(drawer<Size>(props, "streamSize")).toEqual({ w: 1920, h: 1080 });
  });

  it("debounces the PATCH — nothing is sent before the window closes", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200);
    });
    expect(updateSessionDisplay).not.toHaveBeenCalled();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(200);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(1);
  });

  it("sends BOTH dims together (the contract is both-or-neither)", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledWith("t", "s1", {
      render_width: 1280,
      render_height: 720,
    });
  });

  it("coalesces a resolution and a scale change inside the window into one PATCH", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1280, h: 720 });
      drawer<(v: number) => void>(props(), "onUiScaleChange")(1.5);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(1);
    expect(updateSessionDisplay).toHaveBeenCalledWith("t", "s1", {
      render_width: 1280,
      render_height: 720,
      ui_scale: 1.5,
    });
  });

  it("sends ui_scale alone when only the scale moved", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: number) => void>(props(), "onUiScaleChange")(2);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledWith("t", "s1", { ui_scale: 2 });
  });

  it("keeps the new value once the server acks it", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size>(props(), "renderSize")).toEqual({ w: 1280, h: 720 });
    expect(drawer<boolean>(props(), "displayBusy")).toBe(false);
  });

  it("reverts to the last-acked value when the host rejects the change", async () => {
    const props = await mountAndSettle();
    // Ack one change first, so the revert target is a real previous value
    // rather than the initial default.
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1600, h: 900 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size>(props(), "renderSize")).toEqual({ w: 1600, h: 900 });

    updateSessionDisplay.mockRejectedValueOnce(
      new ApiError(409, "display_update_rejected", "host refused"),
    );
    act(() => {
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 960, h: 540 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size>(props(), "renderSize")).toEqual({ w: 1600, h: 900 });
  });

  it("reverts the scale to 1 when the very first change is rejected", async () => {
    const props = await mountAndSettle();
    updateSessionDisplay.mockRejectedValueOnce(
      new ApiError(400, "validation_failed", "out of range"),
    );
    act(() => {
      drawer<(v: number) => void>(props(), "onUiScaleChange")(3);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<number>(props(), "uiScale")).toBe(1);
    expect(drawer<boolean>(props(), "displayBusy")).toBe(false);
    // The render row is untouched by a scale rejection's revert: null is still
    // "match the stream", which is what it was.
    expect(drawer<Size | null>(props(), "renderSize")).toBeNull();
  });

  it("keeps at most one PATCH in flight, and does not lose the deferred change", async () => {
    const props = await mountAndSettle();
    // First request hangs, so the second change lands while it is in flight.
    let release: (v: unknown) => void = () => {};
    updateSessionDisplay.mockImplementationOnce(
      () => new Promise((res) => { release = res; }),
    );
    act(() => {
      drawer<(v: number) => void>(props(), "onUiScaleChange")(1.5);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(1);

    // Queue a second change while the first is still open — it must NOT go out.
    act(() => {
      drawer<(v: number) => void>(props(), "onUiScaleChange")(2);
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(1);

    // Settle the first; the deferred one goes out after its own window.
    await act(async () => {
      release({ session: currentSession });
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(2);
    expect(updateSessionDisplay).toHaveBeenLastCalledWith("t", "s1", { ui_scale: 2 });
    expect(drawer<number>(props(), "uiScale")).toBe(2);
  });

  it("resyncs displayed state from the acked value on success", async () => {
    const props = await mountAndSettle();
    act(() => {
      drawer<(v: number) => void>(props(), "onUiScaleChange")(1.75);
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    // Displayed state IS acked state — no drift possible.
    expect(drawer<number>(props(), "uiScale")).toBe(1.75);
    expect(drawer<Size>(props(), "renderSize")).toEqual({ w: 1280, h: 720 });
  });
});

// Adaptive external resolution (§D6). The stream lever rides the SAME
// debounce / single-in-flight / ack / revert machinery as the render one —
// these cover the parts that are specific to it: the merged body, the
// launch-vs-external split, and the permanent `external_resize_unsupported`
// rejection.
describe("SessionPage — stream (external) resolution", () => {
  type Size = { w: number; h: number };
  const drawer = <T,>(props: Record<string, unknown>, key: string) => props[key] as T;

  async function mountAndSettle(overrides: Record<string, unknown> = {}) {
    currentSession = makeSession({
      state: "running",
      state_detail: "app presented",
      ...overrides,
    });
    const view = renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    return {
      view,
      props: () => lastDrawerProps as Record<string, unknown>,
      strip: () => lastStripProps as Record<string, unknown>,
    };
  }

  it("starts at the launch size — externalSize is null until something changes", async () => {
    const { props, strip } = await mountAndSettle();
    expect(drawer<Size>(props(), "streamSize")).toEqual({ w: 2560, h: 1440 });
    expect(drawer<Size | null>(props(), "externalSize")).toBeNull();
    // Badge hidden: the strip's tier already states the launch size.
    expect(drawer<Size | null>(strip(), "externalSize")).toBeNull();
  });

  it("sends BOTH stream dims together in one PATCH", async () => {
    const { props } = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledWith("t", "s1", {
      stream_width: 1280,
      stream_height: 720,
    });
  });

  it("coalesces a stream change and a render change into one merged body", async () => {
    const { props } = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
      drawer<(v: Size) => void>(props(), "onRenderSizeChange")({ w: 960, h: 540 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(updateSessionDisplay).toHaveBeenCalledTimes(1);
    expect(updateSessionDisplay).toHaveBeenCalledWith("t", "s1", {
      stream_width: 1280,
      stream_height: 720,
      render_width: 960,
      render_height: 540,
    });
  });

  it("shows the badge once the acked external size differs from launch", async () => {
    const { props, strip } = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size | null>(strip(), "externalSize")).toEqual({ w: 1280, h: 720 });
    expect(drawer<boolean>(strip(), "streamAdapting")).toBe(false);
  });

  it("flags 'Adapting…' while the change is in flight, and clears it after", async () => {
    const { props, strip } = await mountAndSettle();
    let release: (v: unknown) => void = () => {};
    updateSessionDisplay.mockImplementationOnce(() => new Promise((res) => { release = res; }));
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    expect(drawer<boolean>(strip(), "streamAdapting")).toBe(true);
    // The DRAWER gets the same flag: its row is the only place the state is
    // readable while the drawer's scrim covers the strip.
    expect(drawer<boolean>(props(), "streamAdapting")).toBe(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<boolean>(strip(), "streamAdapting")).toBe(true);
    await act(async () => {
      release({ session: currentSession });
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(drawer<boolean>(strip(), "streamAdapting")).toBe(false);
  });

  it("hides the badge again when the user goes back to the launch size", async () => {
    const { props, strip } = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size | null>(strip(), "externalSize")).not.toBeNull();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 2560, h: 1440 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size | null>(strip(), "externalSize")).toBeNull();
  });

  it("reads the external size and rung list off the session poll", async () => {
    const { props } = await mountAndSettle({
      stream: {
        width: 2560,
        height: 1440,
        fps: 60,
        bitrate_kbps: 20000,
        external_width: 1600,
        external_height: 900,
        rungs: [
          [2560, 1440],
          [1600, 900],
        ],
        external_resize_supported: true,
        external_owner: "auto",
      },
    });
    // width/height stay the LAUNCH size; the external pair is what is encoded.
    expect(drawer<Size>(props(), "streamSize")).toEqual({ w: 2560, h: 1440 });
    expect(drawer<Size>(props(), "externalSize")).toEqual({ w: 1600, h: 900 });
    expect(drawer<[number, number][]>(props(), "streamRungs")).toEqual([
      [2560, 1440],
      [1600, 900],
    ]);
    // T6b: the "Auto · WxH" chip's data — sourced from the same session poll,
    // now that the control plane threads stream.external_owner onto the
    // session resource (quasar-protocol PR #15).
    expect(drawer<string | undefined>(props(), "externalOwner")).toBe("auto");
  });

  it("reports a pinned external size distinctly from an auto one", async () => {
    const { props } = await mountAndSettle({
      stream: {
        width: 2560,
        height: 1440,
        fps: 60,
        bitrate_kbps: 20000,
        external_width: 1600,
        external_height: 900,
        external_resize_supported: true,
        external_owner: "pinned",
      },
    });
    expect(drawer<string | undefined>(props(), "externalOwner")).toBe("pinned");
  });

  it("has no externalOwner when the poll carries none (pre-amendment control plane, or at launch)", async () => {
    const { props } = await mountAndSettle();
    expect(drawer<string | undefined>(props(), "externalOwner")).toBeUndefined();
  });

  it("clears externalOwner on the very next poll once the ladder releases back to launch", async () => {
    currentSession = makeSession({
      state: "running",
      state_detail: "app presented",
      stream: {
        width: 2560,
        height: 1440,
        fps: 60,
        bitrate_kbps: 20000,
        external_width: 1600,
        external_height: 900,
        external_resize_supported: true,
        external_owner: "auto",
      },
    });
    const view = renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(drawer<string | undefined>(lastDrawerProps as Record<string, unknown>, "externalOwner")).toBe(
      "auto",
    );

    // The next sample reports the session back at its launch size — the
    // control plane's cache clears external_owner in lockstep (it has no
    // meaning at the launch size), and this poll must follow suit rather
    // than holding the stale "auto" value.
    currentSession = { ...currentSession, stream: { width: 2560, height: 1440, fps: 60, bitrate_kbps: 20000 } };
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(drawer<string | undefined>(lastDrawerProps as Record<string, unknown>, "externalOwner")).toBeUndefined();
    view.unmount();
  });

  it("falls back to a client-side 16:9 ladder when the poll carries no rungs", async () => {
    // A control plane older than the amendment sends none at all.
    const { props } = await mountAndSettle();
    expect(drawer<[number, number][]>(props(), "streamRungs")).toEqual([
      [2560, 1440],
      [1920, 1080],
      [1600, 900],
      [1280, 720],
    ]);
  });

  it("reverts and explains a 409 external_resize_unsupported", async () => {
    const { props, view } = await mountAndSettle();
    updateSessionDisplay.mockRejectedValueOnce(
      new ApiError(409, "external_resize_unsupported", "encoder cannot resize"),
    );
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    // Back to the launch size (null), and the row latches inert so the user is
    // not invited to try again against a host that will never accept it.
    expect(drawer<Size | null>(props(), "externalSize")).toBeNull();
    expect(drawer<boolean>(props(), "externalResizeSupported")).toBe(false);
    expect(
      view.container.ownerDocument.body.textContent,
    ).toContain("This session's encoder can't change stream resolution live");
  });

  it("reverts to the last ACKED external size, not to launch, on a later rejection", async () => {
    const { props } = await mountAndSettle();
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1920, h: 1080 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    updateSessionDisplay.mockRejectedValueOnce(
      new ApiError(409, "display_update_rejected", "host refused"),
    );
    act(() => {
      drawer<(v: Size) => void>(props(), "onStreamSizeChange")({ w: 1280, h: 720 });
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    expect(drawer<Size | null>(props(), "externalSize")).toEqual({ w: 1920, h: 1080 });
  });

  it("passes the encoder's live-resize capability through to the drawer", async () => {
    const { props } = await mountAndSettle({
      stream: {
        width: 2560,
        height: 1440,
        fps: 60,
        bitrate_kbps: 20000,
        external_resize_supported: false,
      },
    });
    expect(drawer<boolean>(props(), "externalResizeSupported")).toBe(false);
  });
});

describe("parseTierSize", () => {
  it("parses the tier string the launch path builds", () => {
    expect(parseTierSize("1920×1080@60")).toEqual({ w: 1920, h: 1080 });
    expect(parseTierSize("3840×2160@120")).toEqual({ w: 3840, h: 2160 });
  });

  it("returns null for a missing or unparseable tier", () => {
    expect(parseTierSize(undefined)).toBeNull();
    expect(parseTierSize("")).toBeNull();
    expect(parseTierSize("1080p60")).toBeNull();
    // ASCII 'x' is not the separator the producer uses (U+00D7).
    expect(parseTierSize("1920x1080@60")).toBeNull();
  });

  it("tolerates a tier with no fps suffix", () => {
    expect(parseTierSize("1280×720")).toEqual({ w: 1280, h: 720 });
  });
});

describe("SessionPage — #484 app-boot loader gate", () => {
  it("stays mounted after channelOpen while state_detail is 'app booting'", async () => {
    currentSession = makeSession({ state: "running", state_detail: "app booting" });
    renderPage();

    // Let the launch poller's immediate + first-interval tick observe "app booting".
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await waitFor(() => expect(document.querySelector(".sl-root")).not.toBeNull());

    // Transport comes up — this alone must NOT reveal the stream.
    await act(async () => {
      openChannel();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });

    expect(document.querySelector(".sl-root")).not.toBeNull();
    // The loader's stage copy names the app-booting phase, not a stale
    // transport-negotiation stage.
    expect(document.querySelector(".sl-root")?.textContent).toMatch(/starting/i);
  });

  it("unmounts on 'app presented'", async () => {
    currentSession = makeSession({ state: "running", state_detail: "app booting" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await act(async () => {
      openChannel();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(document.querySelector(".sl-root")).not.toBeNull();

    // Agent reports the app has presented.
    currentSession = makeSession({ state: "running", state_detail: "app presented" });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    // Loader stays in the DOM through its lock->fade sequence before unmounting
    // (LOADER_UNMOUNT_MS).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });

    await waitFor(() => expect(document.querySelector(".sl-root")).toBeNull());
  });

  it("unmounts on the reveal-cap timer when 'app presented' never arrives", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    currentSession = makeSession({ state: "running", state_detail: "app booting" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await act(async () => {
      openChannel();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(document.querySelector(".sl-root")).not.toBeNull();

    // state_detail is stuck on "app booting" forever — advance past the
    // client-side QUASAR_APP_PRESENT_REVEAL_MAX_MS cap (180_000ms), plus the
    // loader's handoff (LOADER_UNMOUNT_MS).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(181_000);
    });
    // ...then the loader's own lock->fade sequence (LOADER_UNMOUNT_MS), which is
    // only armed once the cap has flipped the reveal.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });

    expect(warn).toHaveBeenCalledWith(expect.stringMatching(/reveal cap/i));
    await waitFor(() => expect(document.querySelector(".sl-root")).toBeNull());
  });

  it("unmounts immediately when running arrives with no 'app booting' (fail-open rule 1)", async () => {
    // An older agent (or a session with no app container): state_detail
    // never becomes the literal "app booting" string at all.
    currentSession = makeSession({ state: "running", state_detail: "pipeline live; offer ready" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await act(async () => {
      openChannel();
    });
    // Only the lock->fade delay should stand between channelOpen and unmount —
    // no extra app-boot wait was ever armed.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });

    await waitFor(() => expect(document.querySelector(".sl-root")).toBeNull());
  });
});

// Bench mode is a measurement instrument that reads back <video> pixels on every
// displayed frame. It must be provably inert for an ordinary session: no RVFC
// decode loop, and no `window.__qBench` for anything to drive.
describe("SessionPage — bench mode", () => {
  it("does not arm the bench instrument for an ordinary session", async () => {
    delete (window as unknown as Record<string, unknown>)["__qBench"];
    currentSession = makeSession({ state: "running", state_detail: "app presented" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await act(async () => {
      openChannel();
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });
    // No handle, and nothing scheduled a video-frame callback.
    expect((window as unknown as Record<string, unknown>)["__qBench"]).toBeUndefined();
  });
});

// The post-session summary. Its duration used to be `Date.now() - startedAt`
// where `startedAt` came from `performance.now()`, so the card reported the
// Unix epoch (29799808m 2s on the live capture).
describe("SessionPage — post-session summary", () => {
  it("reports the duration on one clock, with the two clocks far apart", async () => {
    vi.setSystemTime(new Date("2026-08-29T12:00:00Z"));
    // performance.now() is monotonic-since-page-load; Date.now() is ~1.79e12.
    const clock = vi.spyOn(performance, "now").mockReturnValue(1_234);
    currentSession = makeSession({ state: "running", state_detail: "app presented" });
    renderPage();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    await act(async () => {
      openChannel();
    });

    clock.mockReturnValue(1_234 + 42_000);
    await act(async () => {
      await (lastDrawerProps!.onStop as () => Promise<void>)();
    });

    const summary = JSON.parse(screen.getByTestId("summary").textContent ?? "null");
    expect(summary.durationSeconds).toBe(42);
    expect(summary.endReason).toBe("Stopped by user");
  });
});
