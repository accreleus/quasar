// AS10-13 — unit tests for CaptureMetrics counters and rate-window accounting.
//
// jsdom environment notes:
// - PointerEvent is not available in jsdom; it is stubbed globally in beforeEach.
// - navigator.getGamepads: absent in jsdom; defined per-test via Object.defineProperty.
// - RTCDataChannel: absent in jsdom; built as a plain mock object.
// - requestAnimationFrame: stubbed to a no-op in beforeEach (prevents gamepad
//   rAF loop from firing automatically; tests invoke it manually).
// - performance.now(): fake timers must include 'performance' so that
//   vi.advanceTimersByTime also advances performance.now() — the rate-window
//   denominator. Use vi.useFakeTimers({ toFake: ['performance'] }) in each
//   test that checks rate values.
//
// High-rate (500/1000 Hz) validation:
//   Real 500/1000 Hz mouse input cannot be reproduced in jsdom (no real vsync,
//   no hardware). Tests below validate the rate-accounting logic deterministically
//   by dispatching N synthetic events then advancing fake time by T seconds and
//   asserting rate ≈ N/T. A true end-to-end harness would require Playwright +
//   CDP synthetic-device injection — that is documented in the PR as a
//   real-browser harness concern outside unit-test scope.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setupCapture, keyboardLockSupported, pointerLockSupported } from "./capture";
import { EVDEV, MB_MAP } from "./evdev";

// ── Helpers ───────────────────────────────────────────────────────────────────

function makeChannel(bufferedAmount = 0): RTCDataChannel {
  return { bufferedAmount, readyState: "open" } as unknown as RTCDataChannel;
}

function makeVideo(): HTMLVideoElement {
  return {} as HTMLVideoElement;
}

/** Dispatch a synthetic pointermove on document with optional coalesced list. */
function dispatchPointerMove(
  movementX: number,
  movementY: number,
  coalesced?: Array<{ movementX: number; movementY: number }>,
) {
  const e = new PointerEvent("pointermove", { bubbles: true, movementX, movementY });
  if (coalesced) {
    (e as unknown as { getCoalescedEvents: () => unknown[] }).getCoalescedEvents =
      () => coalesced.map((d) => ({ movementX: d.movementX, movementY: d.movementY }));
  }
  document.dispatchEvent(e);
}

/** Simulate pointer lock by setting pointerLockElement and firing the change event. */
function lockPointer(video: HTMLVideoElement) {
  Object.defineProperty(document, "pointerLockElement", { value: video, configurable: true });
  document.dispatchEvent(new Event("pointerlockchange"));
}

function unlockPointer() {
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
  document.dispatchEvent(new Event("pointerlockchange"));
}

/** Dispatch a synthetic keydown on document. */
function dispatchKeyDown(
  code: string,
  mods: { ctrlKey?: boolean; altKey?: boolean; shiftKey?: boolean } = {},
) {
  document.dispatchEvent(
    new KeyboardEvent("keydown", { bubbles: true, code, ...mods }),
  );
}

/** Dispatch a synthetic keyup on document. */
function dispatchKeyUp(code: string) {
  document.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, code }));
}

// ── Global stubs ─────────────────────────────────────────────────────────────

beforeEach(() => {
  // jsdom does not define PointerEvent. Provide a minimal stub with
  // movementX/movementY support so the capture listener can read them.
  if (typeof PointerEvent === "undefined") {
    class FakePointerEvent extends Event {
      movementX: number;
      movementY: number;
      constructor(
        type: string,
        init: EventInit & { movementX?: number; movementY?: number } = {},
      ) {
        super(type, init);
        this.movementX = init.movementX ?? 0;
        this.movementY = init.movementY ?? 0;
      }
      getCoalescedEvents() { return []; }
    }
    vi.stubGlobal("PointerEvent", FakePointerEvent);
  }

  // Prevent the gamepad rAF loop from auto-firing; tests invoke rafCb manually.
  vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
  vi.stubGlobal("cancelAnimationFrame", vi.fn());
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  delete (navigator as unknown as { getGamepads?: unknown }).getGamepads;
  delete (navigator as unknown as { keyboard?: unknown }).keyboard;
  try {
    Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
  } catch { /* ignore */ }
});

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("keyboardLockSupported — secure-context feature detection (quasar#376)", () => {
  it("returns false when navigator.keyboard is absent (insecure context / non-Chromium)", () => {
    delete (navigator as unknown as { keyboard?: unknown }).keyboard;
    expect(keyboardLockSupported()).toBe(false);
  });

  it("returns false when navigator.keyboard exists but lacks lock/unlock", () => {
    Object.defineProperty(navigator, "keyboard", {
      value: {},
      configurable: true,
    });
    expect(keyboardLockSupported()).toBe(false);
  });

  it("returns true when navigator.keyboard.lock/unlock are both present (secure context, Chromium)", () => {
    Object.defineProperty(navigator, "keyboard", {
      value: { lock: vi.fn(), unlock: vi.fn() },
      configurable: true,
    });
    expect(keyboardLockSupported()).toBe(true);
  });
});

