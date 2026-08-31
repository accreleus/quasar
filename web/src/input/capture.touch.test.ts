// Touch gesture layer (input/capture.ts) — asserts on the sendInput SINK, i.e.
// the actual wire frames from protocol/input.md, never on UI classes.
//
// jsdom has no PointerEvent, so one is stubbed here with the fields the gesture
// layer reads (pointerId / pointerType / clientX / clientY). The stub is local
// to this file: capture.test.ts installs its own, and the two must not fight.
//
// The video element is a REAL element (not the `{}` stand-in the metrics tests
// use) because the touch-down scope check is `e.target === videoEl` — a stand-in
// object can never be an event target, so it would silently pass every test by
// never handling anything.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setupCapture, touchLookSupported } from "./capture";

type Msg = Record<string, unknown>;

const BTN_LEFT = 272;
const BTN_RIGHT = 273;

class FakePointerEvent extends Event {
  pointerId: number;
  pointerType: string;
  clientX: number;
  clientY: number;
  movementX = 0;
  movementY = 0;
  constructor(
    type: string,
    init: EventInit & {
      pointerId?: number;
      pointerType?: string;
      clientX?: number;
      clientY?: number;
    } = {},
  ) {
    super(type, { bubbles: true, cancelable: true, ...init });
    this.pointerId = init.pointerId ?? 1;
    this.pointerType = init.pointerType ?? "touch";
    this.clientX = init.clientX ?? 0;
    this.clientY = init.clientY ?? 0;
  }
}

let video: HTMLVideoElement;
let chrome: HTMLButtonElement;
let sent: Msg[];
let harness: ReturnType<typeof setupCapture>;

/** Snapshot the message, because capture.ts reuses one object for mm/ms (#139). */
function makeSink() {
  sent = [];
  return (msg: Msg) => {
    sent.push({ ...msg });
  };
}

function down(target: EventTarget, x: number, y: number, id = 1, pointerType = "touch") {
  target.dispatchEvent(
    new FakePointerEvent("pointerdown", { pointerId: id, pointerType, clientX: x, clientY: y }),
  );
}
function move(x: number, y: number, id = 1, pointerType = "touch") {
  document.dispatchEvent(
    new FakePointerEvent("pointermove", { pointerId: id, pointerType, clientX: x, clientY: y }),
  );
}
function up(x: number, y: number, id = 1, pointerType = "touch") {
  document.dispatchEvent(
    new FakePointerEvent("pointerup", { pointerId: id, pointerType, clientX: x, clientY: y }),
  );
}
function cancel(x: number, y: number, id = 1) {
  document.dispatchEvent(
    new FakePointerEvent("pointercancel", { pointerId: id, pointerType: "touch", clientX: x, clientY: y }),
  );
}

/** Engage capture in fallback mode (no Pointer Lock — the iPad case). */
async function engageFallback() {
  await harness.engage();
}

beforeEach(async () => {
  vi.useFakeTimers();
  (globalThis as unknown as { PointerEvent: typeof FakePointerEvent }).PointerEvent =
    FakePointerEvent;
  // No Pointer Lock: this is the whole point of the touch path.
  delete (Element.prototype as unknown as Record<string, unknown>).requestPointerLock;
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
  vi.stubGlobal("requestAnimationFrame", () => 0);
  vi.stubGlobal("cancelAnimationFrame", () => {});

  video = document.createElement("video");
  chrome = document.createElement("button"); // stands in for .session-summon
  document.body.append(video, chrome);

  harness = setupCapture({
    videoEl: video,
    sendInput: makeSink(),
    onCaptureChange: () => {},
  });
});

afterEach(() => {
  harness.cleanup();
  document.body.innerHTML = "";
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("gating", () => {
  it("emits nothing while capture is released", () => {
    down(video, 100, 100);
    move(300, 100);
    up(300, 100);
    expect(sent).toEqual([]);
  });

  it("emits nothing for a gesture that started before capture was engaged", async () => {
    down(video, 100, 100);
    await engageFallback();
    sent.length = 0;
    move(300, 100);
    up(300, 100);
    expect(sent).toEqual([]);
  });

  it("stops emitting the moment capture is released mid-drag", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(200, 100);
    expect(sent.length).toBeGreaterThan(0);
    sent.length = 0;
    harness.release();
    move(300, 100);
    up(300, 100);
    expect(sent).toEqual([]);
  });
});

describe("tap", () => {
  it("emits a left press+release pair and no motion", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(103, 102); // inside the 10px slop — finger roll, not a drag
    up(103, 102);
    expect(sent).toEqual([
      { t: "mb", button: BTN_LEFT, pressed: true },
      { t: "mb", button: BTN_LEFT, pressed: false },
    ]);
  });

  it("does not emit a click when the platform cancels the press", async () => {
    await engageFallback();
    down(video, 100, 100);
    cancel(100, 100);
    expect(sent).toEqual([]);
  });
});

describe("drag to look", () => {
  it("emits mm carrying the slop-absorbed motion, then incremental deltas", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(105, 100); // still inside the slop → nothing yet
    expect(sent).toEqual([]);
    move(120, 104); // crosses → one mm for the FULL start→now delta
    expect(sent).toEqual([{ t: "mm", dx: 20, dy: 4 }]);
    move(130, 100); // incremental from the last sample
    expect(sent[1]).toEqual({ t: "mm", dx: 10, dy: -4 });
  });

  it("never also emits a click when the finger lifts", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(200, 150);
    up(200, 150);
    expect(sent.every((m) => m.t === "mm")).toBe(true);
  });

  it("uses clientX/clientY, not movementX/movementY", async () => {
    await engageFallback();
    down(video, 400, 400);
    // movementX/Y stay 0 on every synthetic event here (WebKit's real-world
    // behaviour outside Pointer Lock); the deltas must still be right.
    move(360, 430);
    expect(sent).toEqual([{ t: "mm", dx: -40, dy: 30 }]);
  });
});

