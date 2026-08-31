// Input capture for the live session view (protocol/input.md — frozen shapes).
// setupCapture() once the DataChannel is open; cleanup() on unmount/close.
//
// Capture is not Pointer Lock. Two-valued state:
//   captured      — the ONLY send gate: keyboard, buttons, scroll, gamepad.
//   pointerLocked — the video element really holds Pointer Lock; relative mouse
//                   motion (`mm`) is gated on this and only this. No lock, no
//                   mouse-look — never faked. (Gating everything on the lock is
//                   what left an iPad — no requestPointerLock, ever — polling a
//                   gamepad it never forwarded.)
// Where the API exists Pointer Lock drives `captured` (engage() requests,
// pointerlockchange flips, Esc releases); where it doesn't, engage()/release()
// set `captured` directly ("fallback mode").
//
// Hot path (#139): `mm`/`ms` reuse one hoisted object each — no per-event heap
// allocation; wire format unchanged. AS10-13 CaptureMetrics are observational
// only — bufferedAmount is read for backpressure detection, the send path never
// skips on it. `?itrace=1` stamps optional seq/tc on each `mm` (additive per
// protocol/input.md; agent side gated on QUASAR_INPUT_TRACE).
//
// Touch gesture layer — the ONE place a finger becomes a wire message
// (`aimedAtGame` rejects touch-synthesized clicks so a tap can't double-send):
//   drag one finger -> `mm` look; tap -> `mb` 272 click; press-and-hold ->
//   `mb` 273 held until lift; drag two fingers -> `ms` scroll.
// Deltas are computed from clientX/Y, never movementX/Y (unreliable outside
// Pointer Lock — WebKit bug #167775 — and iPadOS has no lock at all).
// Gates: `captured`, then `!pointerLocked` (a real lock's mouse path would
// double-send), then the gesture must have started on the video element
// (matched by pointerId, so chrome taps are never seen here).

import { EVDEV, MB_MAP } from "./evdev";
import { isSummonCombo } from "./summonCombo";

// InputMsg matches the JSON shapes in protocol/input.md.
type InputMsg = Record<string, unknown>;

/** Whether per-`mm` send-side instrumentation is enabled (`?itrace=1`). */
const INPUT_TRACE =
  typeof URLSearchParams !== "undefined" &&
  new URLSearchParams(typeof location !== "undefined" ? location.search : "").get("itrace") === "1";

/** `index` is the W3C Gamepad.index (sparse slots). `id` is the raw vendor
 *  string, kept verbatim — the vendor/product ids are what dead-pad debugging
 *  needs; callers shorten for display. */
export interface GamepadIdentity {
  index: number;
  id: string;
}

/** AS10-13: snapshot of input-pipeline health, sampled each telemetry poll. */
export interface CaptureMetrics {
  pointerLocked: boolean;
  /** The send gate. Can be true with `pointerLocked === false` on a device with
   *  no Pointer Lock API (iPadOS Safari). */
  captured: boolean;
  pointerLockSupported: boolean;
  coalescedSupported: boolean;
  /** Input messages sent in the last window (per second). */
  inputMsgPerSec: number;
  /** Coalesced pointer samples/sec — a subset of inputMsgPerSec. */
  coalescedSamplesPerSec: number;
  channelBufferedAmount: number;
  /** bufferedAmount exceeded BACKPRESSURE_THRESHOLD_BYTES at least once this
   *  window. Early warning only — capture.ts never skips sends. */
  backpressureDetected: boolean;
  gamepadCount: number;
  /** From the same getGamepads() pass as gamepadCount, so they never disagree. */
  pads: GamepadIdentity[];
  gamepadSendPerSec: number;
  mmSentPerSec: number;
  /** `?itrace=1` seq/tc instrumentation active. */
  inputTrace: boolean;
}

/** The two-valued capture state surfaced to the UI on every transition. */
export interface CaptureState {
  captured: boolean;
  pointerLocked: boolean;
}

/**
 * What engage() achieved.
 * - `pointer-lock`: full capture including mouse-look.
 * - `fallback`:     no Pointer Lock API here; everything but mouse-look.
 * - `failed`:       the API exists but the request was refused (post-Esc
 *   cool-down, lost gesture). Capture is NOT engaged — the caller must let the
 *   user retry; a lock-capable device is never silently degraded.
 */