describe("Keyboard Lock refusal is surfaced, once (bypassed-cert follow-up)", () => {
  it("fires onKeyboardLockRefused on the FIRST lock() rejection only, and resets for retry", async () => {
    const video = makeVideo();
    const lock = vi.fn(() => Promise.reject(new DOMException("denied", "SecurityError")));
    const unlock = vi.fn();
    Object.defineProperty(navigator, "keyboard", {
      value: { lock, unlock },
      configurable: true,
    });
    const onRefused = vi.fn();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const { cleanup, syncKeyboardLock } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
      isFullscreen: () => true,
      onKeyboardLockRefused: onRefused,
    });

    lockPointer(video); // captured + fullscreen → wants Keyboard Lock
    await Promise.resolve();
    await Promise.resolve();
    expect(lock).toHaveBeenCalledTimes(1);
    expect(onRefused).toHaveBeenCalledTimes(1);
    expect(warn).toHaveBeenCalled();

    // The refusal reset keyboardLocked, so a later sync retries — but the
    // callback stays one-shot (it drives a toast; a retry loop must not spam).
    syncKeyboardLock();
    await Promise.resolve();
    await Promise.resolve();
    expect(lock).toHaveBeenCalledTimes(2);
    expect(onRefused).toHaveBeenCalledTimes(1);
    cleanup();
  });

  it("does not fire when lock() resolves", async () => {
    const video = makeVideo();
    Object.defineProperty(navigator, "keyboard", {
      value: { lock: vi.fn(() => Promise.resolve()), unlock: vi.fn() },
      configurable: true,
    });
    const onRefused = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
      isFullscreen: () => true,
      onKeyboardLockRefused: onRefused,
    });
    lockPointer(video);
    await Promise.resolve();
    await Promise.resolve();
    expect(onRefused).not.toHaveBeenCalled();
    cleanup();
  });
});

describe("CaptureMetrics — pointer lock state", () => {
  it("reports pointerLocked=false before lock is acquired", () => {
    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    expect(getMetrics().pointerLocked).toBe(false);
    cleanup();
  });

  it("reports pointerLocked=true after pointerlockchange with matching element", () => {
    const video = makeVideo();
    const onLock = vi.fn();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: onLock,
      channel: makeChannel(),
    });

    lockPointer(video);
    expect(getMetrics().pointerLocked).toBe(true);
    // Pointer Lock DRIVES capture where the API exists — both flip together.
    expect(onLock).toHaveBeenCalledWith({ captured: true, pointerLocked: true });

    unlockPointer();
    expect(getMetrics().pointerLocked).toBe(false);
    expect(onLock).toHaveBeenLastCalledWith({ captured: false, pointerLocked: false });

    cleanup();
  });
});

describe("CaptureMetrics — coalesced pointer event support detection", () => {
  it("reports coalescedSupported=true when getCoalescedEvents exists on PointerEvent", () => {
    // The FakePointerEvent stubbed in beforeEach includes getCoalescedEvents.
    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    expect(getMetrics().coalescedSupported).toBe(true);
    cleanup();
  });

  it("reports coalescedSupported=false when getCoalescedEvents is absent", () => {
    class NoCoalesceEvent extends Event {
      movementX = 0;
      movementY = 0;
      constructor(type: string, init: EventInit & { movementX?: number; movementY?: number } = {}) {
        super(type, init);
        this.movementX = init.movementX ?? 0;
        this.movementY = init.movementY ?? 0;
      }
      // no getCoalescedEvents
    }
    vi.stubGlobal("PointerEvent", NoCoalesceEvent);

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    expect(getMetrics().coalescedSupported).toBe(false);
    cleanup();
  });
});

describe("CaptureMetrics — message send rate", () => {
  it("counts input messages sent while pointer is locked", () => {
    // toFake: ['performance'] makes performance.now() advance with fake timers.
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    for (let i = 0; i < 5; i++) dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);

    const m = getMetrics();
    expect(m.inputMsgPerSec).toBeCloseTo(5, 0);
    expect(send).toHaveBeenCalledTimes(5);

    cleanup();
    unlockPointer();
  });

  it("does not count events when pointer is not locked", () => {
    const send = vi.fn();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    dispatchPointerMove(5, 5);

    const m = getMetrics();
    expect(m.inputMsgPerSec).toBe(0);
    expect(send).not.toHaveBeenCalled();

    cleanup();
  });

  it("resets rate window after getMetrics() is called", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);

    for (let i = 0; i < 3; i++) dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);
    expect(getMetrics().inputMsgPerSec).toBeCloseTo(3, 0);

    // No events in next window.
    vi.advanceTimersByTime(1000);
    expect(getMetrics().inputMsgPerSec).toBe(0);

    cleanup();
    unlockPointer();
  });

  it("second getMetrics() reflects only the second window, not accumulated totals", () => {
    // Guards the window-advance contract: each call resets counters so the next
    // call returns only events that occurred after the previous call.
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);

    // First window: 10 events over 1 s → ~10/s.
    for (let i = 0; i < 10; i++) dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);
    const first = getMetrics();
    expect(first.inputMsgPerSec).toBeCloseTo(10, 0);

    // Second window: 3 events over 1 s → ~3/s (not 13/s).
    for (let i = 0; i < 3; i++) dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);
    const second = getMetrics();
    expect(second.inputMsgPerSec).toBeCloseTo(3, 0);

    cleanup();
    unlockPointer();
  });
});

