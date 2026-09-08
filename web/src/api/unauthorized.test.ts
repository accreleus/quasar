/**
 * The 401 fan-out (#154). Small enough to test directly, and worth testing
 * directly because two properties are easy to lose: a handler may unsubscribe
 * itself from inside the notification, and nothing may be delivered after an
 * unsubscribe.
 */

import { describe, expect, it, vi } from "vitest";
import { notifyUnauthorized, onUnauthorized } from "./unauthorized";

describe("onUnauthorized", () => {
  it("delivers to every subscriber, and stops on unsubscribe", () => {
    const a = vi.fn();
    const b = vi.fn();
    const offA = onUnauthorized(a);
    const offB = onUnauthorized(b);

    notifyUnauthorized();
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).toHaveBeenCalledTimes(1);

    offA();
    notifyUnauthorized();
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).toHaveBeenCalledTimes(2);
    offB();
  });

  it("survives a handler that unsubscribes itself mid-notification", () => {
    const seen: string[] = [];
    const off1 = onUnauthorized(() => {
      seen.push("first");
      off1();
    });
    const off2 = onUnauthorized(() => seen.push("second"));

    // Without the copy in notifyUnauthorized this either throws or skips
    // "second", depending on the iterator's mood.
    expect(() => notifyUnauthorized()).not.toThrow();
    expect(seen).toEqual(["first", "second"]);
    off2();
  });

  it("is a no-op with no subscribers", () => {
    expect(() => notifyUnauthorized()).not.toThrow();
  });
});