describe("long press", () => {
  it("emits a right press at 500ms and its release on lift", async () => {
    await engageFallback();
    down(video, 100, 100);
    vi.advanceTimersByTime(499);
    expect(sent).toEqual([]);
    vi.advanceTimersByTime(1);
    expect(sent).toEqual([{ t: "mb", button: BTN_RIGHT, pressed: true }]);
    up(100, 100);
    expect(sent).toEqual([
      { t: "mb", button: BTN_RIGHT, pressed: true },
      { t: "mb", button: BTN_RIGHT, pressed: false },
    ]);
  });

  it("cannot fire during a drag", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(200, 100); // commits to look
    vi.advanceTimersByTime(2000);
    expect(sent.some((m) => m.button === BTN_RIGHT)).toBe(false);
  });

  it("does not also drag once it has fired", async () => {
    await engageFallback();
    down(video, 100, 100);
    vi.advanceTimersByTime(500);
    sent.length = 0;
    move(300, 300);
    expect(sent).toEqual([]);
  });

  it("releases the right button if capture ends while it is held", async () => {
    await engageFallback();
    down(video, 100, 100);
    vi.advanceTimersByTime(500);
    sent.length = 0;
    harness.release();
    expect(sent).toContainEqual({ t: "mb", button: BTN_RIGHT, pressed: false });
  });
});

describe("two-finger scroll", () => {
  it("emits ms from the midpoint's travel, with wheel-down sign for a swipe up", async () => {
    await engageFallback();
    down(video, 100, 200, 1);
    down(video, 140, 200, 2);
    // Both fingers 20px up → midpoint 20px up → positive deltaY (wheel-down).
    move(100, 180, 1);
    move(140, 180, 2);
    const scrolls = sent.filter((m) => m.t === "ms");
    expect(scrolls.length).toBeGreaterThan(0);
    const totalY = scrolls.reduce((a, m) => a + (m.dy as number), 0);
    expect(totalY).toBeCloseTo(20, 5);
    expect(scrolls.reduce((a, m) => a + (m.dx as number), 0)).toBeCloseTo(0, 5);
  });

  it("emits no mm and no click for a two-finger gesture", async () => {
    await engageFallback();
    down(video, 100, 200, 1);
    down(video, 140, 200, 2);
    move(100, 120, 1);
    move(140, 120, 2);
    up(100, 120, 1);
    up(140, 120, 2);
    expect(sent.every((m) => m.t === "ms")).toBe(true);
  });

  it("does not lurch when one of the two fingers lifts", async () => {
    await engageFallback();
    down(video, 100, 200, 1);
    down(video, 300, 200, 2);
    up(300, 200, 2); // midpoint would jump 100px right without re-anchoring
    sent.length = 0;
    move(100, 200, 1);
    expect(sent).toEqual([]);
  });

  it("cancels a pending long press when the second finger lands", async () => {
    await engageFallback();
    down(video, 100, 200, 1);
    vi.advanceTimersByTime(300);
    down(video, 140, 200, 2);
    vi.advanceTimersByTime(1000);
    expect(sent.some((m) => m.button === BTN_RIGHT)).toBe(false);
  });
});

describe("scope — session chrome stays tappable", () => {
  it("ignores a gesture that starts on the summon button", async () => {
    await engageFallback();
    down(chrome, 100, 100);
    move(300, 300);
    up(300, 300);
    expect(sent).toEqual([]);
  });

  it("keeps steering a drag that started on the video and left it", async () => {
    await engageFallback();
    down(video, 100, 100);
    move(300, 100); // finger has wandered over the chrome; still our gesture
    expect(sent).toEqual([{ t: "mm", dx: 200, dy: 0 }]);
  });

  it("does not consume a mouse pointerdown", async () => {
    await engageFallback();
    down(video, 100, 100, 5, "mouse");
    move(300, 100, 5, "mouse");
    up(300, 100, 5, "mouse");
    // The mouse path is gated on pointerLocked, which is false here, so a
    // released-pointer mouse produces nothing — and the touch layer must not
    // step in and invent motion for it.
    expect(sent).toEqual([]);
  });
});

describe("touchLookSupported", () => {
  it("is true with a touchscreen and no Pointer Lock", () => {
    Object.defineProperty(navigator, "maxTouchPoints", { value: 5, configurable: true });
    expect(touchLookSupported()).toBe(true);
  });

  it("is true under WebKit iPad emulation, which reports maxTouchPoints 0", () => {
    Object.defineProperty(navigator, "maxTouchPoints", { value: 0, configurable: true });
    (window as unknown as Record<string, unknown>).ontouchstart = null;
    expect(touchLookSupported()).toBe(true);
    delete (window as unknown as Record<string, unknown>).ontouchstart;
  });

  it("is false without a touchscreen", () => {
    Object.defineProperty(navigator, "maxTouchPoints", { value: 0, configurable: true });
    expect("ontouchstart" in window).toBe(false);
    expect(touchLookSupported()).toBe(false);
  });

  it("is false where Pointer Lock exists (a touch laptop locks the mouse instead)", () => {
    Object.defineProperty(navigator, "maxTouchPoints", { value: 5, configurable: true });
    (Element.prototype as unknown as Record<string, unknown>).requestPointerLock = () => {};
    expect(touchLookSupported()).toBe(false);
    delete (Element.prototype as unknown as Record<string, unknown>).requestPointerLock;
  });
});