describe("CaptureMetrics — coalesced sample counting", () => {
  it("counts coalesced sub-events separately from plain sends", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);

    // 1 pointer-move carrying 3 coalesced sub-events.
    dispatchPointerMove(1, 0, [
      { movementX: 1, movementY: 0 },
      { movementX: 2, movementY: 0 },
      { movementX: 3, movementY: 0 },
    ]);

    vi.advanceTimersByTime(1000);
    const m = getMetrics();

    expect(send).toHaveBeenCalledTimes(3);
    expect(m.coalescedSamplesPerSec).toBeCloseTo(3, 0);

    cleanup();
    unlockPointer();
  });

  it("does not count coalesced samples for plain events (getCoalescedEvents returns [])", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    // FakePointerEvent.getCoalescedEvents returns [] → falls through to summary event.
    dispatchPointerMove(1, 0);

    vi.advanceTimersByTime(1000);
    // coalescedCount stays 0 because the coalesced list was empty.
    expect(getMetrics().coalescedSamplesPerSec).toBe(0);

    cleanup();
    unlockPointer();
  });
});

describe("CaptureMetrics — gamepad count via mocked navigator.getGamepads", () => {
  it("reports connected gamepad count", () => {
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [
        { index: 0, buttons: [], axes: [] },
        null,
        { index: 2, buttons: [], axes: [] },
      ],
      configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    expect(getMetrics().gamepadCount).toBe(2);
    cleanup();
  });

  it("reports 0 when no gamepads connected", () => {
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [], configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    expect(getMetrics().gamepadCount).toBe(0);
    cleanup();
  });

  it("reports 0 when navigator.getGamepads is absent", () => {
    delete (navigator as unknown as { getGamepads?: unknown }).getGamepads;

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    expect(getMetrics().gamepadCount).toBe(0);
    cleanup();
  });
});

describe("CaptureMetrics — pads identity list (sparse getGamepads array)", () => {
  it("lists each connected pad's index/id, skipping nulls interleaved with live pads", () => {
    // Sparse: index 1 is null while 0 and 2 hold live pads — the array shape
    // navigator.getGamepads() actually returns after a pad disconnects and a
    // later one connects into a different slot.
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [
        { index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)", buttons: [], axes: [] },
        null,
        { index: 2, id: "DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)", buttons: [], axes: [] },
      ],
      configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    const m = getMetrics();
    expect(m.pads).toEqual([
      { index: 0, id: "Xbox Wireless Controller (STANDARD GAMEPAD Vendor: 045e Product: 0b13)" },
      { index: 2, id: "DualSense Wireless Controller (STANDARD GAMEPAD Vendor: 054c Product: 0ce6)" },
    ]);
    cleanup();
  });

  it("gamepadCount and pads.length always agree — zero pads", () => {
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [null, null], configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    const m = getMetrics();
    expect(m.gamepadCount).toBe(0);
    expect(m.pads).toEqual([]);
    cleanup();
  });

  it("gamepadCount and pads.length always agree — one pad", () => {
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, id: "Generic USB Gamepad", buttons: [], axes: [] }],
      configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    const m = getMetrics();
    expect(m.gamepadCount).toBe(1);
    expect(m.pads).toHaveLength(1);
    expect(m.pads[0]).toEqual({ index: 0, id: "Generic USB Gamepad" });
    cleanup();
  });

  it("gamepadCount and pads.length always agree — multiple pads across a sparse array", () => {
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [
        { index: 0, id: "Pad A", buttons: [], axes: [] },
        null,
        { index: 2, id: "Pad B", buttons: [], axes: [] },
        { index: 3, id: "Pad C", buttons: [], axes: [] },
      ],
      configurable: true,
    });

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    const m = getMetrics();
    expect(m.gamepadCount).toBe(m.pads.length);
    expect(m.gamepadCount).toBe(3);
    expect(m.pads.map((p) => p.index)).toEqual([0, 2, 3]);
    cleanup();
  });

  it("reports an empty pads list when navigator.getGamepads is absent", () => {
    delete (navigator as unknown as { getGamepads?: unknown }).getGamepads;

    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });
    const m = getMetrics();
    expect(m.gamepadCount).toBe(0);
    expect(m.pads).toEqual([]);
    cleanup();
  });
});

