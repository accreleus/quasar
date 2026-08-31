import { describe, expect, it } from "vitest";
import { swapOutcome } from "./swapOutcome";

const running = (state_detail: string | null) => ({
  state: "running" as const,
  state_detail,
  error_message: null,
});

describe("swapOutcome", () => {
  it("is pending while state_detail is 'swapping'", () => {
    expect(swapOutcome(running("swapping"))).toEqual({ kind: "pending" });
  });

  it("is pending for an unrecognised running detail (matches the control plane's own default case)", () => {
    expect(swapOutcome(running("some future detail"))).toEqual({ kind: "pending" });
  });

  it("is success on 'swap complete'", () => {
    expect(swapOutcome(running("swap complete"))).toEqual({ kind: "success" });
  });

  it("extracts the reason from the agent's 'swap failed; rolled back: <reason>' detail", () => {
    expect(swapOutcome(running("swap failed; rolled back: new source container launch failed: boom"))).toEqual({
      kind: "error",
      message: "new source container launch failed: boom",
    });
  });

  it("falls back to a generic message when the rollback detail has no reason after the marker", () => {
    expect(swapOutcome(running("swap failed; rolled back:"))).toEqual({
      kind: "error",
      message: "The switch failed and the previous app was restored.",
    });
  });

  it("is an error on 'swap rejected' (agent unreachable / ack:false)", () => {
    expect(swapOutcome(running("swap rejected"))).toEqual({
      kind: "error",
      message: "The switch could not be started.",
    });
  });

  it("is a terminal error whenever state leaves running, regardless of detail text", () => {
    expect(
      swapOutcome({ state: "failed", state_detail: "swapping", error_message: "host_lost" }),
    ).toEqual({ kind: "error", message: "host_lost" });
  });

  it("falls back to a generic message when a non-running state carries no error_message", () => {
    expect(swapOutcome({ state: "failed", state_detail: null, error_message: null })).toEqual({
      kind: "error",
      message: "The session ended unexpectedly during the switch.",
    });
  });

  it("treats a null state_detail as pending while still running", () => {
    expect(swapOutcome(running(null))).toEqual({ kind: "pending" });
  });
});
