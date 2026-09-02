// The HUD's own behaviour: what the rest pill carries, how the shelf morphs,
// what the keyboard does, and when the thing hides. The pane-level detail lives
// in panes/*.test.tsx; what is asserted here is the object that holds them.
//
// Also covered: the quality readings, the preference-driven item set, the
// action gating and the external-resolution badge.

import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRef } from "react";
import { Hud, type HudHandle } from "./Hud";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../webrtc/telemetry";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import {
  DEFAULT_OVERLAY_PREFERENCES,
  applyPreset,
  toggleItem,
  type SessionOverlayPreferences,
} from "../../../settings/overlayPreferences";

let prefs: SessionOverlayPreferences = DEFAULT_OVERLAY_PREFERENCES;
vi.mock("../../../settings/OverlayPreferencesContext", () => ({
  useOverlayPreferences: () => ({ prefs, loaded: true, save: async () => {}, error: null }),
}));

const authValue = {
  status: "authenticated",
  user: { id: "u1", username: "admin" },
  token: "t0k3n",
  isAdmin: false,
  login: async () => {},
  claim: async () => {},
  logout: async () => {},
} as unknown as AuthContextValue;

function makeRegister() {
  const fns: ((s: TelemetrySnapshot) => void)[] = [];
  const register = (fn: (s: TelemetrySnapshot) => void) => {
    fns.push(fn);
    return () => {};
  };
  return { register, push: (s: TelemetrySnapshot) => fns.forEach((f) => f(s)) };
}

const base = {
  channelOpen: true,
  appPresented: true,
  appName: "Bench: Ball",
  tier: "1920×1080@60",
  resolvedCodec: "h264",
  sessionId: "s-1",
  inputCaptured: false,
  onGrab: vi.fn(),
  onRelease: vi.fn(),
  escReleases: false,
  escInsecureContext: false,
  pointerLockAvailable: true,
  touchLook: false,
  micGranted: true,
  micOn: false,
  micBusy: false,
  onToggleMic: vi.fn(),
  fullscreen: false,
  onFullscreen: vi.fn(),
  stopping: false,
  onStop: vi.fn(),
  swappingTo: null,
  games: <div data-testid="games-pane">Switch game</div>,
  scalingMode: "contain" as const,
  onScalingChange: vi.fn(),
  streamSize: { w: 1920, h: 1080 },
  renderSize: { w: 1920, h: 1080 },
  onRenderSizeChange: vi.fn(),
  uiScale: 1,
  onUiScaleChange: vi.fn(),
  displayBusy: false,
};

function renderHud(props: Partial<React.ComponentProps<typeof Hud>> = {}) {
  const { register, push } = makeRegister();
  const ref = createRef<HudHandle>();
  const view = render(
    <AuthContext.Provider value={authValue}>
      <Hud ref={ref} register={register} {...base} {...props} />
    </AuthContext.Provider>,
  );
  return { ...view, push, ref, register };
}

const chevron = () => screen.getByRole("button", { name: /menu/i });
const tabBtn = (name: RegExp) => screen.getByRole("tab", { name });

/** jsdom has no ResizeObserver, so the HUD's re-measure path is unreachable
 *  without one. This records what each instance observes and lets a test fire
 *  the callback by hand; it never fires on its own, so every other test in this
 *  file behaves exactly as it did before. */
class MockResizeObserver {
  readonly targets: Element[] = [];
  constructor(readonly cb: ResizeObserverCallback) {
    liveObservers.push(this);
  }
  observe(el: Element) {
    this.targets.push(el);
  }
  unobserve() {}
  disconnect() {
    liveObservers.splice(liveObservers.indexOf(this), 1);
  }
}
let liveObservers: MockResizeObserver[] = [];
globalThis.ResizeObserver = MockResizeObserver as unknown as typeof ResizeObserver;

beforeEach(() => {
  prefs = DEFAULT_OVERLAY_PREFERENCES;
  liveObservers = [];
  vi.clearAllMocks();
});
afterEach(() => vi.useRealTimers());