describe("CaptureMetrics — gamepad send rate", () => {
  it("counts a gamepad state-change send via manual rAF invocation", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    // Capture the rAF callback so we can drive pollGamepads manually.
    // Use an object wrapper to avoid TS control-flow narrowing of `let` to `never`.
    const rafHolder: { cb: ((t: number) => void) | null } = { cb: null };
    vi.stubGlobal("requestAnimationFrame", vi.fn((cb: (t: number) => void) => {
      rafHolder.cb = cb;
      return 1;
    }));

    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, buttons: [{ value: 1 }], axes: [0.5] }],
      configurable: true,
    });

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    // Gamepad forwarding is gated on capture — lock the pointer first.
    lockPointer(video);

    // Advance 1 s so the window denominator is 1.
    vi.advanceTimersByTime(1000);

    // First rAF tick: no prev state → sends once.
    if (rafHolder.cb) rafHolder.cb(0);
    // Second tick: state unchanged → no send.
    if (rafHolder.cb) rafHolder.cb(16);

    const m = getMetrics();
    // Exactly one send in 1 s → rate should be ≈ 1.0/s (within 0.1).
    expect(m.gamepadSendPerSec).toBeCloseTo(1.0, 1);
    expect(send).toHaveBeenCalledTimes(1);
    expect(send).toHaveBeenCalledWith(expect.objectContaining({ t: "gp", i: 0 }));
    // The no-send second tick (state unchanged) must not inflate the count.
    expect(send).not.toHaveBeenCalledTimes(2);

    cleanup();
    unlockPointer();
  });
});

describe("CaptureMetrics — bufferedAmount and backpressure", () => {
  it("reports channelBufferedAmount from the channel at snapshot time", () => {
    const ch = makeChannel(8192);
    const { cleanup, getMetrics } = setupCapture({
      videoEl: makeVideo(), sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: ch,
    });
    expect(getMetrics().channelBufferedAmount).toBe(8192);
    cleanup();
  });

  it("latches backpressureDetected when bufferedAmount exceeds 16 KiB on a send", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const ch = makeChannel(32 * 1024); // > 16 KiB
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: ch,
    });

    lockPointer(video);
    dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);

    expect(getMetrics().backpressureDetected).toBe(true);
    cleanup();
    unlockPointer();
  });

  it("reports backpressureDetected=false when bufferedAmount is below threshold", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const ch = makeChannel(0);
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: ch,
    });

    lockPointer(video);
    dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);

    expect(getMetrics().backpressureDetected).toBe(false);
    cleanup();
    unlockPointer();
  });

  it("resets backpressureDetected after a clean window follows a high-pressure window", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const ch = { bufferedAmount: 32 * 1024, readyState: "open" } as unknown as RTCDataChannel;
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: ch,
    });

    lockPointer(video);
    dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);
    expect(getMetrics().backpressureDetected).toBe(true); // window 1: high

    // Drop backpressure and send another event in window 2.
    (ch as unknown as { bufferedAmount: number }).bufferedAmount = 0;
    dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);
    expect(getMetrics().backpressureDetected).toBe(false); // window 2: clean

    cleanup();
    unlockPointer();
  });

  it("reports backpressureDetected=false and channelBufferedAmount=0 when channel is absent", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(),
      // channel intentionally omitted
    });

    lockPointer(video);
    dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);

    const m = getMetrics();
    expect(m.backpressureDetected).toBe(false);
    expect(m.channelBufferedAmount).toBe(0);

    cleanup();
    unlockPointer();
  });
});

describe("input release — Ctrl+Alt+Shift+Z combo", () => {
  it("exits pointer lock and does NOT forward the completing 'Z' keydown", () => {
    const exitPointerLock = vi.fn();
    (document as unknown as { exitPointerLock: () => void }).exitPointerLock =
      exitPointerLock;

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    dispatchKeyDown("KeyZ", { ctrlKey: true, altKey: true, shiftKey: true });

    expect(exitPointerLock).toHaveBeenCalledTimes(1);
    // KeyZ evdev code is 44 — it must never reach the host.
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 44 }),
    );

    cleanup();
    unlockPointer();
    delete (document as unknown as { exitPointerLock?: () => void }).exitPointerLock;
  });

  it("integration: modifiers held + combo release + pointerlockchange flushes key-ups for the held modifiers, never forwards Z", () => {
    const exitPointerLock = vi.fn(() => {
      // Mimic the real browser: exitPointerLock() eventually fires
      // pointerlockchange with pointerLockElement cleared. The test drives
      // that transition explicitly below (jsdom doesn't do it for us).
    });
    (document as unknown as { exitPointerLock: () => void }).exitPointerLock =
      exitPointerLock;

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);

    // Hold Ctrl, Alt, Shift down first (as a real combo press would), then the
    // completing Z.
    dispatchKeyDown("ControlLeft", { ctrlKey: true });
    dispatchKeyDown("AltLeft", { ctrlKey: true, altKey: true });
    dispatchKeyDown("ShiftLeft", { ctrlKey: true, altKey: true, shiftKey: true });
    dispatchKeyDown("KeyZ", { ctrlKey: true, altKey: true, shiftKey: true });

    expect(exitPointerLock).toHaveBeenCalledTimes(1);
    // Z (evdev 44) must never be forwarded, pressed or released.
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 44 }),
    );

    send.mockClear();
    // Drive the actual unlock transition (evdev: ControlLeft=29, AltLeft=56, ShiftLeft=42).
    unlockPointer();

    expect(send).toHaveBeenCalledWith({ t: "k", code: 29, pressed: false });
    expect(send).toHaveBeenCalledWith({ t: "k", code: 56, pressed: false });
    expect(send).toHaveBeenCalledWith({ t: "k", code: 42, pressed: false });
    // Still never Z.
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 44 }),
    );

    cleanup();
    delete (document as unknown as { exitPointerLock?: () => void }).exitPointerLock;
  });

  it("forwards a plain 'Z' (no modifiers) normally", () => {
    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    dispatchKeyDown("KeyZ");
    expect(send).toHaveBeenCalledWith({ t: "k", code: 44, pressed: true });

    cleanup();
    unlockPointer();
  });
});

