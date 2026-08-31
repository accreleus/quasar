// Shelf section 2 — Controller & input (handoff §E.2).
//
// The Esc wording is load-bearing (quasar#376): over plain HTTP the Keyboard
// Lock API doesn't exist, so Esc stays browser-owned and releases capture no
// matter what the app wants. One sentence per case; the mock's copy is the
// fullscreen one. Two of the cases are invisible to feature detection — a
// bypassed certificate (lib/certTrust.ts) and an observed lock() refusal both
// leave the API reading as "supported" — so they take wording precedence.
//
// The gamepad readout is telemetry, so it lives in a memoized child subscribing
// via `register` (#139) — its re-renders never reach the rest of the pane.

import { memo, useEffect, useState } from "react";
import { SCALING_MODES, type ScalingMode } from "../../../../settings/displayPreferences";
import { EMPTY_SNAPSHOT, type TelemetrySnapshot } from "../../../../webrtc/telemetry";
import { IconGamepadLarge } from "../icons";
import type { TelemetryRegistrar } from "../HudBar";

export interface InputPaneProps {
  register: TelemetryRegistrar;
  /** True once the input DataChannel is open (capture becomes available). */
  channelOpen: boolean;
  /** Capture engaged. not Pointer Lock: a browser without it still captures. */
  inputCaptured: boolean;
  /** Toggle — engages when released, releases when captured. */
  onGrab: () => void;
  /** True when Esc is browser-owned (or ours) and releases capture. */
  escReleases: boolean;
  /** True when that is specifically because Keyboard Lock is unavailable. */
  escInsecureContext: boolean;
  /** Certificate bypassed rather than trusted (lib/certTrust.ts). Takes wording
   *  precedence over the other Esc explanations: "use HTTPS" is wrong advice on
   *  an origin that already IS https. */
  escCertUntrusted?: boolean;
  /** keyboard.lock() was observed refused this session — the API still reads as
   *  supported, yet Esc quits. */
  escLockRefused?: boolean;
  /** False on a browser with no Element.requestPointerLock — no mouse look. */
  pointerLockAvailable: boolean;
  /** True where touch gestures are the mouse (touchscreen + no Pointer Lock). */
  touchLook: boolean;
  scalingMode: ScalingMode;
  onScalingChange: (m: ScalingMode) => void;
}

/** Segment labels for the CSS-only presentation modes. Four, not the mock's
 *  three: `stretch` exists today and dropping a segment would drop a control. */
const SCALING_LABELS: Record<ScalingMode, string> = {
  contain: "Fit",
  cover: "Fill",
  stretch: "Stretch",
  integer: "1:1",
};

/**
 * Shortens a gamepad's raw `id` to fit this column. Chromium formats ids as
 * `"<name> (STANDARD GAMEPAD Vendor: ... Product: ...)"` — strip the
 * parenthesized suffix, keep the device name. Ids without a trailing
 * parenthetical pass through, capped to length. The full id stays in `title`.
 */
export function shortGamepadLabel(id: string): string {
  const parenIdx = id.indexOf(" (");
  const base = (parenIdx === -1 ? id : id.slice(0, parenIdx)).trim();
  const label = base || id.trim() || "Unknown controller";
  return label.length > 40 ? `${label.slice(0, 39)}…` : label;
}

/**
 * Isolated telemetry subtree: gamepad count + per-pad identity. `pads` and
 * `gamepadCount` come from the same snapshot, so they can never disagree.
 */
const Gamepads = memo(function Gamepads({ register }: { register: TelemetryRegistrar }) {
  const [snap, setSnap] = useState<TelemetrySnapshot>(EMPTY_SNAPSHOT);
  useEffect(() => register(setSnap), [register]);
  const count = snap.inputMetrics?.gamepadCount ?? 0;
  const pads = snap.inputMetrics?.pads ?? [];
  return (
    <>
      <div className="ov-kv">
        <span>Gamepads</span>
        <b className="mono">{count}</b>
      </div>
      {pads.map((pad) => (
        <div className="ov-kv" key={pad.index}>
          <span>Slot {pad.index + 1}</span>
          <b title={pad.id}>{shortGamepadLabel(pad.id)}</b>
        </div>
      ))}
      {count === 0 && (
        <div className="col-note">
          No controller seen yet. Press a button on an idle pad — browsers only report a
          gamepad after its first input.
        </div>
      )}
    </>
  );
});