export type EngageMode = "pointer-lock" | "fallback" | "failed";

export interface EngageResult {
  mode: EngageMode;
  /** Present only for `failed` — whatever requestPointerLock threw/rejected. */
  error?: unknown;
}

export interface CaptureOptions {
  videoEl: HTMLVideoElement;
  /** Caller-built send fn; wire format is not inspected here (protocol/input.md). */
  sendInput: (msg: InputMsg) => void;
  /** Both booleans, every transition: `captured` drives the session chrome,
   *  `pointerLocked` decides whether an on-screen tap target is reachable. */
  onCaptureChange: (state: CaptureState) => void;
  /** Read only for bufferedAmount backpressure detection (AS10-13); never
   *  written from capture.ts. */
  channel?: RTCDataChannel;
  /** Live fullscreen state, read on every keyboard-lock re-evaluation (the API
   *  only grants Escape capture in fullscreen). Absent → Keyboard Lock never
   *  engaged. */
  isFullscreen?: () => boolean;
  /** Fired once per capture instance, on the FIRST keyboard.lock() rejection —
   *  updateKeyboardLock re-runs on every pointer-lock/fullscreen transition and
   *  would otherwise spam the UI. Refusal stays best-effort (keyboardLocked
   *  resets so a later sync retries), just no longer silent.
   *  Not the bypassed-certificate detector: lock() RESOLVES there (Chrome 151,
   *  measured) — that case is lib/certTrust.ts. */
  onKeyboardLockRefused?: (error: unknown) => void;
  /** Ctrl+Alt+Shift+Q while captured: capture.ts releases the lock first, then
   *  calls this. The completing 'Q' keydown is never forwarded to the host. */
  onSummonOverlay?: () => void;
}

/** bufferedAmount above which we flag backpressure (16 KiB). */
const BACKPRESSURE_THRESHOLD_BYTES = 16 * 1024;

/** Keyboard Lock is Chromium-only; elsewhere Esc keeps exiting pointer lock
 *  (Ctrl+Alt+Shift+Z is the portable deliberate release). */
interface KeyboardLockApi {
  lock: (keyCodes?: string[]) => Promise<void>;
  unlock: () => void;
}
function keyboardLockApi(): KeyboardLockApi | null {
  const kb = (navigator as unknown as { keyboard?: KeyboardLockApi }).keyboard;
  return kb && typeof kb.lock === "function" && typeof kb.unlock === "function"
    ? kb
    : null;
}

/** Chromium exposes Keyboard Lock only in secure contexts — on plain HTTP,
 *  in-game Esc capture is impossible. The UI words the release hint from this;
 *  quasar#376 tracks TLS. */
export function keyboardLockSupported(): boolean {
  return keyboardLockApi() != null;
}

/** Probed on `Element.prototype`, not `"pointerLockElement" in document` —
 *  WebKit ships the two independently, and the element method is the one the
 *  grab gesture calls (iPadOS Safari has never shipped it). */
export function pointerLockSupported(): boolean {
  return typeof Element !== "undefined" && "requestPointerLock" in Element.prototype;
}

/**
 * Touchscreen AND no Pointer Lock — exactly the devices where the touch layer
 * runs. Answers "are gestures live", not "does this screen accept a finger"
 * (a touch-capable laptop still has Pointer Lock and its mouse-look is the
 * mouse); the UI words the capture hint from this.
 */
export function touchLookSupported(): boolean {
  if (pointerLockSupported()) return false;
  // Two probes: `maxTouchPoints` reads 0 under WebKit's iPad emulation, which
  // does set `ontouchstart`; a real iPad answers yes to both.
  return (
    (typeof navigator !== "undefined" && (navigator.maxTouchPoints ?? 0) > 0) ||
    (typeof window !== "undefined" && "ontouchstart" in window)
  );
}

/** CSS px past which a gesture is a drag, no longer a tap/long-press. 10px is
 *  WebKit's own tap slop; the motion it absorbs is sent in the drag's first
 *  `mm`, so look has no dead zone. */
const TOUCH_SLOP_PX = 10;

/** 500ms is the iOS/Android long-press convention, and the sole tap/hold
 *  boundary — a separate "tap under N ms" rule would leave a dead band that
 *  emits nothing. */
const LONG_PRESS_MS = 500;