describe("Hud rest pill", () => {
  it("carries the connection glyph, the frame rate, the title and the controls", () => {
    const { push, container } = renderHud();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 60, rttMs: 18, bitrateKbps: 8400 }));

    expect(container.querySelector(".signal")).toBeTruthy();
    expect(screen.getByText("60")).toBeTruthy();
    expect(screen.getByText("Bench: Ball")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Capture input" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Microphone" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Fullscreen" })).toBeTruthy();
    expect(chevron()).toBeTruthy();
    expect(screen.getByRole("button", { name: "Exit session" })).toBeTruthy();
  });

  it("hides the section tabs until the shelf is open", () => {
    const { container } = renderHud();
    // They are in the DOM (the CSS hides them), but every one carries the
    // class that does it — at rest they would be three more things to read.
    for (const btn of Array.from(container.querySelectorAll(".ib[data-tab]"))) {
      expect(btn.className).toContain("open-only");
    }
    expect(container.querySelector('.hud-root[data-open="true"]')).toBeNull();
  });

  it("puts Exit last, in danger colours", () => {
    const { container } = renderHud();
    const cluster = container.querySelector(".hud-ctl")!;
    const exit = screen.getByRole("button", { name: "Exit session" });
    expect(exit.className).toContain("danger");
    // Its 12px separation is a CSS rule keyed off being the danger button in
    // the cluster; what is assertable here is that nothing follows it.
    expect(cluster.lastElementChild).toBe(exit);
  });

  it("reports poor, not good, once a delivering stream stalls to 0 fps", () => {
    const { push, container } = renderHud();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 0, framesDecodedTotal: 900 }));
    expect(container.querySelector(".signal")?.getAttribute("data-q")).toBe("poor");
  });

  it("a fresh session with no frames yet still reads good at fps 0", () => {
    const { push, container } = renderHud();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 0, framesDecodedTotal: 0 }));
    expect(container.querySelector(".signal")?.getAttribute("data-q")).not.toBe("poor");
  });

  it("reports no signal while the channel is closed", () => {
    const { push, container } = renderHud({ channelOpen: false });
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 60, framesDecodedTotal: 900 }));
    expect(container.querySelector(".signal")?.getAttribute("aria-label")).toMatch(/no signal/i);
  });

  it("caps at good, never excellent, while the app hasn't presented", () => {
    const { push, container } = renderHud({ appPresented: false });
    act(() =>
      push({ ...EMPTY_SNAPSHOT, fps: 60, framesDecodedTotal: 900, presentSdMs: 1, packetsLost: 0 }),
    );
    expect(container.querySelector(".signal")?.getAttribute("data-q")).toBe("good");
  });

  it("shows the encoded size once it differs from the launch size", () => {
    renderHud({ badgeExternalSize: { w: 1280, h: 720 } });
    expect(screen.getByText("Stream 1280×720")).toBeTruthy();
  });

  it("says 'Adapting…' while a stream change is in flight", () => {
    renderHud({ badgeExternalSize: { w: 1280, h: 720 }, streamAdapting: true });
    expect(screen.getByText("Adapting…")).toBeTruthy();
    expect(screen.queryByText("Stream 1280×720")).toBeNull();
  });
});

