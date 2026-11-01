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

const MATCH = { match: true, sensitivity: 1, raw: false, flip: false, nintendo: false };

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
    const { cap, sent } = start(video(1920, 960), { match: false, sensitivity: 1, raw: false, flip: false, nintendo: false });
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
    const { cap, sent } = start(video(1920, 960), { match: true, sensitivity: 2, raw: false, flip: false, nintendo: false });
    move(1);
    expect(sent).toEqual([{ dx: 4, dy: 0 }]);
    cap.cleanup();
  });

  it("keeps the fractional remainder across events", () => {
    const { cap, sent } = start(video(1920, 1920), { match: false, sensitivity: 0.25, raw: false, flip: false, nintendo: false });
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

  it("follows a stream-size change (an ABR step): the video resize event", () => {
    const el = video(1920, 960);
    const { cap, sent } = start(el, MATCH);
    Object.defineProperty(el, "videoWidth", { value: 960, configurable: true });
    Object.defineProperty(el, "videoHeight", { value: 540, configurable: true });
    el.dispatchEvent(new Event("resize"));
    move(5);
    expect(sent).toEqual([{ dx: 5, dy: 0 }]);
    cap.cleanup();
  });

  it("stream below the compositor: the cursor gain is compositor pixels per shown pixel", () => {
    // 2560x1440 compositor, 1920x1080 stream shown 1:1; the host must move 4/3 per browser pixel.
    const el = video(1920, 1920);
    el.dataset.compositorW = "2560";
    el.dataset.compositorH = "1440";
    const { cap, sent } = start(el, MATCH);
    move(3);
    move(3);
    move(3);
    expect(sent).toEqual([{ dx: 4, dy: 0 }, { dx: 4, dy: 0 }, { dx: 4, dy: 0 }]);
    cap.cleanup();
  });

  it("an ABR rung step mid-session keeps the cursor under the pointer", () => {
    // Compositor 1920x1080 and a 1920x1080 window: gain 1, then the stream steps to 1280x720.
    const el = video(1920, 1920);
    el.dataset.compositorW = "1920";
    el.dataset.compositorH = "1080";
    const { cap, sent } = start(el, MATCH);
    move(6);
    Object.defineProperty(el, "videoWidth", { value: 1280, configurable: true });
    Object.defineProperty(el, "videoHeight", { value: 720, configurable: true });
    el.dispatchEvent(new Event("resize"));
    move(6);
    expect(sent).toEqual([{ dx: 6, dy: 0 }, { dx: 6, dy: 0 }]);
    cap.cleanup();
  });

  it("follows a compositor-size change (a render resolution change)", async () => {
    const el = video(1920, 1920);
    const { cap, sent } = start(el, MATCH);
    move(6);
    el.dataset.compositorW = "1280";
    el.dataset.compositorH = "720";
    await new Promise((r) => setTimeout(r));
    move(6);
    expect(sent).toEqual([{ dx: 6, dy: 0 }, { dx: 4, dy: 0 }]);
    cap.cleanup();
  });

  it("follows a scaling-mode change, which keeps the box the same size", async () => {
    // 1920x1080 in a 1440x900 box: contain draws at 0.75, cover at 0.8333.
    const el = video(1920, 1440, 1080, 900);
    const { cap, sent } = start(el, MATCH);
    move(100);
    el.dataset.scalingMode = "cover";
    await new Promise((r) => setTimeout(r));
    move(100);
    el.dataset.scalingMode = "stretch";
    await new Promise((r) => setTimeout(r));
    move(100, 100);
    expect(sent).toEqual([{ dx: 133, dy: 0 }, { dx: 120, dy: 0 }, { dx: 133, dy: 120 }]);
    cap.cleanup();
  });

  it("setMouse changes the speed of the running capture", () => {
    const { cap, sent } = start(video(1920, 960), { match: false, sensitivity: 1, raw: false, flip: false, nintendo: false });
    cap.setMouse(MATCH);
    move(2);
    expect(sent).toEqual([{ dx: 4, dy: 0 }]);
    cap.cleanup();
  });
});

describe("flip scroll in setupCapture", () => {
  const NEUTRAL = { match: false, sensitivity: 1, raw: false, flip: false, nintendo: false };
  /** Wire text of every `ms` sent (the message object is reused, so copy at send time). */
  function startWheel(mouse?: MouseSettings) {
    const wire: string[] = [];
    const el = video(1920, 1920);
    const cap = setupCapture({
      videoEl: el,
      sendInput: (m) => {
        if (m.t === "ms") wire.push(JSON.stringify(m));
      },
      onCaptureChange: () => {},
      mouse,
    });
    Object.defineProperty(document, "pointerLockElement", { value: el, configurable: true });
    document.dispatchEvent(new Event("pointerlockchange"));
    return { cap, wire };
  }
  const wheel = (deltaX: number, deltaY: number) => document.dispatchEvent(new WheelEvent("wheel", { deltaX, deltaY }));

  it("off (and the default): the wheel is sent exactly as it came", () => {
    for (const mouse of [NEUTRAL, undefined]) {
      const { cap, wire } = startWheel(mouse);
      wheel(0, 120);
      wheel(-40, -3.5);
      expect(wire).toEqual(['{"t":"ms","dx":0,"dy":120}', '{"t":"ms","dx":-40,"dy":-3.5}']);
      cap.cleanup();
    }
  });

  it("on: both axes change sign", () => {
    const { cap, wire } = startWheel({ ...NEUTRAL, flip: true });
    wheel(0, 120);
    wheel(-40, -3.5);
    expect(wire).toEqual(['{"t":"ms","dx":0,"dy":-120}', '{"t":"ms","dx":40,"dy":3.5}']);
    cap.cleanup();
  });

  it("setMouse flips a running capture and flips it back", () => {
    const { cap, wire } = startWheel();
    cap.setMouse({ ...NEUTRAL, flip: true });
    wheel(0, 100);
    cap.setMouse(NEUTRAL);
    wheel(0, 100);
    expect(wire).toEqual(['{"t":"ms","dx":0,"dy":-100}', '{"t":"ms","dx":0,"dy":100}']);
    cap.cleanup();
  });
});
