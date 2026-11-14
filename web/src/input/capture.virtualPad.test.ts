// The on-screen pad through the capture engine (#1401): asserts on the sendInput sink.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setupCapture } from "./capture";
import { BTN, neutralPad, setButton } from "./virtualPad";

type Msg = Record<string, unknown>;

class FakePointerEvent extends Event {
  pointerId: number;
  pointerType: string;
  clientX: number;
  clientY: number;
  constructor(type: string, init: EventInit & { pointerId?: number; pointerType?: string; clientX?: number; clientY?: number } = {}) {
    super(type, { bubbles: true, cancelable: true, ...init });
    this.pointerId = init.pointerId ?? 1;
    this.pointerType = init.pointerType ?? "touch";
    this.clientX = init.clientX ?? 0;
    this.clientY = init.clientY ?? 0;
  }
}

let video: HTMLVideoElement;
let overlay: HTMLButtonElement;
let sent: Msg[];
let poll: (() => void) | null;
let harness: ReturnType<typeof setupCapture>;
let pads: unknown[];

const gp = () => sent.filter((m) => m.t === "gp");
const pressA = () => setButton(neutralPad(), BTN.A, true);

beforeEach(async () => {
  vi.useFakeTimers();
  (globalThis as unknown as { PointerEvent: typeof FakePointerEvent }).PointerEvent = FakePointerEvent;
  delete (Element.prototype as unknown as Record<string, unknown>).requestPointerLock;
  Object.defineProperty(document, "pointerLockElement", { value: null, configurable: true });
  poll = null;
  vi.stubGlobal("requestAnimationFrame", (cb: () => void) => {
    poll = cb;
    return 1;
  });
  vi.stubGlobal("cancelAnimationFrame", () => {});
  pads = [];
  Object.defineProperty(navigator, "getGamepads", { value: () => pads, configurable: true });

  video = document.createElement("video");
  overlay = document.createElement("button");
  document.body.append(video, overlay);
  sent = [];
  harness = setupCapture({
    videoEl: video,
    sendInput: (m) => sent.push({ ...m }),
    onCaptureChange: () => {},
  });
  await harness.engage();
});

afterEach(() => {
  harness.cleanup();
  document.body.innerHTML = "";
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("setVirtualPad", () => {
  it("sends the state through the gp path, on change only", () => {
    harness.setVirtualPad(pressA());
    poll!();
    expect(gp()).toEqual([{ t: "gp", i: 0, buttons: pressA().buttons, axes: [0, 0, 0, 0] }]);
    poll!();
    expect(gp()).toHaveLength(1);
    harness.setVirtualPad(neutralPad());
    poll!();
    expect(gp()).toHaveLength(2);
  });

  it("sends nothing while capture is released", async () => {
    harness.release();
    sent.length = 0;
    harness.setVirtualPad(pressA());
    poll!();
    expect(gp()).toEqual([]);
  });

  it("null leaves nothing held: one neutral state, and nothing again", () => {
    harness.setVirtualPad(pressA());
    poll!();
    sent.length = 0;
    harness.setVirtualPad(null);
    expect(gp()).toEqual([{ t: "gp", i: 0, buttons: neutralPad().buttons, axes: [0, 0, 0, 0] }]);
    harness.setVirtualPad(null);
    poll!();
    expect(gp()).toHaveLength(1);
  });

  it("releasing capture sends the neutral state, and re-capture resends the held one", async () => {
    harness.setVirtualPad(pressA());
    poll!();
    sent.length = 0;
    harness.release();
    expect(gp()).toEqual([{ t: "gp", i: 0, buttons: neutralPad().buttons, axes: [0, 0, 0, 0] }]);
    await harness.engage();
    sent.length = 0;
    poll!();
    expect(gp()[0]?.buttons).toEqual(pressA().buttons);
  });

  it("cleanup sends the neutral state", () => {
    harness.setVirtualPad(pressA());
    poll!();
    sent.length = 0;
    harness.cleanup();
    expect(gp()).toEqual([{ t: "gp", i: 0, buttons: neutralPad().buttons, axes: [0, 0, 0, 0] }]);
  });

  it("is never sent beside a real pad: neutral first, then the real pad's full state", () => {
    harness.setVirtualPad(pressA());
    poll!();
    sent.length = 0;
    pads = [{ index: 0, id: "pad", mapping: "standard", buttons: [{ value: 1 }], axes: [0.5] }];
    poll!();
    expect(gp()).toEqual([
      { t: "gp", i: 0, buttons: neutralPad().buttons, axes: [0, 0, 0, 0] },
      { t: "gp", i: 0, buttons: [1], axes: [0.5] },
    ]);
    sent.length = 0;
    poll!();
    expect(gp()).toEqual([]);
  });

  it("the same real pad returning after the virtual one is sent in full again (gpPrev cleared)", () => {
    const real = { index: 0, id: "pad", mapping: "standard", buttons: [{ value: 1 }], axes: [0.5] };
    pads = [real];
    poll!();
    pads = [];
    harness.setVirtualPad(pressA());
    poll!();
    sent.length = 0;
    pads = [real]; // unchanged since it was last sent: only a cleared gpPrev resends it
    poll!();
    expect(gp()).toEqual([
      { t: "gp", i: 0, buttons: neutralPad().buttons, axes: [0, 0, 0, 0] },
      { t: "gp", i: 0, buttons: [1], axes: [0.5] },
    ]);
  });
});

describe("touches on the overlay (regression guards)", () => {
  it("never reach the touch layer: no mouse input, with or without moving", () => {
    overlay.dispatchEvent(new FakePointerEvent("pointerdown", { clientX: 10, clientY: 10 }));
    document.dispatchEvent(new FakePointerEvent("pointermove", { clientX: 200, clientY: 10 }));
    vi.advanceTimersByTime(2000); // past the long-press timer
    document.dispatchEvent(new FakePointerEvent("pointerup", { clientX: 200, clientY: 10 }));
    expect(sent.filter((m) => m.t !== "gp")).toEqual([]);
  });

  it("while a touch on the picture still looks around", () => {
    video.dispatchEvent(new FakePointerEvent("pointerdown", { clientX: 10, clientY: 10 }));
    document.dispatchEvent(new FakePointerEvent("pointermove", { clientX: 60, clientY: 10 }));
    expect(sent.some((m) => m.t === "mm")).toBe(true);
  });
});