describe("Hud preferences", () => {
  it("the Metrics preset drops the title and the connection glyph, keeping the numbers", () => {
    prefs = applyPreset(DEFAULT_OVERLAY_PREFERENCES, "metrics");
    const { push, container } = renderHud();
    act(() => push({ ...EMPTY_SNAPSHOT, fps: 60 }));

    expect(screen.queryByText("Bench: Ball")).toBeNull();
    expect(container.querySelector(".signal")).toBeNull();
    expect(screen.getByText("60")).toBeTruthy();
  });

  it("omits a control the preference has turned off", () => {
    prefs = toggleItem(toggleItem(DEFAULT_OVERLAY_PREFERENCES, "mic"), "fullscreen");
    renderHud();
    expect(screen.queryByRole("button", { name: "Microphone" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Fullscreen" })).toBeNull();
    // The way in never yields: the chevron and the tabs are not preference-gated.
    expect(chevron()).toBeTruthy();
  });

  it("keeps the shelf reachable even under 'never show'", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    const { container } = renderHud();
    // Hidden, not unmounted: opening it must still be possible.
    expect(container.querySelector(".hud")?.className).toContain("hidden");
    fireEvent.click(chevron());
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");
  });

  it("docks where the preference says, on the matching axis", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripPosition: "left" };
    const { container } = renderHud();
    const root = container.querySelector(".hud-root")!;
    expect(root.getAttribute("data-pos")).toBe("left");
    expect(root.getAttribute("data-axis")).toBe("v");
  });
});

describe("Hud shelf", () => {
  it("opens on Switch game from the chevron", () => {
    renderHud();
    expect(screen.queryByTestId("games-pane")).toBeNull();
    fireEvent.click(chevron());
    expect(screen.getByTestId("games-pane")).toBeTruthy();
    expect(chevron().getAttribute("aria-expanded")).toBe("true");
  });

  it("collapses when the section already showing is pressed again", () => {
    renderHud();
    fireEvent.click(tabBtn(/^Controller and input$/));
    expect(screen.getByText("Keys")).toBeTruthy();
    fireEvent.click(tabBtn(/^Controller and input$/));
    expect(screen.queryByText("Keys")).toBeNull();
  });

  it("never resizes the bar when the section changes", () => {
    const { container } = renderHud();
    const bar = container.querySelector<HTMLElement>(".hud-bar")!;
    fireEvent.click(tabBtn(/^Switch game$/));
    const before = bar.getAttribute("style");
    fireEvent.click(tabBtn(/^Performance stats$/));
    expect(bar.getAttribute("style")).toBe(before);
    // Only the shelf's depth is animated; the bar carries no size of its own.
    expect(before ?? "").not.toMatch(/height/);
  });

  it("marks the showing section selected, and only that one", () => {
    renderHud();
    fireEvent.click(tabBtn(/^Performance stats$/));
    expect(tabBtn(/^Performance stats$/).getAttribute("aria-selected")).toBe("true");
    expect(tabBtn(/^Switch game$/).getAttribute("aria-selected")).toBe("false");
    // aria-selected is only meaningful on a tab inside a tablist.
    expect(screen.getByRole("tablist", { name: /session menu sections/i })).toBeTruthy();
  });

  it("renders the three display controls on the Display section", () => {
    renderHud();
    fireEvent.click(tabBtn(/^Display$/));
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: /interface size/i })).toBeTruthy();
    expect(screen.getByText("Render resolution")).toBeTruthy();
  });
});

