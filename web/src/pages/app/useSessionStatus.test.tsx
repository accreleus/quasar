// #449 — noteFailure is the one writer of the sticky verdict. After the loader's
// reveal has begun it holds only what the page can still show.

import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useSessionStatus } from "./useSessionStatus";
import {
  accessRevokedFailure,
  isBannerVerdict,
  launchFailureFromSession,
  showsAfterReveal,
  takenOverFailure,
  unreachableFailure,
  type LaunchFailure,
} from "./sessionFailure";

vi.mock("../../api/library", () => ({
  getSession: () => new Promise(() => {}),
}));

const verdict = (state: string, over: Record<string, unknown> = {}) =>
  launchFailureFromSession({
    state,
    state_detail: null,
    error_message: null,
    failure_code: null,
    app_log_tail: null,
    ...over,
  } as Parameters<typeof launchFailureFromSession>[0])!;

const kinds: [LaunchFailure["kind"], LaunchFailure, boolean][] = [
  ["failed", verdict("failed", { failure_code: "app_exited_early" }), false],
  ["ended", verdict("stopped"), false],
  ["unreachable", unreachableFailure(), true],
  ["taken_over", takenOverFailure(), true],
  ["access_revoked", accessRevokedFailure({ stop_reason: "entitlement_revoked" })!, true],
];

describe("noteFailure", () => {
  it.each(kinds)("%s: held before the reveal starts", (_k, failure) => {
    const { result } = renderHook(() => useSessionStatus("t", "s1", false, { current: false }));
    act(() => result.current.noteFailure(failure));
    expect(result.current.launchFailure).toBe(failure);
  });

  it.each(kinds)("%s: after the reveal starts, held only if the page can show it", (_k, failure, shown) => {
    const { result } = renderHook(() => useSessionStatus("t", "s1", false, { current: true }));
    act(() => result.current.noteFailure(failure));
    expect(result.current.launchFailure).toBe(shown ? failure : null);
  });

  it("the reveal starting between two writes drops the second", () => {
    const revealStarted = { current: false };
    const { result } = renderHook(() => useSessionStatus("t", "s1", false, revealStarted));
    act(() => result.current.noteFailure(unreachableFailure()));
    revealStarted.current = true;
    act(() => result.current.noteFailure(kinds[1][1]));
    expect(result.current.launchFailure?.kind).toBe("unreachable");
  });

  it("agrees with the page's banner kinds", () => {
    for (const [, failure, shown] of kinds) {
      expect(showsAfterReveal(failure)).toBe(shown);
      expect(isBannerVerdict(failure)).toBe(failure.kind === "taken_over" || failure.kind === "access_revoked");
    }
  });
});