describe("input release — Ctrl+Alt+Shift+Q overlay summon combo", () => {
  it("exits pointer lock, calls onSummonOverlay once, and does NOT forward the completing 'Q' keydown", () => {
    const exitPointerLock = vi.fn();
    (document as unknown as { exitPointerLock: () => void }).exitPointerLock =
      exitPointerLock;

    const video = makeVideo();
    const send = vi.fn();
    const onSummonOverlay = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video,
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
      onSummonOverlay,
    });

    lockPointer(video);
    dispatchKeyDown("KeyQ", { ctrlKey: true, altKey: true, shiftKey: true });

    expect(onSummonOverlay).toHaveBeenCalledTimes(1);
    expect(exitPointerLock).toHaveBeenCalledTimes(1);
    // KeyQ evdev code is 16 — it must never reach the host.
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 16 }),
    );

    cleanup();
    unlockPointer();
    delete (document as unknown as { exitPointerLock?: () => void }).exitPointerLock;
  });

  it("still exits pointer lock when onSummonOverlay is not provided", () => {
    const exitPointerLock = vi.fn();
    (document as unknown as { exitPointerLock: () => void }).exitPointerLock =
      exitPointerLock;

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    expect(() =>
      dispatchKeyDown("KeyQ", { ctrlKey: true, altKey: true, shiftKey: true }),
    ).not.toThrow();

    expect(exitPointerLock).toHaveBeenCalledTimes(1);
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 16 }),
    );

    cleanup();
    unlockPointer();
    delete (document as unknown as { exitPointerLock?: () => void }).exitPointerLock;
  });

  it("forwards a plain 'Q' (no modifiers) normally", () => {
    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    dispatchKeyDown("KeyQ");
    expect(send).toHaveBeenCalledWith({ t: "k", code: 16, pressed: true });

    cleanup();
    unlockPointer();
  });
});

describe("input release — held-key flush on unlock", () => {
  it("sends key-up for every held key when pointer lock is released", () => {
    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    // KeyA=30, KeyB=48 held down (no key-up dispatched).
    dispatchKeyDown("KeyA");
    dispatchKeyDown("KeyB");
    expect(send).toHaveBeenCalledWith({ t: "k", code: 30, pressed: true });
    expect(send).toHaveBeenCalledWith({ t: "k", code: 48, pressed: true });

    send.mockClear();
    unlockPointer();

    // Each held key is released on the unlock transition.
    expect(send).toHaveBeenCalledWith({ t: "k", code: 30, pressed: false });
    expect(send).toHaveBeenCalledWith({ t: "k", code: 48, pressed: false });

    cleanup();
  });

  it("does not double-release a key that was already released before unlock", () => {
    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    dispatchKeyDown("KeyA");
    dispatchKeyUp("KeyA"); // removes 30 from the held set
    send.mockClear();

    unlockPointer();
    // Nothing left held → no key-up flushed for 30.
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "k", code: 30, pressed: false }),
    );

    cleanup();
  });
});

describe("gamepad — capture gating and reset on unlock", () => {
  function withRaf() {
    const rafHolder: { cb: ((t: number) => void) | null } = { cb: null };
    vi.stubGlobal("requestAnimationFrame", vi.fn((cb: (t: number) => void) => {
      rafHolder.cb = cb;
      return 1;
    }));
    return rafHolder;
  }

  it("does NOT forward gamepad state while pointer is unlocked", () => {
    const rafHolder = withRaf();
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, buttons: [{ value: 1 }], axes: [0.5] }],
      configurable: true,
    });

    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: makeVideo(), sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    // Not locked → poll must not forward the pad.
    if (rafHolder.cb) rafHolder.cb(0);
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ t: "gp" }),
    );

    cleanup();
  });

  it("forwards gamepad state once captured, sends a neutral state on release, and resets so re-capture resends full state", () => {
    const rafHolder = withRaf();
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, buttons: [{ value: 1 }], axes: [0.5] }],
      configurable: true,
    });

    const video = makeVideo();
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video, sendInput: send, onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    // First poll while captured → full state forwarded.
    if (rafHolder.cb) rafHolder.cb(0);
    expect(send).toHaveBeenCalledWith({ t: "gp", i: 0, buttons: [1], axes: [0.5] });

    send.mockClear();
    unlockPointer();
    // Neutral state sent for the pad that had been forwarded (same arity, zeroed).
    expect(send).toHaveBeenCalledWith({ t: "gp", i: 0, buttons: [0], axes: [0] });

    // Re-capture: gpPrev was cleared, so the next poll resends the full state.
    send.mockClear();
    lockPointer(video);
    if (rafHolder.cb) rafHolder.cb(1);
    expect(send).toHaveBeenCalledWith({ t: "gp", i: 0, buttons: [1], axes: [0.5] });

    cleanup();
    unlockPointer();
  });
});