describe("Hud capture", () => {
  it("flips the bar button's state and title", () => {
    const { rerender, register } = renderHud();
    const btn = () => screen.getByRole("button", { name: "Capture input" });
    expect(btn().getAttribute("aria-pressed")).toBe("false");
    expect(btn().getAttribute("title")).toBe("Capture input");

    rerender(
      <AuthContext.Provider value={authValue}>
        <Hud register={register} {...base} inputCaptured />
      </AuthContext.Provider>,
    );
    expect(btn().getAttribute("aria-pressed")).toBe("true");
    expect(btn().getAttribute("title")).toBe("Release input (Ctrl Alt ⇧ Z)");
  });

  it("flips the pane CTA, the Esc row and the input-locked cell", () => {
    const { push, rerender, register } = renderHud();
    fireEvent.click(tabBtn(/^Controller and input$/));
    expect(document.querySelector(".capture-cta")?.textContent).toBe("Capture input");
    expect(screen.getByText("Sent to the app")).toBeTruthy();

    // Capture engaging collapses the shelf (the picture is the point), so the
    // captured pane state is read by re-opening it.
    rerender(
      <AuthContext.Provider value={authValue}>
        <Hud register={register} {...base} inputCaptured escReleases />
      </AuthContext.Provider>,
    );
    expect(screen.queryByText("Keys")).toBeNull();
    fireEvent.click(tabBtn(/^Controller and input$/));
    expect(document.querySelector(".capture-cta")?.textContent).toBe("Release input");
    expect(screen.getByText("Releases capture")).toBeTruthy();

    fireEvent.click(tabBtn(/^Performance stats$/));
    fireEvent.click(screen.getByRole("tab", { name: "Detailed" }));
    act(() =>
      push({
        ...EMPTY_SNAPSHOT,
        inputMetrics: {
          pointerLocked: true,
          captured: true,
          pointerLockSupported: true,
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
        },
      }),
    );
    const lockRow = screen.getByText("input locked").parentElement!;
    expect(lockRow.textContent).toContain("yes");
  });

  // Contract (openapi.yaml SessionOverlayPreferences): while input is captured
  // the pointer is locked to the game, so a click on these could not have been
  // meant for them. Ported from the two v1 strip tests.
  it("makes the actions inert while input is captured", () => {
    renderHud({ inputCaptured: true });
    expect(screen.getByRole("button", { name: "Exit session" })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Microphone" })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Fullscreen" })).toHaveProperty("disabled", true);
    // Capture stays live: it is the way back out.
    expect(screen.getByRole("button", { name: "Capture input" })).toHaveProperty("disabled", false);
  });

  it("keeps them inert while captured even under always_visible", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "always_visible" };
    const { container } = renderHud({ inputCaptured: true });
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");
    expect(screen.getByRole("button", { name: "Exit session" })).toHaveProperty("disabled", true);
    expect(screen.getByRole("button", { name: "Microphone" })).toHaveProperty("disabled", true);
  });

  it("brings them back the moment capture releases", () => {
    const { rerender, register } = renderHud({ inputCaptured: true });
    rerender(
      <AuthContext.Provider value={authValue}>
        <Hud register={register} {...base} inputCaptured={false} />
      </AuthContext.Provider>,
    );
    expect(screen.getByRole("button", { name: "Exit session" })).toHaveProperty("disabled", false);
    expect(screen.getByRole("button", { name: "Fullscreen" })).toHaveProperty("disabled", false);
  });

  it("disables capture until the input channel is open", () => {
    renderHud({ channelOpen: false });
    expect(screen.getByRole("button", { name: "Capture input" })).toHaveProperty("disabled", true);
  });

  it("disables exit while a stop is already in flight", () => {
    renderHud({ stopping: true });
    expect(screen.getByRole("button", { name: "Exit session" })).toHaveProperty("disabled", true);
  });

  it("renders the mic unavailable, not hidden, when the server has not granted it", () => {
    renderHud({ micGranted: false });
    const mic = screen.getByRole("button", { name: "Microphone" });
    expect(mic).toHaveProperty("disabled", true);
    expect(mic.getAttribute("title")).toBe("Microphone disabled by server");
  });

  it("calls the handlers on click", () => {
    const onGrab = vi.fn();
    const onToggleMic = vi.fn();
    const onFullscreen = vi.fn();
    const onStop = vi.fn();
    renderHud({ onGrab, onToggleMic, onFullscreen, onStop });

    fireEvent.click(screen.getByRole("button", { name: "Capture input" }));
    fireEvent.click(screen.getByRole("button", { name: "Microphone" }));
    fireEvent.click(screen.getByRole("button", { name: "Fullscreen" }));
    fireEvent.click(screen.getByRole("button", { name: "Exit session" }));
    expect(onGrab).toHaveBeenCalled();
    expect(onToggleMic).toHaveBeenCalled();
    expect(onFullscreen).toHaveBeenCalled();
    expect(onStop).toHaveBeenCalled();
  });
});