/** Wheel units per CSS px of two-finger travel. 1.0 = the wheel path's own
 *  pixel-deltaMode units, so touch and wheel agree (protocol/input.md: `ms`
 *  dx/dy are high-resolution wheel units). */
const TOUCH_SCROLL_SCALE = 1;

/**
 * Attach input listeners and pointer-lock tracking. cleanup() on unmount /
 * channel close; getMetrics() snapshots and resets the rate window (wall-clock
 * gated, any frequency); engage() from a user gesture; release() from any
 * surface. Input is forwarded only while captured; `mm` also needs the lock.
 */
export function setupCapture({
  videoEl,
  sendInput,
  onCaptureChange,
  channel,
  isFullscreen,
  onKeyboardLockRefused,
  onSummonOverlay,
}: CaptureOptions): {
  cleanup: () => void;
  getMetrics: () => CaptureMetrics;
  syncKeyboardLock: () => void;
  engage: () => Promise<EngageResult>;
  release: () => void;
} {
  /** Gates `mm` and nothing else. */
  let pointerLocked = false;
  /** The send gate. */
  let captured = false;
  /** "lock": pointerlockchange owns the release; "fallback": release() does.
   *  Keeps another element's pointerlockchange from dropping a fallback capture. */
  let captureMode: "none" | "lock" | "fallback" = "none";

  const lockSupported = pointerLockSupported();

  if (!lockSupported) {
    console.info(
      "input: Pointer Lock API unavailable on this browser (iPadOS Safari has " +
        "never implemented it) — capture forwards keyboard, buttons, scroll and " +
        "gamepad; relative mouse look is not possible here.",
    );
  } else if (!keyboardLockSupported()) {
    // Without this, "Esc still exits capture" on plain HTTP looks like a client
    // bug (quasar#376). Gated on lockSupported: telling an iPad user "use
    // HTTPS" would change nothing.
    console.info(
      "input: Keyboard Lock API unavailable (secure context required) — " +
        "Esc stays browser-owned and will release capture; use HTTPS to enable in-game Esc.",
    );
  }

  // hot-path reuse (#139): overwritten in place; shape per protocol/input.md
  const mmMsg: InputMsg = { t: "mm", dx: 0, dy: 0 };
  const msMsg: InputMsg = { t: "ms", dx: 0, dy: 0 };
  // ?itrace=1 mm sequence counter
  let mmSeq = 0;

  // Gamepad state for delta-only sends.
  const gpPrev: Record<number, { buttons: number[]; axes: number[] }> = {};
  let rafId: number;

  // Held keys already forwarded (evdev codes), flushed as key-ups on release —
  // otherwise a key held when capture drops sticks down on the host.
  const heldKeys = new Set<number>();

  // Idempotency guard so repeated syncs don't re-lock/re-unlock.
  let keyboardLocked = false;
  // One-shot guard for onKeyboardLockRefused — see its CaptureOptions doc.
  let keyboardLockRefusalReported = false;

  // ── AS10-13: rate-window counters (never allocated on hot path) ───────────
  let msgCount = 0;           // total input messages sent (any type)
  let coalescedCount = 0;     // coalesced pointer samples forwarded
  let gpSendCount = 0;        // gamepad state-change messages sent
  let mmSendCount = 0;        // relative mouse-move messages sent
  let backpressureSeen = false; // latched: bufferedAmount > threshold this window
  let windowStartMs = performance.now();

  // Whether getCoalescedEvents is available — probed once at setup time.
  const coalescedSupported =
    typeof PointerEvent !== "undefined" &&
    "getCoalescedEvents" in PointerEvent.prototype;

  // Wrap sendInput to count every outbound message and sample backpressure.
  const send = (msg: InputMsg) => {
    sendInput(msg);
    msgCount++;
    if (channel && channel.bufferedAmount > BACKPRESSURE_THRESHOLD_BYTES) {
      backpressureSeen = true;
    }
  };

  // For cleanup-time release sends: unmount races the WebRTC teardown, so
  // sendInput can throw — a throw must not abort the rest of cleanup.
  const sendBestEffort = (msg: InputMsg) => {
    try {
      send(msg);
    } catch {
      // ignore — channel closing/closed
    }
  };

  // ── Pointer Lock ──────────────────────────────────────────────────────────

  /** Escape captured for the game only while captured AND fullscreen (the API
   *  only grants it there). Idempotent; no-op without the API. */
  const updateKeyboardLock = () => {
    const kb = keyboardLockApi();
    if (!kb) return;
    const want = captured && !!isFullscreen?.();
    if (want && !keyboardLocked) {
      keyboardLocked = true;
      // Race guard: the desired state can flip while lock() is pending —
      // re-check in .then() and unlock rather than hold a stale lock.
      void kb
        .lock(["Escape"])
        .then(() => {
          if (!(captured && !!isFullscreen?.())) {
            keyboardLocked = false;
            kb.unlock();
          }
        })
        .catch((error: unknown) => {
          // rejects if not fullscreen / denied — reset so a later sync retries.
          // Best-effort but never silent: an unreported refusal reads as "Esc
          // randomly quits the game".
          keyboardLocked = false;
          console.warn(
            "input: Keyboard Lock request was refused — Esc stays browser-owned " +
              "and will release capture instead of reaching the game.",
            error,
          );
          if (!keyboardLockRefusalReported) {
            keyboardLockRefusalReported = true;
            onKeyboardLockRefused?.(error);
          }
        });
    } else if (!want && keyboardLocked) {
      keyboardLocked = false;
      kb.unlock();
    }
  };

  /** Key-up for every held key, then clear. cleanup() passes the best-effort
   *  send. */
  const flushHeldKeys = (sendFn: (msg: InputMsg) => void = send) => {
    for (const code of heldKeys) sendFn({ t: "k", code, pressed: false });
    heldKeys.clear();
  };

  /** All-zero state per forwarded pad (host must not keep a button held), then
   *  clear gpPrev so re-capture resends full state. Frozen `gp` shape only. */
  const resetGamepads = (sendFn: (msg: InputMsg) => void = send) => {
    for (const key of Object.keys(gpPrev)) {
      const idx = Number(key);
      const prev = gpPrev[idx];
      sendFn({
        t: "gp",
        i: idx,
        buttons: prev.buttons.map(() => 0),
        axes: prev.axes.map(() => 0),
      });
      delete gpPrev[idx];
    }
  };

  /** Single writer for `captured`. The captured→released transition releases
   *  everything the host still thinks is held, covering every release path. */
  const setCaptured = (next: boolean) => {
    if (captured === next) {
      onCaptureChange({ captured, pointerLocked });
      return;
    }
    const was = captured;
    captured = next;
    onCaptureChange({ captured, pointerLocked });
    if (was && !captured) {
      flushHeldKeys();
      resetGamepads();
      // a mid-gesture finger must not leave a long-press right button held
      resetTouch();
    }
    updateKeyboardLock();
  };

  const onLockChange = () => {
    pointerLocked = document.pointerLockElement === videoEl;
    if (pointerLocked) {
      captureMode = "lock";
      setCaptured(true);
    } else if (captureMode === "lock") {
      // the lock owned this capture, so its loss is the release; a fallback-mode
      // pointerlockchange belongs to another element and must not touch our state
      captureMode = "none";
      setCaptured(false);
    } else {
      onCaptureChange({ captured, pointerLocked });
    }
  };
  document.addEventListener("pointerlockchange", onLockChange);

  /**
   * Engage from a user gesture. A refused lock on a lock-capable device is
   * `failed` and engages nothing — never silently degrade a desktop to
   * no-mouse-look. Never `await`s before requestPointerLock: that would spend
   * the user gesture the API requires.
   */
  const engage = async (): Promise<EngageResult> => {
    if (captured) {
      return { mode: pointerLocked ? "pointer-lock" : "fallback" };
    }
    if (!lockSupported) {
      captureMode = "fallback";
      setCaptured(true);
      return { mode: "fallback" };
    }
    try {
      // Chrome ≥113 returns a Promise, older engines undefined; handle both
      // plus a synchronous throw.
      const maybe: unknown = videoEl.requestPointerLock();
      if (maybe && typeof (maybe as PromiseLike<void>).then === "function") {
        await (maybe as PromiseLike<void>);
      }
      return { mode: "pointer-lock" };
    } catch (error) {
      return { mode: "failed", error };
    }
  };

  /** Idempotent. Lock mode goes through exitPointerLock so browser state and
   *  ours never diverge (pointerlockchange runs the release); fallback clears
   *  directly. */
  const release = () => {
    if (captureMode === "lock" && document.exitPointerLock) {
      document.exitPointerLock();
      return;
    }
    // exitPointerLock absent: leaving a user captured with no way out is worse
    // than a possible divergence.
    if (!captured) return;
    captureMode = "none";
    setCaptured(false);
  };

  // ── Mouse ─────────────────────────────────────────────────────────────────

  // seq/tc stamp when tracing; zero wire impact when ?itrace=1 is absent
  const stampMm = () => {
    mmSendCount++;
    if (!INPUT_TRACE) return;
    mmMsg.seq = mmSeq++;
    mmMsg.tc = performance.now();
  };

  // A touchscreen tap also emits a synthesized mousedown/mouseup pair, which in
  // fallback mode would fire a game click on every touch — including the tap on
  // the session-menu button that is the way back out. pointerdown always
  // precedes the synthesized mouse event, so this is enough to tell them apart.
  let lastPointerType = "mouse";
  const onPointerDown = (e: PointerEvent) => {
    lastPointerType = e.pointerType || "mouse";
  };
  document.addEventListener("pointerdown", onPointerDown, true);

  /** Fallback mode: forward only what landed on the video, and never a
   *  touch-synthesized click (the touch layer is the one place a finger becomes
   *  a message — a tap here would double-send). Under Pointer Lock: skipped;
   *  every mouse event is routed to the locked element and there is no chrome
   *  to hit. */
  const aimedAtGame = (e: Event): boolean => {
    if (pointerLocked) return true;
    if (lastPointerType === "touch") return false;
    return e.target === videoEl;
  };

  const onPointerMove = (e: PointerEvent) => {
    // Gated on pointerLocked, not captured: without a lock movementX/Y describe
    // a cursor walking around a web page. No lock, no mouse-look.
    if (!pointerLocked) return;
    // getCoalescedEvents recovers every raw sample, so a 1000 Hz mouse forwards
    // its full trajectory; each sample is a normal `mm`.
    const coalesced = e.getCoalescedEvents?.();
    if (coalesced && coalesced.length > 1) {
      let sent = false;
      for (const ce of coalesced) {
        if (ce.movementX !== 0 || ce.movementY !== 0) {
          mmMsg.dx = ce.movementX;
          mmMsg.dy = ce.movementY;
          stampMm();
          send(mmMsg);
          coalescedCount++;
          sent = true;
        }
      }
      // Some engines zero movementX/Y on coalesced events; fall back to the
      // summary event's delta so motion is never silently dropped.
      if (sent) return;
    }
    if (e.movementX !== 0 || e.movementY !== 0) {
      mmMsg.dx = e.movementX;
      mmMsg.dy = e.movementY;
      stampMm();
      send(mmMsg);
    }
  };

  const onMouseDown = (e: MouseEvent) => {
    if (!captured || !aimedAtGame(e)) return;
    const button = MB_MAP[e.button];
    if (button !== undefined) send({ t: "mb", button, pressed: true });
  };

  const onMouseUp = (e: MouseEvent) => {
    if (!captured || !aimedAtGame(e)) return;
    const button = MB_MAP[e.button];
    if (button !== undefined) send({ t: "mb", button, pressed: false });
  };

  const onWheel = (e: WheelEvent) => {
    // preventDefault() inside the guard: in fallback mode the drawer can be
    // open while captured, and swallowing its wheel would make it unscrollable
    if (!captured || !aimedAtGame(e)) return;
    e.preventDefault();
    msMsg.dx = e.deltaX;
    msMsg.dy = e.deltaY;
    send(msMsg);
  };

  document.addEventListener("pointermove", onPointerMove);
  document.addEventListener("mousedown", onMouseDown);
  document.addEventListener("mouseup", onMouseUp);
  // passive:false so we can call preventDefault() to suppress page scroll.
  document.addEventListener("wheel", onWheel, { passive: false });

  // ── Touch gestures (see the header) ───────────────────────────────────────

  /** One live finger. `x`/`y` track the last sample; `startX`/`startY` the touch-down. */
  interface TouchPoint {
    x: number;
    y: number;
    startX: number;
    startY: number;
  }

  /** Fingers down on the video, keyed by pointerId. Membership is the scope
   *  guarantee: a pointer that went down elsewhere is never inserted, so its
   *  moves and lift miss `.get()` below and chrome keeps its taps. */
  const touches = new Map<number, TouchPoint>();

  /**
   * pending — inside the slop, nothing sent yet; look — past the slop, moves
   * are `mm`; hold — long-press fired, right button down, motion ignored;
   * scroll — two+ fingers, moves are `ms`. Never moves backwards, and only
   * returns to "none" when the last finger lifts — lifting one finger
   * mid-scroll cannot promote the other into a look-drag.
   */
  let touchGesture: "none" | "pending" | "look" | "hold" | "scroll" = "none";
  let longPressTimer: ReturnType<typeof setTimeout> | null = null;
  /** True while a long-press has pressed BTN_RIGHT that we still owe a release for. */
  let rightHeld = false;
  /** Last two-finger midpoint, in client px; the `ms` delta is its movement. */
  let scrollX = 0;
  let scrollY = 0;

  /** Touch input is live only while captured AND unlocked — see the header. */
  const touchLive = () => captured && !pointerLocked;

  const cancelLongPress = () => {
    if (longPressTimer !== null) {
      clearTimeout(longPressTimer);
      longPressTimer = null;
    }
  };

  /** Release a long-press right button if we hold one. Idempotent. */
  const releaseRightButton = (sendFn: (msg: InputMsg) => void = send) => {
    if (!rightHeld) return;
    rightHeld = false;
    sendFn({ t: "mb", button: MB_MAP[2], pressed: false });
  };

  /** Scroll anchor. Midpoint rather than one finger, so a pinch (fingers
   *  opposing) produces ~no scroll instead of a lurch. */
  const touchMidpoint = (): { x: number; y: number } => {
    let sx = 0;
    let sy = 0;
    for (const p of touches.values()) {
      sx += p.x;
      sy += p.y;
    }
    const n = touches.size || 1;
    return { x: sx / n, y: sy / n };
  };

  /** Abandon any in-flight gesture, leaving the host with nothing held — a
   *  long-press right button must never survive the capture that produced it. */
  const resetTouch = (sendFn: (msg: InputMsg) => void = send) => {
    cancelLongPress();
    releaseRightButton(sendFn);
    touches.clear();
    touchGesture = "none";
  };

  const onTouchDown = (e: PointerEvent) => {
    if (e.pointerType !== "touch" || !touchLive()) return;
    // chrome is not a descendant of <video>, so identity is the whole scope check
    if (e.target !== videoEl) return;
    // Suppress the platform's own pan/callout/zoom. `.session-video` also
    // carries `touch-action: none`, which keeps iOS delivering pointermove
    // instead of pointercancel once panning would have begun.
    e.preventDefault();

    touches.set(e.pointerId, { x: e.clientX, y: e.clientY, startX: e.clientX, startY: e.clientY });

    if (touches.size === 1) {
      touchGesture = "pending";
      longPressTimer = setTimeout(() => {
        longPressTimer = null;
        // only a still-"pending" gesture can become a right-click
        if (touchGesture !== "pending" || !touchLive()) return;
        touchGesture = "hold";
        rightHeld = true;
        send({ t: "mb", button: MB_MAP[2], pressed: true });
      }, LONG_PRESS_MS);
      return;
    }

    // second finger = scroll; release a fired long-press before switching
    cancelLongPress();
    releaseRightButton();
    touchGesture = "scroll";
    const mid = touchMidpoint();
    scrollX = mid.x;
    scrollY = mid.y;
  };

  const onTouchMove = (e: PointerEvent) => {
    if (e.pointerType !== "touch") return;
    const pt = touches.get(e.pointerId);
    if (!pt) return;
    if (!touchLive()) return;

    const dx = e.clientX - pt.x;
    const dy = e.clientY - pt.y;
    pt.x = e.clientX;
    pt.y = e.clientY;

    if (touchGesture === "scroll") {
      const mid = touchMidpoint();
      // fingers up = content down = positive deltaY, matching the wheel's sign
      const sdx = (scrollX - mid.x) * TOUCH_SCROLL_SCALE;
      const sdy = (scrollY - mid.y) * TOUCH_SCROLL_SCALE;
      scrollX = mid.x;
      scrollY = mid.y;
      if (sdx !== 0 || sdy !== 0) {
        msMsg.dx = sdx;
        msMsg.dy = sdy;
        send(msMsg);
      }
      return;
    }

    // A fired long-press owns the finger; moving it does not also look around.
    if (touchGesture === "hold") return;

    if (touchGesture === "pending") {
      const totX = e.clientX - pt.startX;
      const totY = e.clientY - pt.startY;
      if (Math.hypot(totX, totY) < TOUCH_SLOP_PX) return;
      // committed to a drag; the slop-absorbed motion goes out as the first `mm`
      cancelLongPress();
      touchGesture = "look";
      mmMsg.dx = totX;
      mmMsg.dy = totY;
      stampMm();
      send(mmMsg);
      return;
    }

    if (touchGesture === "look" && (dx !== 0 || dy !== 0)) {
      mmMsg.dx = dx;
      mmMsg.dy = dy;
      stampMm();
      send(mmMsg);
    }
  };

  /** `cancelled` = pointercancel (the platform took the gesture back). A
   *  cancelled press must never emit the tap it was becoming. */
  const endTouch = (e: PointerEvent, cancelled: boolean) => {
    if (e.pointerType !== "touch") return;
    const pt = touches.get(e.pointerId);
    if (!pt) return;
    touches.delete(e.pointerId);

    const wasPending = touchGesture === "pending";

    if (touches.size === 0) {
      // tap: press+release as one pair so the guest sees a complete click
      if (!cancelled && wasPending && touchLive()) {
        cancelLongPress();
        send({ t: "mb", button: MB_MAP[0], pressed: true });
        send({ t: "mb", button: MB_MAP[0], pressed: false });
      }
      resetTouch();
      return;
    }

    // Re-seat the scroll anchor on the survivors — otherwise the midpoint jump
    // on lift would be forwarded as a large bogus scroll.
    if (touchGesture === "scroll") {
      const mid = touchMidpoint();
      scrollX = mid.x;
      scrollY = mid.y;
    }
  };

  const onTouchUp = (e: PointerEvent) => endTouch(e, false);
  const onTouchCancel = (e: PointerEvent) => endTouch(e, true);

  // Move/up/cancel are on the document so a finger dragged off the picture
  // keeps steering; they are scoped by pointerId membership in `touches`.
  document.addEventListener("pointerdown", onTouchDown, { passive: false });
  document.addEventListener("pointermove", onTouchMove);
  document.addEventListener("pointerup", onTouchUp);
  document.addEventListener("pointercancel", onTouchCancel);

  // ── Keyboard ──────────────────────────────────────────────────────────────

  const onKeyDown = (e: KeyboardEvent) => {
    if (!captured || e.repeat) return;
    // Escape release, fallback mode only: under Pointer Lock the browser owns
    // Escape (or Keyboard Lock forwards it to the game) and capture.ts must not
    // touch it; without a lock nothing else would ever end capture from a
    // hardware keyboard.
    if (!pointerLocked && e.code === "Escape") {
      e.preventDefault();
      release();
      return;
    }
    // Ctrl+Alt+Shift+Q: release AND open the menu (Esc / the Z combo only
    // release). This arm runs only while captured; SessionPage owns the
    // uncaptured half of the chord — summonCombo.ts explains the split.
    if (isSummonCombo(e)) {
      e.preventDefault();
      release();
      onSummonOverlay?.();
      return;
    }
    // Ctrl+Alt+Shift+Z deliberate release. The completing 'Z' must not leak to
    // the host; the already-forwarded modifiers get key-ups from the unlock
    // flush, so no stuck keys.
    if (e.ctrlKey && e.altKey && e.shiftKey && e.code === "KeyZ") {
      e.preventDefault();
      release();
      return;
    }
    e.preventDefault();
    const code = EVDEV[e.code];
    if (code !== undefined) {
      send({ t: "k", code, pressed: true });
      heldKeys.add(code);
    }
  };

  const onKeyUp = (e: KeyboardEvent) => {
    if (!captured) return;
    e.preventDefault();
    const code = EVDEV[e.code];
    if (code !== undefined) {
      send({ t: "k", code, pressed: false });
      heldKeys.delete(code);
    }
  };

  document.addEventListener("keydown", onKeyDown);
  document.addEventListener("keyup", onKeyUp);

  // ── Gamepad (W3C Standard Gamepad — polled in rAF, send on delta only) ───

  function pollGamepads() {
    // Gated on `captured`, never `pointerLocked` (see header). The rAF loop
    // keeps running so forwarding resumes on re-capture — gpPrev was cleared on
    // release, so the first post-capture poll resends full state.
    if (captured) {
      const pads = navigator.getGamepads ? navigator.getGamepads() : [];
      for (const pad of pads) {
        if (!pad) continue;
        const buttons = Array.from(pad.buttons, (b) => b.value);
        const axes = Array.from(pad.axes, (a) => Math.round(a * 1000) / 1000);
        const prev = gpPrev[pad.index];
        const changed =
          !prev ||
          buttons.some((v, i) => v !== prev.buttons[i]) ||
          axes.some((v, i) => v !== prev.axes[i]);
        if (changed) {
          send({ t: "gp", i: pad.index, buttons, axes });
          gpSendCount++;
          gpPrev[pad.index] = { buttons, axes };
        }
      }
    }
    rafId = requestAnimationFrame(pollGamepads);
  }
  rafId = requestAnimationFrame(pollGamepads);

  // ── AS10-13: metrics accessor ─────────────────────────────────────────────

  const getMetrics = (): CaptureMetrics => {
    const nowMs = performance.now();
    // guard against a zero elapsed window
    const elapsedSec = Math.max((nowMs - windowStartMs) / 1000, 0.001);

    const inputMsgPerSec = msgCount / elapsedSec;
    const coalescedSamplesPerSec = coalescedCount / elapsedSec;
    const gamepadSendPerSec = gpSendCount / elapsedSec;
    const mmSentPerSec = mmSendCount / elapsedSec;
    const channelBufferedAmount = channel ? channel.bufferedAmount : 0;

    // one pass over the sparse array so count and pads never disagree
    const rawPads = navigator.getGamepads ? navigator.getGamepads() : [];
    let gamepadCount = 0;
    const padIdentities: GamepadIdentity[] = [];
    for (const p of rawPads) {
      if (!p) continue;
      gamepadCount++;
      padIdentities.push({ index: p.index, id: p.id });
    }

    const snapshot: CaptureMetrics = {
      pointerLocked,
      captured,
      pointerLockSupported: lockSupported,
      coalescedSupported,
      inputMsgPerSec,
      coalescedSamplesPerSec,
      channelBufferedAmount,
      backpressureDetected: backpressureSeen,
      gamepadCount,
      pads: padIdentities,
      gamepadSendPerSec,
      mmSentPerSec,
      inputTrace: INPUT_TRACE,
    };

    // Reset rate-window for the next poll period.
    msgCount = 0;
    coalescedCount = 0;
    gpSendCount = 0;
    mmSendCount = 0;
    backpressureSeen = false;
    windowStartMs = nowMs;

    return snapshot;
  };

  // ── Cleanup ───────────────────────────────────────────────────────────────

  const cleanup = () => {
    // Release everything before listeners come down, so an abrupt unmount while
    // captured doesn't leave the host with stuck keys or gamepad buttons.
    if (captured) {
      flushHeldKeys(sendBestEffort);
      resetGamepads(sendBestEffort);
    }
    resetTouch(sendBestEffort);
    captured = false;
    captureMode = "none";
    document.removeEventListener("pointerlockchange", onLockChange);
    document.removeEventListener("pointerdown", onPointerDown, true);
    document.removeEventListener("pointermove", onPointerMove);
    document.removeEventListener("mousedown", onMouseDown);
    document.removeEventListener("mouseup", onMouseUp);
    document.removeEventListener("wheel", onWheel);
    document.removeEventListener("pointerdown", onTouchDown);
    document.removeEventListener("pointermove", onTouchMove);
    document.removeEventListener("pointerup", onTouchUp);
    document.removeEventListener("pointercancel", onTouchCancel);
    document.removeEventListener("keydown", onKeyDown);
    document.removeEventListener("keyup", onKeyUp);
    cancelAnimationFrame(rafId);
    // the pointerlockchange listener is gone, so unlock here
    if (keyboardLocked) {
      keyboardLocked = false;
      keyboardLockApi()?.unlock();
    }
    if (document.pointerLockElement === videoEl && document.exitPointerLock) {
      document.exitPointerLock();
    }
  };

  return { cleanup, getMetrics, syncKeyboardLock: updateKeyboardLock, engage, release };
}