describe("CaptureMetrics — high-rate send-rate accounting (fake-timer validation)", () => {
  // NOTE: These tests validate rate-window accounting logic only.
  // True 500/1000 Hz end-to-end testing requires a real browser with a synthetic
  // high-rate HID device injected via Playwright/CDP. That is documented in the PR
  // as a real-browser harness concern beyond jsdom unit-test scope.

  it("correctly accounts for 500 events dispatched over 1 s (simulates 500 Hz)", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    for (let i = 0; i < 500; i++) dispatchPointerMove(1, 0);
    vi.advanceTimersByTime(1000);

    // 500 sends / 1.0 s ≈ 500/s. toBeCloseTo(-1) = within factor of 10.
    expect(getMetrics().inputMsgPerSec).toBeCloseTo(500, -1);

    cleanup();
    unlockPointer();
  });

  it("correctly accounts for 1000 coalesced samples over 1 s (simulates 1000 Hz)", () => {
    vi.useFakeTimers({ toFake: ["performance"] });

    const video = makeVideo();
    const { cleanup, getMetrics } = setupCapture({
      videoEl: video, sendInput: vi.fn(), onCaptureChange: vi.fn(), channel: makeChannel(),
    });

    lockPointer(video);
    // 10 pointer-move events × 100 coalesced sub-events = 1000 sends.
    for (let i = 0; i < 10; i++) {
      dispatchPointerMove(1, 0, Array.from({ length: 100 }, () => ({ movementX: 1, movementY: 0 })));
    }
    vi.advanceTimersByTime(1000);

    const m = getMetrics();
    expect(m.coalescedSamplesPerSec).toBeCloseTo(1000, -1);
    expect(m.inputMsgPerSec).toBeCloseTo(1000, -1);

    cleanup();
    unlockPointer();
  });
});

// ─────────────────────────────────────────────────────────────────────────────
// Capture without Pointer Lock (iPadOS Safari).
//
// The regression these guard: one boolean (`pointerLocked`) gated FOUR
// modalities, and on a browser with no Pointer Lock it could never become true,
// so a connected controller was polled every frame and never forwarded a byte.
// Every test below asserts on what reaches sendInput — the DataChannel send —
// not on any UI state.
//
// jsdom has no Pointer Lock at all (`"requestPointerLock" in Element.prototype`
// is false), which is exactly the iPad shape, so these need no un-stubbing;
// the lock-capable cases define the method explicitly.
// ─────────────────────────────────────────────────────────────────────────────

/** Install / remove Element.prototype.requestPointerLock for one test. */
function setPointerLockApi(impl: (() => unknown) | null) {
  if (impl) {
    Object.defineProperty(Element.prototype, "requestPointerLock", {
      value: impl,
      configurable: true,
      writable: true,
    });
  } else {
    delete (Element.prototype as unknown as { requestPointerLock?: unknown })
      .requestPointerLock;
  }
}

afterEach(() => setPointerLockApi(null));

describe("pointerLockSupported — probes the ELEMENT method, not the document property", () => {
  it("is false when Element.prototype.requestPointerLock is absent (WebKit/iPadOS)", () => {
    setPointerLockApi(null);
    // The document property and the element method ship independently, so the
    // old `"pointerLockElement" in document` probe could answer true with no
    // lockable element anywhere — the capability report said pointer_lock:true
    // for a client that has none. Presence of the property must not change the
    // answer here.
    Object.defineProperty(document, "pointerLockElement", {
      value: null,
      configurable: true,
    });
    expect("pointerLockElement" in document).toBe(true);
    expect(pointerLockSupported()).toBe(false);
  });

  it("is true when the element method exists", () => {
    setPointerLockApi(() => undefined);
    expect(pointerLockSupported()).toBe(true);
  });
});