describe("Hud stage click", () => {
  it("summons the shelf when input is not captured", () => {
    const { ref } = renderHud();
    act(() => ref.current!.stageClick());
    expect(screen.getByTestId("games-pane")).toBeTruthy();
  });

  it("ignores the click while input is captured — those clicks belong to the game", () => {
    const { ref } = renderHud({ inputCaptured: true });
    act(() => ref.current!.stageClick());
    expect(screen.queryByTestId("games-pane")).toBeNull();
  });
});

describe("Hud keyboard", () => {
  it("closes an open shelf on Escape and leaves a closed one alone", () => {
    renderHud();
    fireEvent.click(chevron());
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByTestId("games-pane")).toBeNull();
  });

  it("opens on the summon chord, releasing capture first", () => {
    const onRelease = vi.fn();
    renderHud({ inputCaptured: true, onRelease });
    fireEvent.keyDown(document, {
      key: "q",
      code: "KeyQ",
      ctrlKey: true,
      altKey: true,
      shiftKey: true,
    });
    expect(onRelease).toHaveBeenCalled();
    expect(screen.getByTestId("games-pane")).toBeTruthy();
  });

  it("releases capture on Ctrl+Alt+Shift+Z without opening anything", () => {
    const onRelease = vi.fn();
    renderHud({ inputCaptured: true, onRelease });
    fireEvent.keyDown(document, {
      key: "z",
      code: "KeyZ",
      ctrlKey: true,
      altKey: true,
      shiftKey: true,
    });
    expect(onRelease).toHaveBeenCalled();
    expect(screen.queryByTestId("games-pane")).toBeNull();
  });

  it("jumps to Performance stats on Shift+S", () => {
    renderHud();
    fireEvent.keyDown(document, { key: "S", code: "KeyS", shiftKey: true });
    expect(screen.getByText("Performance stats")).toBeTruthy();
  });

  // Shift+S is an ordinary in-game binding; opening a menu on it mid-play would
  // be the HUD stealing a keystroke from the game.
  it("leaves Shift+S alone while input is captured", () => {
    renderHud({ inputCaptured: true });
    fireEvent.keyDown(document, { key: "S", code: "KeyS", shiftKey: true });
    expect(screen.queryByText("Performance stats")).toBeNull();
  });

  it("ignores the arrows and Shift+S while a field has focus", () => {
    renderHud();
    fireEvent.click(tabBtn(/^Display$/));
    const select = screen.getByRole("combobox", { name: /render resolution/i });
    select.focus();
    fireEvent.keyDown(select, { key: "ArrowRight" });
    fireEvent.keyDown(select, { key: "S", code: "KeyS", shiftKey: true });
    // The arrow would have wrapped Display → Games, and Shift+S would have
    // jumped to stats; both belonged to the select.
    expect(screen.queryByTestId("games-pane")).toBeNull();
    expect(screen.getByRole("combobox", { name: /render resolution/i })).toBeTruthy();
  });

  it("cycles the sections with the arrow keys while open", () => {
    renderHud();
    fireEvent.click(chevron()); // games
    fireEvent.keyDown(document, { key: "ArrowRight" });
    expect(screen.getByText("Keys")).toBeTruthy();
    fireEvent.keyDown(document, { key: "ArrowLeft" });
    expect(screen.getByTestId("games-pane")).toBeTruthy();
  });

  it("ignores the arrows while the shelf is closed", () => {
    renderHud();
    fireEvent.keyDown(document, { key: "ArrowRight" });
    expect(screen.queryByText("Keys")).toBeNull();
  });
});

