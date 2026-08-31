// Screen Wake Lock (#434) — gamepad input never pokes the OS idle timer, so a
// controller-only player's screen sleeps mid-game. Three guarantees:
//  1. Re-acquire on visibilitychange: the UA releases the sentinel on hidden
//     and never restores it; requesting while hidden always rejects, so wait
//     for the visible edge.
//  2. No leaked sentinel: stop()/unmount always release — a leak keeps the
//     screen awake after the user leaves.
//  3. Silence on every failure path (API absent, insecure context, power
//     saver): none of that is a session problem; only a debug breadcrumb.
// Framework-free so it can be exercised in a real browser without React.

import { useEffect } from "react";
import { reportBestEffortFailure } from "./reportBestEffortFailure";

/** Structural subset of WakeLockSentinel we depend on. */
interface SentinelLike {
  readonly released: boolean;
  release(): Promise<void>;
  addEventListener(type: "release", listener: () => void): void;
  removeEventListener(type: "release", listener: () => void): void;
}

interface WakeLockLike {
  request(type: "screen"): Promise<SentinelLike>;
}

function wakeLockApi(): WakeLockLike | null {
  if (typeof navigator === "undefined") return null;
  const wl = (navigator as Navigator & { wakeLock?: WakeLockLike }).wakeLock;
  return wl && typeof wl.request === "function" ? wl : null;
}

/** API presence only — `true` does not promise a request will succeed. */
export function screenWakeLockSupported(): boolean {
  return wakeLockApi() !== null;
}

/**
 * Holds a `screen` wake lock for as long as it is armed, re-acquiring whenever
 * the document returns to the foreground.
 *
 * Lifecycle: `start()` arms and requests; `stop()` disarms and releases. Both
 * are idempotent and neither ever rejects.
 */
export class ScreenWakeLock {
  /** True between start() and stop() — "the caller still wants a lock". */
  private armed = false;
  private sentinel: SentinelLike | null = null;
  /** In-flight request, so two triggers can't open two sentinels. */
  private pending: Promise<void> | null = null;

  private readonly onVisibilityChange = () => {
    // Only the visible edge matters. The hidden edge is the UA releasing the
    // sentinel out from under us, which the sentinel's own release event
    // already reports.
    if (document.visibilityState === "visible") void this.acquire();
  };

  private readonly onSentinelRelease = () => {
    this.sentinel = null;
  };

  /** Arm and request. Safe to call when unsupported or already held. */
  start(): void {
    if (this.armed) return;
    this.armed = true;
    document.addEventListener("visibilitychange", this.onVisibilityChange);
    void this.acquire();
  }

  /** Disarm and release. Safe to call twice, or when never started. */
  stop(): void {
    if (!this.armed) return;
    this.armed = false;
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    const held = this.sentinel;
    this.sentinel = null;
    if (!held) return;
    held.removeEventListener("release", this.onSentinelRelease);
    void held.release().catch((err: unknown) => {
      // Releasing an already-released sentinel rejects on some engines; the
      // lock is gone either way, which is the only thing that matters.
      reportBestEffortFailure("silent-debug", "wakeLock: release", err);
    });
  }

  /** Test/diagnostic accessor — the live sentinel, or null when none is held. */
  held(): SentinelLike | null {
    return this.sentinel;
  }

  private acquire(): Promise<void> {
    if (!this.armed || this.sentinel) return Promise.resolve();
    if (this.pending) return this.pending;
    const api = wakeLockApi();
    if (!api) return Promise.resolve();
    // A request from a hidden document is specified to reject. Skipping it
    // keeps the console clean; the visible edge re-drives us.
    if (typeof document !== "undefined" && document.visibilityState === "hidden") {
      return Promise.resolve();
    }
    this.pending = api
      .request("screen")
      .then((sentinel) => {
        // stop() may have run while the request was in flight — never keep a
        // sentinel nobody wants.
        if (!this.armed) {
          void sentinel.release().catch(() => {});
          return;
        }
        this.sentinel = sentinel;
        sentinel.addEventListener("release", this.onSentinelRelease);
      })
      .catch((err: unknown) => {
        // Unsupported context, power saver, no visible screen: all normal.
        reportBestEffortFailure("silent-debug", "wakeLock: request", err);
      })
      .finally(() => {
        this.pending = null;
      });
    return this.pending;
  }
}

/**
 * Hold a screen wake lock while `active` is true.
 *
 * `active` should be the caller's existing notion of "the session is live" —
 * do not invent a second one. Flipping it to false, unmounting, or navigating
 * away all release the sentinel.
 */
export function useScreenWakeLock(active: boolean): void {
  useEffect(() => {
    if (!active) return;
    const lock = new ScreenWakeLock();
    lock.start();
    return () => lock.stop();
  }, [active]);
}