/** The one sentence that explains what Esc does here, and why. */
function escNote(props: InputPaneProps): string {
  if (props.touchLook) {
    return (
      "Touch is your mouse here: drag anywhere on the picture to look, tap to click, " +
      "press and hold for a right-click, and drag two fingers to scroll. Your keyboard " +
      "and controller are captured too. Tap the session-menu button to stop — it stays " +
      "on screen the whole time you're playing."
    );
  }
  if (!props.pointerLockAvailable) {
    return (
      "This browser has no mouse capture (Pointer Lock) and no touchscreen, so relative " +
      "mouse look isn’t possible here — but capture still sends your keyboard, controller, " +
      "clicks and scrolling to the game. Press Esc, use the release combo, or tap the " +
      "session-menu button to stop."
    );
  }
  if (props.escCertUntrusted) {
    return (
      "In-game Esc capture is unreliable here: this address uses a certificate your device " +
      "doesn’t trust (you clicked through a certificate warning to get in). Download the " +
      "server certificate from /v1/tls/certificate.pem, trust it on this device, then reload."
    );
  }
  if (props.escLockRefused) {
    return (
      "The browser refused to hand Esc to the game (Keyboard Lock), so Esc releases capture " +
      "instead of reaching the game. Re-entering fullscreen usually restores it."
    );
  }
  if (props.escInsecureContext) {
    return (
      "In-game Esc capture needs the HTTPS address — Keyboard Lock is unavailable on this " +
      "insecure origin, so the browser keeps Esc and it releases capture."
    );
  }
  return (
    "Fullscreen and a secure origin let Quasar forward Esc to the game. Over plain HTTP, " +
    "or outside fullscreen, the browser keeps it and capture releases instead."
  );
}

export function InputPane(props: InputPaneProps) {
  return (
    <>
      <div className="pane-head">
        <h3>Controller &amp; input</h3>
        <p>
          Controllers are forwarded while input is captured. Press a button on an idle pad to
          wake it.
        </p>
      </div>
      <div className="cols">
        <div>
          <button
            type="button"
            className="capture-cta"
            onClick={props.onGrab}
            disabled={!props.channelOpen}
            aria-label={props.inputCaptured ? "Release input" : "Capture input"}
          >
            <IconGamepadLarge />
            <span>{props.inputCaptured ? "Release input" : "Capture input"}</span>
          </button>
          <div className="ov-kv">
            <span>Release capture</span>
            <span className="combo">
              <kbd>Ctrl</kbd>
              <kbd>Alt</kbd>
              <kbd>⇧</kbd>
              <kbd>Z</kbd>
            </span>
          </div>
          <div className="ov-kv">
            <span>Mouse look</span>
            <b>
              {props.pointerLockAvailable
                ? "Available"
                : props.touchLook
                  ? "Drag on the picture"
                  : "Not on this device"}
            </b>
          </div>
        </div>

        <div>
          <div className="col-lb">Keys</div>
          <div className="ov-kv">
            <span>Esc key</span>
            <b>{props.escReleases ? "Releases capture" : "Sent to the app"}</b>
          </div>
          {/* Gesture reference — only where the gestures are actually live; a
              desktop cannot produce them. */}
          {props.touchLook && (
            <>
              <div className="ov-kv">
                <span>Click</span>
                <b>Tap</b>
              </div>
              <div className="ov-kv">
                <span>Right-click</span>
                <b>Press and hold</b>
              </div>
              <div className="ov-kv">
                <span>Scroll</span>
                <b>Drag two fingers</b>
              </div>
            </>
          )}
          <div className="col-note">{escNote(props)}</div>
        </div>

        <div>
          <div className="col-lb">Connected</div>
          <Gamepads register={props.register} />
          <div className="ctl-row">
            Scaling
            <div className="segmented" role="tablist" aria-label="Scaling mode">
              {SCALING_MODES.map((mode) => (
                <button
                  key={mode}
                  type="button"
                  role="tab"
                  aria-selected={props.scalingMode === mode}
                  onClick={() => props.onScalingChange(mode)}
                >
                  {SCALING_LABELS[mode]}
                </button>
              ))}
            </div>
          </div>
        </div>
      </div>
    </>
  );
}
