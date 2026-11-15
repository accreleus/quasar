import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OnScreenControls } from "./OnScreenControls";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../webrtc/telemetry";
import { BTN, type VirtualPadState } from "../../input/virtualPad";

class FakePointerEvent extends Event {
  pointerId: number;
  pointerType = "touch";
  clientX: number;
  clientY: number;
  constructor(type: string, init: EventInit & { pointerId?: number; clientX?: number; clientY?: number } = {}) {
    super(type, { bubbles: true, cancelable: true, ...init });
    this.pointerId = init.pointerId ?? 1;
    this.clientX = init.clientX ?? 0;
    this.clientY = init.clientY ?? 0;
  }
}
const ptr = (el: Element, type: string, id = 1, x = 0, y = 0) =>
  fireEvent(el, new FakePointerEvent(type, { pointerId: id, clientX: x, clientY: y }));

let setPad: ReturnType<typeof vi.fn<(p: VirtualPadState | null) => void>>;
const last = () => setPad.mock.calls.at(-1)![0];
const held = () => last()?.buttons.map((v, i) => (v ? i : -1)).filter((i) => i >= 0) ?? [];

function mount(gamepadCount = 0) {
  let push: (s: TelemetrySnapshot) => void = () => {};
  const register = (fn: (s: TelemetrySnapshot) => void) => {
    push = fn;
    return () => {};
  };
  const view = render(<OnScreenControls register={register} setPad={setPad} />);
  const withPads = (n: number) =>
    act(() => push({ ...EMPTY_SNAPSHOT, inputMetrics: { gamepadCount: n } as never }));
  if (gamepadCount) withPads(gamepadCount);
  return { ...view, withPads };
}

beforeEach(() => {
  (globalThis as unknown as { PointerEvent: typeof FakePointerEvent }).PointerEvent = FakePointerEvent;
  setPad = vi.fn();
});
afterEach(() => vi.unstubAllGlobals());

describe("OnScreenControls", () => {
  it("names every control", () => {
    mount();
    for (const name of ["A", "B", "X", "Y", "LB", "RB", "LT", "RT", "Start", "Select"]) {
      expect(screen.getByRole("button", { name })).toBeTruthy();
    }
    for (const name of ["Left stick", "Right stick", "D-pad"]) {
      expect(screen.getByRole("group", { name })).toBeTruthy();
    }
  });

  it("holds a button while the finger is down and releases it on pointerup", () => {
    mount();
    const a = screen.getByRole("button", { name: "A" });
    ptr(a, "pointerdown");
    expect(held()).toEqual([BTN.A]);
    ptr(a, "pointerup");
    expect(held()).toEqual([]);
  });

  it("holds two controls at once, each released by its own finger", () => {
    mount();
    const a = screen.getByRole("button", { name: "A" });
    const rt = screen.getByRole("button", { name: "RT" });
    ptr(a, "pointerdown", 1);
    ptr(rt, "pointerdown", 2);
    expect(held()).toEqual([BTN.A, BTN.RT]);
    ptr(a, "pointerup", 1);
    expect(held()).toEqual([BTN.RT]);
  });

  it("pointercancel and lost capture release too", () => {
    mount();
    const b = screen.getByRole("button", { name: "B" });
    ptr(b, "pointerdown");
    ptr(b, "pointercancel");
    expect(held()).toEqual([]);
    ptr(b, "pointerdown", 2);
    ptr(b, "lostpointercapture", 2);
    expect(held()).toEqual([]);
  });

  it("ignores a second finger on a held button", () => {
    mount();
    const a = screen.getByRole("button", { name: "A" });
    ptr(a, "pointerdown", 1);
    ptr(a, "pointerdown", 2);
    ptr(a, "pointerup", 2);
    expect(held()).toEqual([BTN.A]);
  });

  it("drives a stick from the thumb offset, clamped, and springs back to zero", () => {
    mount();
    const stick = screen.getByRole("group", { name: "Left stick" });
    stick.getBoundingClientRect = () => ({ left: 0, top: 0, width: 100, height: 100, right: 100, bottom: 100, x: 0, y: 0, toJSON() {} });
    ptr(stick, "pointerdown", 1, 100, 50); // 50 px right of centre = the rim
    expect(last()!.axes).toEqual([1, 0, 0, 0]);
    ptr(stick, "pointermove", 1, 500, 50); // dragged far past the rim
    expect(last()!.axes).toEqual([1, 0, 0, 0]);
    ptr(stick, "pointerup", 1, 500, 50);
    expect(last()!.axes).toEqual([0, 0, 0, 0]);
  });

  it("maps a d-pad diagonal to two buttons", () => {
    mount();
    const dpad = screen.getByRole("group", { name: "D-pad" });
    dpad.getBoundingClientRect = () => ({ left: 0, top: 0, width: 96, height: 96, right: 96, bottom: 96, x: 0, y: 0, toJSON() {} });
    ptr(dpad, "pointerdown", 1, 90, 6);
    expect(held()).toEqual([BTN.UP, BTN.RIGHT]);
    ptr(dpad, "pointermove", 1, 48, 90);
    expect(held()).toEqual([BTN.DOWN]);
    ptr(dpad, "pointerup", 1);
    expect(held()).toEqual([]);
  });

  it("clears the pad when it unmounts", () => {
    const { unmount } = mount();
    ptr(screen.getByRole("button", { name: "A" }), "pointerdown");
    unmount();
    expect(last()).toBeNull();
  });

  it("disappears, clearing the pad, when a real controller connects", () => {
    const { withPads } = mount();
    expect(screen.queryByTestId("on-screen-controls")).toBeTruthy();
    withPads(1);
    expect(screen.queryByTestId("on-screen-controls")).toBeNull();
    expect(last()).toBeNull();
  });
});
