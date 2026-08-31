// Finding 1 — the bootstrap deadlock.
//
// The regression these lock down: with capture.ts gating every keydown on
// pointer lock, an uncaptured user pressing the summon chord reached nothing,
// and the drawer is the only route to "Capture input" and "Exit session".
//
// NOT covered here, and not coverable: jsdom implements no Pointer Lock, so
// `document.pointerLockElement` is stubbed below rather than driven by a real
// requestPointerLock(). What that stub cannot prove is that a real browser
// keeps `pointerLockElement` set for the duration of the keydown dispatch in
// which exitPointerLock() is called (the spec queues the change as a task, so
// it does) — the disjointness argument rests on that plus code review.

import { renderHook, act } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useOverlaySummon } from "./useOverlaySummon";
import { setupCapture } from "../../input/capture";

/** jsdom has no Pointer Lock; stand in for it. */
function setPointerLock(el: Element | null) {
  Object.defineProperty(document, "pointerLockElement", {
    value: el,
    configurable: true,
    writable: true,
  });
}

function pressCombo(init: Partial<KeyboardEventInit> = {}) {
  const e = new KeyboardEvent("keydown", {
    code: "KeyQ",
    key: "Q",
    ctrlKey: true,
    altKey: true,
    shiftKey: true,
    bubbles: true,
    cancelable: true,
    ...init,
  });
  document.dispatchEvent(e);
  return e;
}

afterEach(() => {
  setPointerLock(null);
});

describe("useOverlaySummon", () => {
  it("summons the drawer on the chord while input is NOT captured", () => {
    const onSummon = vi.fn();
    renderHook(() => useOverlaySummon(onSummon));
    act(() => {
      pressCombo();
    });
    expect(onSummon).toHaveBeenCalledTimes(1);
  });

  it("swallows the completing Q so it cannot reach the page", () => {
    renderHook(() => useOverlaySummon(vi.fn()));
    let event!: KeyboardEvent;
    act(() => {
      event = pressCombo();
    });
    expect(event.defaultPrevented).toBe(true);
  });

  it("stands down while input IS captured — that press belongs to capture.ts", () => {
    const onSummon = vi.fn();
    renderHook(() => useOverlaySummon(onSummon));
    setPointerLock(document.createElement("video"));
    act(() => {
      pressCombo();
    });
    expect(onSummon).not.toHaveBeenCalled();
  });

  it("ignores every other chord", () => {
    const onSummon = vi.fn();
    renderHook(() => useOverlaySummon(onSummon));
    act(() => {
      pressCombo({ code: "KeyZ" });
      pressCombo({ shiftKey: false });
      pressCombo({ repeat: true });
    });
    expect(onSummon).not.toHaveBeenCalled();
  });

  it("removes its listener on unmount", () => {
    const onSummon = vi.fn();
    const { unmount } = renderHook(() => useOverlaySummon(onSummon));
    unmount();
    act(() => {
      pressCombo();
    });
    expect(onSummon).not.toHaveBeenCalled();
  });
});

// The whole point of splitting the chord across two listeners is that exactly
// one of them answers any given press. Both are live at once during a session,
// so test them together rather than in isolation.
describe("summon chord: exactly one handler answers a press", () => {
  function mountBoth() {
    const pageSummon = vi.fn();
    const captureSummon = vi.fn();
    // Production order: SessionPage's listener is attached on mount, capture.ts's
    // only once the DataChannel opens — so the page listener runs first.
    const hook = renderHook(() => useOverlaySummon(pageSummon));
    const videoEl = document.createElement("video");
    document.body.appendChild(videoEl);
    const { cleanup } = setupCapture({
      videoEl,
      sendInput: () => {},
      onCaptureChange: () => {},
      onSummonOverlay: captureSummon,
    });
    return {
      pageSummon,
      captureSummon,
      videoEl,
      teardown: () => {
        cleanup();
        hook.unmount();
        videoEl.remove();
      },
    };
  }

  it("captured: capture.ts answers, SessionPage does not", () => {
    const { pageSummon, captureSummon, videoEl, teardown } = mountBoth();
    setPointerLock(videoEl);
    document.dispatchEvent(new Event("pointerlockchange"));

    act(() => {
      pressCombo();
    });

    expect(captureSummon).toHaveBeenCalledTimes(1);
    expect(pageSummon).not.toHaveBeenCalled();
    teardown();
  });

  it("uncaptured: SessionPage answers, capture.ts does not", () => {
    const { pageSummon, captureSummon, teardown } = mountBoth();
    setPointerLock(null);
    document.dispatchEvent(new Event("pointerlockchange"));

    act(() => {
      pressCombo();
    });

    expect(pageSummon).toHaveBeenCalledTimes(1);
    expect(captureSummon).not.toHaveBeenCalled();
    teardown();
  });
});