describe("engage() without Pointer Lock — the send gate opens anyway", () => {
  function withRaf() {
    const holder: { cb: ((t: number) => void) | null } = { cb: null };
    vi.stubGlobal("requestAnimationFrame", vi.fn((cb: (t: number) => void) => {
      holder.cb = cb;
      return 1;
    }));
    return holder;
  }

  function mountVideo(): HTMLVideoElement {
    const el = document.createElement("video");
    document.body.appendChild(el);
    return el;
  }

  it("reports mode 'fallback' and captured=true with pointerLocked=false", async () => {
    setPointerLockApi(null);
    const onCapture = vi.fn();
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: vi.fn(),
      onCaptureChange: onCapture,
      channel: makeChannel(),
    });

    const result = await engage();
    expect(result.mode).toBe("fallback");
    expect(onCapture).toHaveBeenLastCalledWith({ captured: true, pointerLocked: false });
    const m = getMetrics();
    expect(m.captured).toBe(true);
    expect(m.pointerLocked).toBe(false);
    expect(m.pointerLockSupported).toBe(false);

    cleanup();
  });

  it("FORWARDS a gamepad state change — the defect: the pad was polled, never sent", async () => {
    setPointerLockApi(null);
    const rafHolder = withRaf();
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, buttons: [{ value: 1 }], axes: [0.5] }],
      configurable: true,
    });

    const send = vi.fn();
    const { cleanup, engage } = setupCapture({
      videoEl: makeVideo(),
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    // Before capture: polled, not sent (unchanged behaviour).
    if (rafHolder.cb) rafHolder.cb(0);
    expect(send).not.toHaveBeenCalled();

    await engage();
    if (rafHolder.cb) rafHolder.cb(1);
    expect(send).toHaveBeenCalledWith({ t: "gp", i: 0, buttons: [1], axes: [0.5] });

    cleanup();
  });

  it("FORWARDS hardware keyboard key-down and key-up", async () => {
    setPointerLockApi(null);
    const send = vi.fn();
    const { cleanup, engage } = setupCapture({
      videoEl: makeVideo(),
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    dispatchKeyDown("KeyW");
    expect(send).not.toHaveBeenCalled();

    await engage();
    dispatchKeyDown("KeyW");
    expect(send).toHaveBeenCalledWith({ t: "k", code: EVDEV.KeyW, pressed: true });
    dispatchKeyUp("KeyW");
    expect(send).toHaveBeenCalledWith({ t: "k", code: EVDEV.KeyW, pressed: false });

    cleanup();
  });

  it("does NOT forward relative mouse motion — no lock, no mouse look", async () => {
    setPointerLockApi(null);
    const send = vi.fn();
    const { cleanup, engage } = setupCapture({
      videoEl: makeVideo(),
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    await engage();
    dispatchPointerMove(25, -14);
    expect(send).not.toHaveBeenCalledWith(expect.objectContaining({ t: "mm" }));

    cleanup();
  });

  it("forwards a real mouse button aimed at the video, but not a touch-synthesized tap", async () => {
    setPointerLockApi(null);
    const video = mountVideo();
    const send = vi.fn();
    const { cleanup, engage } = setupCapture({
      videoEl: video,
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    await engage();

    // A tap: pointerdown(pointerType:"touch") then a synthesized mousedown. This
    // is also what a tap on the session-menu button produces, so forwarding it
    // would fire a shot into the game every time the user reached for the menu.
    document.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerType: "touch" }));
    video.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, button: 0 }));
    expect(send).not.toHaveBeenCalledWith(expect.objectContaining({ t: "mb" }));

    // A trackpad/mouse click on the picture: forwarded.
    document.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerType: "mouse" }));
    video.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, button: 0 }));
    expect(send).toHaveBeenCalledWith({ t: "mb", button: MB_MAP[0], pressed: true });

    // The same real click aimed at session chrome (not the video) is not the game's.
    send.mockClear();
    const chrome = document.createElement("button");
    document.body.appendChild(chrome);
    chrome.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, button: 0 }));
    expect(send).not.toHaveBeenCalledWith(expect.objectContaining({ t: "mb" }));

    cleanup();
    chrome.remove();
    video.remove();
  });

  it("Escape releases capture and flushes held keys + gamepads — the no-lock way out", async () => {
    setPointerLockApi(null);
    const rafHolder = withRaf();
    Object.defineProperty(navigator, "getGamepads", {
      value: () => [{ index: 0, buttons: [{ value: 1 }], axes: [0.5] }],
      configurable: true,
    });

    const send = vi.fn();
    const onCapture = vi.fn();
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: send,
      onCaptureChange: onCapture,
      channel: makeChannel(),
    });

    await engage();
    dispatchKeyDown("ShiftLeft");
    if (rafHolder.cb) rafHolder.cb(0);
    send.mockClear();

    dispatchKeyDown("Escape");

    // Escape itself is never forwarded — it is our release gesture here.
    expect(send).not.toHaveBeenCalledWith({ t: "k", code: EVDEV.Escape, pressed: true });
    // Held modifier released, pad neutralized.
    expect(send).toHaveBeenCalledWith({ t: "k", code: EVDEV.ShiftLeft, pressed: false });
    expect(send).toHaveBeenCalledWith({ t: "gp", i: 0, buttons: [0], axes: [0] });
    expect(getMetrics().captured).toBe(false);
    expect(onCapture).toHaveBeenLastCalledWith({ captured: false, pointerLocked: false });

    // Released: nothing forwards any more.
    send.mockClear();
    dispatchKeyDown("KeyW");
    if (rafHolder.cb) rafHolder.cb(1);
    expect(send).not.toHaveBeenCalled();

    cleanup();
  });

  it("release() is idempotent and re-engaging works", async () => {
    setPointerLockApi(null);
    const { cleanup, engage, release, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    release();
    expect(getMetrics().captured).toBe(false);
    await engage();
    expect(getMetrics().captured).toBe(true);
    release();
    release();
    expect(getMetrics().captured).toBe(false);
    await engage();
    expect(getMetrics().captured).toBe(true);

    cleanup();
  });

  it("the Ctrl+Alt+Shift+Q summon combo still releases and opens the overlay", async () => {
    setPointerLockApi(null);
    const send = vi.fn();
    const onSummonOverlay = vi.fn();
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: makeVideo(),
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
      onSummonOverlay,
    });

    await engage();
    send.mockClear();
    dispatchKeyDown("KeyQ", { ctrlKey: true, altKey: true, shiftKey: true });
    expect(onSummonOverlay).toHaveBeenCalledTimes(1);
    expect(getMetrics().captured).toBe(false);
    expect(send).not.toHaveBeenCalledWith(
      expect.objectContaining({ code: EVDEV.KeyQ, pressed: true }),
    );

    cleanup();
  });
});

