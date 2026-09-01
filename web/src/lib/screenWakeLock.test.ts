import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { ScreenWakeLock, screenWakeLockSupported, useScreenWakeLock } from "./screenWakeLock";

/** Minimal stand-in for a real WakeLockSentinel, with the UA-side release. */
class FakeSentinel {
  released = false;
  private listeners = new Set<() => void>();
  addEventListener(_type: "release", cb: () => void) {
    this.listeners.add(cb);
  }
  removeEventListener(_type: "release", cb: () => void) {
    this.listeners.delete(cb);
  }
  release() {
    this.releasedByUa();
    return Promise.resolve();
  }
  /** What the browser does on its own when the document is hidden. */
  releasedByUa() {
    if (this.released) return;
    this.released = true;
    for (const cb of this.listeners) cb();
  }
}

interface Harness {
  sentinels: FakeSentinel[];
  requests: number;
  reject: Error | null;
}

function installWakeLock(): Harness {
  const h: Harness = { sentinels: [], requests: 0, reject: null };
  Object.defineProperty(navigator, "wakeLock", {
    configurable: true,
    value: {
      request: (type: string) => {
        expect(type).toBe("screen");
        h.requests += 1;
        if (h.reject) return Promise.reject(h.reject);
        const s = new FakeSentinel();
        h.sentinels.push(s);
        return Promise.resolve(s);
      },
    },
  });
  return h;
}

function removeWakeLock() {
  Object.defineProperty(navigator, "wakeLock", { configurable: true, value: undefined });
}

function setVisibility(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: state });
  document.dispatchEvent(new Event("visibilitychange"));
}

/** Let the request promise chain settle. */
const settle = () => new Promise<void>((r) => setTimeout(r, 0));

afterEach(() => {
  removeWakeLock();
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  // `restoreAllMocks` only restores `vi.spyOn` spies since vitest 3; clear call
  // history too, or a plain `vi.fn()`'s counters survive into the next test.
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("screenWakeLockSupported", () => {
  it("is false when the API is absent", () => {
    removeWakeLock();
    expect(screenWakeLockSupported()).toBe(false);
  });

  it("is true when navigator.wakeLock.request exists", () => {
    installWakeLock();
    expect(screenWakeLockSupported()).toBe(true);
  });
});

describe("ScreenWakeLock", () => {
  it("acquires a sentinel on start", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    await settle();

    expect(h.requests).toBe(1);
    expect(lock.held()).toBe(h.sentinels[0]);
    expect(h.sentinels[0].released).toBe(false);
    lock.stop();
  });

  it("releases the sentinel on stop and holds nothing after", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    await settle();
    lock.stop();
    await settle();

    expect(h.sentinels[0].released).toBe(true);
    expect(lock.held()).toBeNull();
  });

  // The single most commonly-missed behaviour: the UA drops the sentinel on
  // hide and does NOT restore it on return.
  it("re-acquires after a background/foreground cycle", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    await settle();
    const first = h.sentinels[0];

    // Background: browser releases the sentinel, then fires visibilitychange.
    first.releasedByUa();
    setVisibility("hidden");
    await settle();
    expect(lock.held()).toBeNull();
    expect(h.requests).toBe(1); // never requests while hidden — it would reject

    // Foreground again.
    setVisibility("visible");
    await settle();

    expect(h.requests).toBe(2);
    expect(h.sentinels).toHaveLength(2);
    expect(lock.held()).toBe(h.sentinels[1]);
    expect(h.sentinels[1].released).toBe(false);
    lock.stop();
  });

  it("does not re-acquire after stop, even on a foreground edge", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    await settle();
    lock.stop();
    await settle();

    setVisibility("hidden");
    setVisibility("visible");
    await settle();

    expect(h.requests).toBe(1);
    expect(lock.held()).toBeNull();
  });

  it("never opens a second sentinel while one is held", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    lock.start();
    await settle();
    setVisibility("visible");
    await settle();

    expect(h.requests).toBe(1);
  });

  it("degrades silently when the API is absent", async () => {
    removeWakeLock();
    const lock = new ScreenWakeLock();
    expect(() => lock.start()).not.toThrow();
    await settle();
    expect(lock.held()).toBeNull();
    expect(() => lock.stop()).not.toThrow();
  });

  it("degrades silently when the request rejects", async () => {
    const h = installWakeLock();
    h.reject = new DOMException("denied", "NotAllowedError");
    const unhandled = vi.fn();
    process.on("unhandledRejection", unhandled);
    const lock = new ScreenWakeLock();
    lock.start();
    await settle();
    process.off("unhandledRejection", unhandled);

    expect(lock.held()).toBeNull();
    expect(unhandled).not.toHaveBeenCalled();
    lock.stop();
  });

  it("releases a sentinel that arrives after stop", async () => {
    const h = installWakeLock();
    const lock = new ScreenWakeLock();
    lock.start();
    lock.stop(); // request still in flight
    await settle();

    expect(lock.held()).toBeNull();
    expect(h.sentinels[0].released).toBe(true);
  });
});

describe("useScreenWakeLock", () => {
  it("holds nothing while inactive, acquires when it goes live", async () => {
    const h = installWakeLock();
    const view = renderHook(({ live }) => useScreenWakeLock(live), {
      initialProps: { live: false },
    });
    await settle();
    expect(h.requests).toBe(0);

    view.rerender({ live: true });
    await settle();
    expect(h.sentinels).toHaveLength(1);
    expect(h.sentinels[0].released).toBe(false);
    view.unmount();
  });

  it("releases when the session stops (active -> false)", async () => {
    const h = installWakeLock();
    const view = renderHook(({ live }) => useScreenWakeLock(live), {
      initialProps: { live: true },
    });
    await settle();
    expect(h.sentinels[0].released).toBe(false);

    view.rerender({ live: false });
    await settle();
    expect(h.sentinels[0].released).toBe(true);
    view.unmount();
  });

  // Navigating away from /app/session/:id unmounts the page. A sentinel left
  // behind here keeps the user's screen awake after they have gone.
  it("releases on unmount (navigate away)", async () => {
    const h = installWakeLock();
    const view = renderHook(({ live }) => useScreenWakeLock(live), {
      initialProps: { live: true },
    });
    await settle();
    view.unmount();
    await settle();

    expect(h.sentinels[0].released).toBe(true);
  });

  it("stops re-acquiring on visibility once unmounted", async () => {
    const h = installWakeLock();
    const view = renderHook(({ live }) => useScreenWakeLock(live), {
      initialProps: { live: true },
    });
    await settle();
    view.unmount();
    await settle();

    setVisibility("hidden");
    setVisibility("visible");
    await settle();

    expect(h.requests).toBe(1);
  });
});
