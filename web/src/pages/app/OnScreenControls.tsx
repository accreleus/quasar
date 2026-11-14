// #1401: a touch pad over the picture — one standard gamepad, sent through the
// capture engine's setVirtualPad. Mounted only while input is captured on a
// touch device; unmounting (or a real controller showing up) leaves nothing held.
// Every control owns its pointer (setPointerCapture), so several fingers work at
// once and a finger that slides off a button still releases it.

import { memo, useCallback, useEffect, useRef, useState, type PointerEvent as ReactPointerEvent, type ReactNode } from "react";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../webrtc/telemetry";
import type { TelemetryRegistrar } from "./hud/HudBar";
import {
  BTN,
  STICK_LEFT,
  STICK_RIGHT,
  clampToRadius,
  dpadFromOffset,
  neutralPad,
  setButton,
  setStick,
  stickFromOffset,
  type VirtualPadState,
} from "../../input/virtualPad";

type Update = (fn: (pad: VirtualPadState) => VirtualPadState) => void;

const own = (e: ReactPointerEvent<HTMLElement>) => {
  e.preventDefault();
  e.currentTarget.setPointerCapture?.(e.pointerId);
};

/** One finger per control: pointer handlers shared by the button, stick and d-pad. */
function usePointerControl(onMove: (e: ReactPointerEvent<HTMLElement>) => void, onEnd: () => void) {
  const active = useRef<number | null>(null);
  return {
    onPointerDown(e: ReactPointerEvent<HTMLElement>) {
      if (active.current !== null) return;
      active.current = e.pointerId;
      own(e);
      onMove(e);
    },
    onPointerMove(e: ReactPointerEvent<HTMLElement>) {
      if (e.pointerId === active.current) onMove(e);
    },
    onPointerUp(e: ReactPointerEvent<HTMLElement>) {
      if (e.pointerId !== active.current) return;
      active.current = null;
      onEnd();
    },
    onContextMenu: (e: { preventDefault(): void }) => e.preventDefault(),
  };
}

function PadButton({ index, label, cls, update, children }: {
  index: number;
  label: string;
  cls: string;
  update: Update;
  children?: ReactNode;
}) {
  const [down, setDown] = useState(false);
  const set = useCallback((on: boolean) => {
    setDown(on);
    update((p) => setButton(p, index, on));
  }, [index, update]);
  const h = usePointerControl(() => set(true), () => set(false));
  return (
    <button
      type="button"
      className={`osc-btn ${cls}`}
      aria-label={label}
      aria-pressed={down}
      data-down={down || undefined}
      {...h}
      onPointerCancel={h.onPointerUp}
      onLostPointerCapture={h.onPointerUp}
    >
      {children ?? label}
    </button>
  );
}

function Stick({ first, label, update }: { first: number; label: string; update: Update }) {
  const knob = useRef<HTMLDivElement>(null);
  const move = (e: ReactPointerEvent<HTMLElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const radius = r.width / 2;
    const dx = e.clientX - (r.left + radius);
    const dy = e.clientY - (r.top + r.height / 2);
    const { x, y } = stickFromOffset(dx, dy, radius);
    const k = clampToRadius(dx, dy, radius);
    knob.current?.style.setProperty("transform", `translate(${k.x}px, ${k.y}px)`);
    update((p) => setStick(p, first, x, y));
  };
  const end = () => {
    knob.current?.style.removeProperty("transform");
    update((p) => setStick(p, first, 0, 0));
  };
  const h = usePointerControl(move, end);
  return (
    <div className="osc-stick" role="group" aria-label={label} {...h} onPointerCancel={h.onPointerUp} onLostPointerCapture={h.onPointerUp}>
      <div className="osc-knob" ref={knob} />
    </div>
  );
}

const DIRS = [
  ["up", BTN.UP, "Up", "osc-up"],
  ["down", BTN.DOWN, "Down", "osc-down"],
  ["left", BTN.LEFT, "Left", "osc-left"],
  ["right", BTN.RIGHT, "Right", "osc-right"],
] as const;

function Dpad({ update }: { update: Update }) {
  const [held, setHeld] = useState({ up: false, down: false, left: false, right: false });
  const apply = (d: typeof held) => {
    setHeld(d);
    update((p) => DIRS.reduce((acc, [k, idx]) => setButton(acc, idx, d[k]), p));
  };
  const h = usePointerControl(
    (e) => {
      const r = e.currentTarget.getBoundingClientRect();
      apply(dpadFromOffset(e.clientX - (r.left + r.width / 2), e.clientY - (r.top + r.height / 2), r.width / 2));
    },
    () => apply({ up: false, down: false, left: false, right: false }),
  );
  return (
    <div className="osc-dpad" role="group" aria-label="D-pad" {...h} onPointerCancel={h.onPointerUp} onLostPointerCapture={h.onPointerUp}>
      {DIRS.map(([k, , name, cls]) => (
        <span key={k} className={`osc-dir ${cls}`} data-down={held[k] || undefined} aria-label={name} role="img" />
      ))}
    </div>
  );
}

function Controls({ setPad }: { setPad: (pad: VirtualPadState | null) => void }) {
  const pad = useRef(neutralPad());
  const update = useCallback<Update>((fn) => {
    const next = fn(pad.current);
    if (next === pad.current) return;
    pad.current = next;
    setPad(next);
  }, [setPad]);
  // Unmount (capture released, switch off, real pad connected): the engine sends the neutral state.
  useEffect(() => () => setPad(null), [setPad]);

  return (
    <div className="osc" data-testid="on-screen-controls">
      <div className="osc-cluster osc-cluster-l">
        <div className="osc-shoulder">
          <PadButton index={BTN.LT} label="LT" cls="osc-trigger" update={update} />
          <PadButton index={BTN.LB} label="LB" cls="osc-trigger" update={update} />
        </div>
        <Dpad update={update} />
        <Stick first={STICK_LEFT} label="Left stick" update={update} />
      </div>
      <div className="osc-cluster osc-cluster-r">
        <div className="osc-shoulder">
          <PadButton index={BTN.RB} label="RB" cls="osc-trigger" update={update} />
          <PadButton index={BTN.RT} label="RT" cls="osc-trigger" update={update} />
        </div>
        <div className="osc-face">
          <PadButton index={BTN.Y} label="Y" cls="osc-face-btn osc-y" update={update} />
          <PadButton index={BTN.X} label="X" cls="osc-face-btn osc-x" update={update} />
          <PadButton index={BTN.B} label="B" cls="osc-face-btn osc-b" update={update} />
          <PadButton index={BTN.A} label="A" cls="osc-face-btn osc-a" update={update} />
        </div>
        <Stick first={STICK_RIGHT} label="Right stick" update={update} />
      </div>
      <div className="osc-meta">
        <PadButton index={BTN.SELECT} label="Select" cls="osc-pill" update={update} />
        <PadButton index={BTN.START} label="Start" cls="osc-pill" update={update} />
      </div>
    </div>
  );
}

/** Hidden while a real controller is connected (the engine also refuses to send both). */
export const OnScreenControls = memo(function OnScreenControls({ register, setPad }: {
  register: TelemetryRegistrar;
  setPad: (pad: VirtualPadState | null) => void;
}) {
  const [snap, setSnap] = useState<TelemetrySnapshot>(EMPTY_SNAPSHOT);
  useEffect(() => register(setSnap), [register]);
  if ((snap.inputMetrics?.gamepadCount ?? 0) > 0) return null;
  return <Controls setPad={setPad} />;
});