describe("engage() WITH Pointer Lock — desktop path is unchanged", () => {
  it("requests the lock on the video element and reports mode 'pointer-lock'", async () => {
    const requestPointerLock = vi.fn(() => undefined);
    setPointerLockApi(requestPointerLock);
    const video = document.createElement("video");
    const { cleanup, engage } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    const result = await engage();
    expect(requestPointerLock).toHaveBeenCalledTimes(1);
    expect(result.mode).toBe("pointer-lock");
    cleanup();
  });

  it("awaits a Promise-returning requestPointerLock (Chrome ≥113)", async () => {
    setPointerLockApi(() => Promise.resolve());
    const { cleanup, engage } = setupCapture({
      videoEl: document.createElement("video"),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    expect((await engage()).mode).toBe("pointer-lock");
    cleanup();
  });

  it("a REJECTED lock reports 'failed' and engages nothing — no silent degrade", async () => {
    setPointerLockApi(() => Promise.reject(new Error("locked out")));
    const send = vi.fn();
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: document.createElement("video"),
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    const result = await engage();
    expect(result.mode).toBe("failed");
    expect(result.error).toBeInstanceOf(Error);
    expect(getMetrics().captured).toBe(false);
    dispatchKeyDown("KeyW");
    expect(send).not.toHaveBeenCalled();
    cleanup();
  });

  it("a SYNCHRONOUSLY THROWING lock reports 'failed' rather than escaping the caller", async () => {
    setPointerLockApi(() => {
      throw new TypeError("requestPointerLock is not a function");
    });
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: document.createElement("video"),
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });
    const result = await engage();
    expect(result.mode).toBe("failed");
    expect(getMetrics().captured).toBe(false);
    cleanup();
  });

  it("release() goes through exitPointerLock so browser and module state cannot diverge", async () => {
    const video = document.createElement("video");
    setPointerLockApi(() => undefined);
    const exitPointerLock = vi.fn();
    Object.defineProperty(document, "exitPointerLock", {
      value: exitPointerLock,
      configurable: true,
    });
    const { cleanup, release } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    lockPointer(video);
    release();
    expect(exitPointerLock).toHaveBeenCalledTimes(1);
    unlockPointer();
    cleanup();
  });

  it("Escape is NOT intercepted while pointer-locked — the browser (or Keyboard Lock) owns it", () => {
    const video = document.createElement("video");
    setPointerLockApi(() => undefined);
    const send = vi.fn();
    const { cleanup } = setupCapture({
      videoEl: video,
      sendInput: send,
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    lockPointer(video);
    dispatchKeyDown("Escape");
    // Forwarded to the host as a normal key, exactly as before this change.
    expect(send).toHaveBeenCalledWith({ t: "k", code: EVDEV.Escape, pressed: true });

    cleanup();
    unlockPointer();
  });

  it("a pointerlockchange for ANOTHER element cannot drop a fallback-mode capture", async () => {
    setPointerLockApi(null);
    const video = document.createElement("video");
    const { cleanup, engage, getMetrics } = setupCapture({
      videoEl: video,
      sendInput: vi.fn(),
      onCaptureChange: vi.fn(),
      channel: makeChannel(),
    });

    await engage();
    Object.defineProperty(document, "pointerLockElement", {
      value: document.createElement("div"),
      configurable: true,
    });
    document.dispatchEvent(new Event("pointerlockchange"));
    expect(getMetrics().captured).toBe(true);

    cleanup();
  });
});