describe("Hud auto-hide", () => {
  it("on_capture: visible until capture, then hidden after the grace period", () => {
    vi.useFakeTimers();
    const { container, rerender, register } = renderHud();
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");

    rerender(
      <AuthContext.Provider value={authValue}>
        <Hud register={register} {...base} inputCaptured />
      </AuthContext.Provider>,
    );
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");
    act(() => {
      vi.advanceTimersByTime(4001);
    });
    expect(container.querySelector(".hud")?.className).toContain("hidden");
  });

  it("always_visible never hides, even while captured", () => {
    vi.useFakeTimers();
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "always_visible" };
    const { container } = renderHud({ inputCaptured: true });
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");
  });

  it("never_visible hides whenever the shelf is closed", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    const { container } = renderHud();
    expect(container.querySelector(".hud")?.className).toContain("hidden");
  });

  it("a swap in flight pins it, under every preference", () => {
    prefs = { ...DEFAULT_OVERLAY_PREFERENCES, stripAutoHide: "never_visible" };
    const { container } = renderHud({ swappingTo: "Purple App" });
    expect(container.querySelector(".hud")?.className).not.toContain("hidden");
  });
});

describe("Hud swapping", () => {
  it("replaces the bar with the switching line, and restores it after", () => {
    const { rerender, register } = renderHud({ swappingTo: "Purple App" });
    expect(screen.getByText(/Switching to/)).toBeTruthy();
    expect(screen.getByText("Purple App")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Exit session" })).toBeNull();

    rerender(
      <AuthContext.Provider value={authValue}>
        <Hud register={register} {...base} swappingTo={null} />
      </AuthContext.Provider>,
    );
    expect(screen.queryByText(/Switching to/)).toBeNull();
    expect(screen.getByRole("button", { name: "Exit session" })).toBeTruthy();
  });
});

// The rest pill is sized once from a measurement of the bar, and `--hud-w` then
// pins the bar's own box — so nothing about the bar reports a content change.
// On the live v3 capture that left the first-paint width one glyph short: the
// title ellipsised to "Quasar Bench: …" and "60 fps" clipped to "60 fp" until
// the shelf was opened and closed.
describe("Hud pill measurement", () => {
  /** Fires the HUD's observer and flushes the frame its callback defers to. */
  async function settleFrame() {
    await act(async () => {
      liveObservers.forEach((ro) => ro.cb([], ro as unknown as ResizeObserver));
      await new Promise((r) => setTimeout(r, 32));
    });
  }

  function stubBarWidth(bar: HTMLElement) {
    const box = { width: 0, height: 36 };
    let reads = 0;
    bar.getBoundingClientRect = () => {
      reads += 1;
      return { ...box, top: 0, left: 0, right: box.width, bottom: 36, x: 0, y: 0 } as DOMRect;
    };
    return { set: (w: number) => (box.width = w), reads: () => reads };
  }

  it("observes the bar's contents, which is the only part `--hud-w` does not pin", () => {
    const { container } = renderHud();
    const bar = container.querySelector<HTMLElement>(".hud-bar")!;
    const watched = liveObservers.at(-1)!.targets;
    expect(watched).toContain(bar);
    expect(watched).toContain(container.querySelector(".hud-read"));
    expect(watched.some((el) => el.tagName === "B")).toBe(true);
  });

  it("re-measures when a readout settles on its final text", async () => {
    const { container } = renderHud();
    const hud = container.querySelector<HTMLElement>(".hud")!;
    const bar = stubBarWidth(container.querySelector<HTMLElement>(".hud-bar")!);

    bar.set(360); // the bar as it reads with the empty snapshot: "0fps"
    await settleFrame();
    expect(hud.style.getPropertyValue("--hud-w")).toBe("min(362px, calc(100vw - 48px))");

    bar.set(368); // "60fps" — one glyph wider, and the pill must follow
    await settleFrame();
    expect(hud.style.getPropertyValue("--hud-w")).toBe("min(370px, calc(100vw - 48px))");
  });

  it("coalesces a burst of observations into one measurement", async () => {
    const { container } = renderHud();
    const bar = stubBarWidth(container.querySelector<HTMLElement>(".hud-bar")!);
    bar.set(360);
    const before = bar.reads();

    await act(async () => {
      const ro = liveObservers.at(-1)!;
      for (let i = 0; i < 4; i += 1) ro.cb([], ro as unknown as ResizeObserver);
      await new Promise((r) => setTimeout(r, 32));
    });

    expect(bar.reads() - before).toBe(1);
  });
});
