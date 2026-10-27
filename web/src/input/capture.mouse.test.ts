// Relative mouse speed through setupCapture (mouseScale.ts holds the arithmetic).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setupCapture } from "./capture";
import type { MouseSettings } from "./mouseScale";

/** A video element with a stream size and a box, as the session view has it. */
function video(streamW: number, boxW: number, streamH = streamW * 0.5625, boxH = boxW * 0.5625): HTMLVideoElement {
  const el = document.createElement("video");
  Object.defineProperty(el, "videoWidth", { value: streamW, configurable: true });
  Object.defineProperty(el, "videoHeight", { value: streamH, configurable: true });
  Object.defineProperty(el, "clientWidth", { value: boxW, configurable: true });
  Object.defineProperty(el, "clientHeight", { value: boxH, configurable: true });
  return el;
}

function start(el: HTMLVideoElement, mouse?: MouseSettings) {
  const sent: { dx: number; dy: number }[] = [];
  const cap = setupCapture({
    videoEl: el,
    sendInput: (m) => {
      if (m.t === "mm") sent.push({ dx: m.dx as number, dy: m.dy as number });
    },
    onCaptureChange: () => {},
    mouse,
  });
  Object.defineProperty(document, "pointerLockElement", { value: el, configurable: true });
  document.dispatchEvent(new Event("pointerlockchange"));
  return { cap, sent };
}

const move = (movementX: number, movementY = 0) => {
  const e = new Event("pointermove") as Event & { movementX: number; movementY: number };
  e.movementX = movementX;
  e.movementY = movementY;
  document.dispatchEvent(e);
};

const MATCH = { match: true, sensitivity: 1 };

beforeEach(() => {
  vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
  vi.stubGlobal("cancelAnimationFrame", vi.fn());
});
afterEach(() => {
  vi.unstubAllGlobals();
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
});

describe("mouse speed in setupCapture", () => {
  it("matching on, stream twice the window: movement is doubled", () => {
    const { cap, sent } = start(video(1920, 960), MATCH);
    move(5, -3);
    expect(sent).toEqual([{ dx: 10, dy: -6 }]);
    cap.cleanup();
  });

  it("matching off: movement is sent as it came", () => {
    const { cap, sent } = start(video(1920, 960), { match: false, sensitivity: 1 });
    move(5, -3);
    expect(sent).toEqual([{ dx: 5, dy: -3 }]);
    cap.cleanup();
  });

  it("window equal to the stream: no change, even with matching on", () => {
    const { cap, sent } = start(video(1920, 1920), MATCH);
    move(5, -3);
    move(0.5, 0);
    expect(sent).toEqual([{ dx: 5, dy: -3 }, { dx: 0.5, dy: 0 }]);
    cap.cleanup();
  });

  it("sensitivity multiplies after matching", () => {
    const { cap, sent } = start(video(1920, 960), { match: true, sensitivity: 2 });
    move(1);
    expect(sent).toEqual([{ dx: 4, dy: 0 }]);
    cap.cleanup();
  });

  it("keeps the fractional remainder across events", () => {
    const { cap, sent } = start(video(1920, 1920), { match: false, sensitivity: 0.25 });
    for (let i = 0; i < 8; i++) move(1);
    expect(sent.reduce((a, m) => a + m.dx, 0)).toBe(2);
    cap.cleanup();
  });

  it("coalesced samples are each scaled once, and the summary event is not added on top", () => {
    const { cap, sent } = start(video(1920, 960), MATCH);
    const e = new Event("pointermove") as Event & { movementX: number; movementY: number; getCoalescedEvents: () => unknown[] };
    e.movementX = 3;
    e.movementY = 0;
    e.getCoalescedEvents = () => [{ movementX: 1, movementY: 0 }, { movementX: 2, movementY: 0 }];
    document.dispatchEvent(e);
    expect(sent).toEqual([{ dx: 2, dy: 0 }, { dx: 4, dy: 0 }]);
    cap.cleanup();
  });

  it("follows a window resize", () => {
    const el = video(1920, 960);
    const { cap, sent } = start(el, MATCH);
    Object.defineProperty(el, "clientWidth", { value: 1920, configurable: true });
    Object.defineProperty(el, "clientHeight", { value: 1080, configurable: true });
    el.dispatchEvent(new Event("resize"));
    move(5);
    expect(sent).toEqual([{ dx: 5, dy: 0 }]);
    cap.cleanup();
  });

  it("setMouse changes the speed of the running capture", () => {
    const { cap, sent } = start(video(1920, 960), { match: false, sensitivity: 1 });
    cap.setMouse(MATCH);
    move(2);
    expect(sent).toEqual([{ dx: 4, dy: 0 }]);
    cap.cleanup();
  });
});
