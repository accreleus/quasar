// The on-screen controls' pad: W3C Standard Gamepad state (17 buttons, 4 axes)
// plus the pure maths that turns a finger into it. capture.ts sends the state
// through the same `gp` path as a real pad; nothing here touches the DOM.

export const PAD_BUTTONS = 17;
export const PAD_AXES = 4;

/** Standard Gamepad button indices. */
export const BTN = {
  A: 0, B: 1, X: 2, Y: 3, LB: 4, RB: 5, LT: 6, RT: 7,
  SELECT: 8, START: 9, UP: 12, DOWN: 13, LEFT: 14, RIGHT: 15,
} as const;

/** First axis of each stick: left = axes 0,1, right = axes 2,3. */
export const STICK_LEFT = 0;
export const STICK_RIGHT = 2;

export interface VirtualPadState {
  buttons: number[];
  axes: number[];
}

export const neutralPad = (): VirtualPadState => ({
  buttons: new Array<number>(PAD_BUTTONS).fill(0),
  axes: new Array<number>(PAD_AXES).fill(0),
});

/** Returns `pad` itself when nothing changes, a new state otherwise. */
export function setButton(pad: VirtualPadState, index: number, down: boolean): VirtualPadState {
  const v = down ? 1 : 0;
  if (pad.buttons[index] === v) return pad;
  const buttons = pad.buttons.slice();
  buttons[index] = v;
  return { buttons, axes: pad.axes };
}

/** Returns `pad` itself when nothing changes, a new state otherwise. */
export function setStick(pad: VirtualPadState, first: number, x: number, y: number): VirtualPadState {
  if (pad.axes[first] === x && pad.axes[first + 1] === y) return pad;
  const axes = pad.axes.slice();
  axes[first] = x;
  axes[first + 1] = y;
  return { buttons: pad.buttons, axes };
}

export const STICK_DEAD_ZONE = 0.15;

/** Finger offset from the stick centre → axis pair in -1..1. The thumb is
 *  clamped to the circle; inside the dead zone the stick reads zero, outside it
 *  the range is rescaled so the output starts from 0 at the edge. y grows down,
 *  as the standard layout wants. */
export function stickFromOffset(
  dx: number,
  dy: number,
  radius: number,
  deadZone = STICK_DEAD_ZONE,
): { x: number; y: number } {
  const dist = Math.hypot(dx, dy);
  if (radius <= 0 || dist === 0) return { x: 0, y: 0 };
  const m = Math.min(dist / radius, 1);
  if (m < deadZone) return { x: 0, y: 0 };
  const scaled = (m - deadZone) / (1 - deadZone);
  return { x: (dx / dist) * scaled, y: (dy / dist) * scaled };
}

/** Same clamp for drawing the knob, in px. */
export function clampToRadius(dx: number, dy: number, radius: number): { x: number; y: number } {
  const dist = Math.hypot(dx, dy);
  if (dist <= radius || dist === 0) return { x: dx, y: dy };
  return { x: (dx / dist) * radius, y: (dy / dist) * radius };
}

/** Finger offset from the d-pad centre → the directions held. Diagonals are two
 *  directions at once. */
export function dpadFromOffset(dx: number, dy: number, radius: number) {
  const t = radius * 0.3;
  return { up: dy < -t, down: dy > t, left: dx < -t, right: dx > t };
}
