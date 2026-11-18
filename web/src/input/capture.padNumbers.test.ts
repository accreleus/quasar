// Controller numbering through the capture engine (amendment 80, #1428): the `gp.i` the host
// sees is a dense number, never the browser's sparse `Gamepad.index`.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setupCapture } from "./capture";

type Msg = Record<string, unknown>;

let video: HTMLVideoElement;
let sent: Msg[];
let poll: (() => void) | null;
let harness: ReturnType<typeof setupCapture>;
let pads: (unknown | null)[];

const pad = (index: number, value = 1) => ({ index, id: `pad${index}`, mapping: "standard", buttons: [{ value }], axes: [0] });
const gps = () => sent.filter((m) => m.t === "gp");
const tick = () => poll?.();

beforeEach(async () => {
  vi.useFakeTimers();
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
  document.body.append(video);
  sent = [];
  harness = setupCapture({ videoEl: video, sendInput: (m) => sent.push({ ...m }), onCaptureChange: () => {} });
  await harness.engage();
});

afterEach(() => {
  harness.cleanup();
  document.body.innerHTML = "";
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("controller numbers", () => {
  it("numbers a lone controller 0 whatever slot the browser gave it", () => {
    pads = [null, pad(1)];
    tick();
    expect(gps()).toEqual([{ t: "gp", i: 0, buttons: [1], axes: [0] }]);
  });

  it("numbers controllers in the order they connect, and keeps each number while it stays", () => {
    pads = [null, pad(1)];
    tick();
    pads = [pad(0), pad(1)]; // a second controller arrives in the lower slot
    tick();
    expect(gps().map((m) => m.i)).toEqual([0, 1]);
    pads = [pad(0, 0), pad(1)]; // slot 0 changes: it is number 1, slot 1 keeps number 0
    tick();
    expect(gps().at(-1)).toEqual({ t: "gp", i: 1, buttons: [0], axes: [0] });
  });

  it("rests a disconnected controller's number, moves the one above it down, and hands the freed top number to the next", () => {
    pads = [pad(0), pad(1)];
    tick(); // slot 0 = 0, slot 1 = 1
    sent.length = 0;
    pads = [null, pad(1)]; // number 0 leaves: the survivor is a lone controller, so it is 0
    tick();
    expect(gps()).toEqual([
      { t: "gp", i: 0, buttons: [0], axes: [0] }, // 0 rests
      { t: "gp", i: 1, buttons: [0], axes: [0] }, // the mover rests its old number
      { t: "gp", i: 0, buttons: [1], axes: [0] }, // then it is sent as 0
    ]);
    sent.length = 0;
    pads = [null, pad(1), pad(2)]; // a new controller is number 1
    tick();
    expect(gps()).toEqual([{ t: "gp", i: 1, buttons: [1], axes: [0] }]);
  });

  it("keeps three controllers dense when the middle one is unplugged", () => {
    pads = [pad(0), pad(1), pad(2)];
    tick();
    sent.length = 0;
    pads = [pad(0), null, pad(2)];
    tick();
    // The middle number and the one above rest; the third controller becomes 1; the first,
    // below the gap, is untouched.
    expect(gps()).toEqual([
      { t: "gp", i: 1, buttons: [0], axes: [0] },
      { t: "gp", i: 2, buttons: [0], axes: [0] },
      { t: "gp", i: 1, buttons: [1], axes: [0] },
    ]);
    sent.length = 0;
    pads = [null, null, pad(2)]; // the first leaves too: the last one standing is 0
    tick();
    expect(gps().at(-1)).toEqual({ t: "gp", i: 0, buttons: [1], axes: [0] });
    expect(gps().map((m) => m.i)).toEqual([0, 1, 0]);
  });

  it("sends no controller past number 3", () => {
    pads = [pad(0), pad(1), pad(2), pad(3), pad(4)];
    tick();
    expect(gps().map((m) => m.i)).toEqual([0, 1, 2, 3]);
  });
});
