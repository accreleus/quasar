// The loader's phase machine (handoff-v3 §D). Pure: every input is a real
// transport signal or a control-plane field, and the same index paints the
// headline word and the glyph rail, so the two can never disagree.

import { describe, expect, it } from "vitest";
import { derivePhase, stallPhaseForStep } from "./loaderPhases";

const base = {
  state: "assigned",
  wsOpen: false,
  pcConnected: false,
  firstFrame: false,
  inputOpen: false,
  appLaunchState: undefined,
};

describe("derivePhase", () => {
  it("walks the four steps", () => {
    expect(derivePhase(base)).toEqual({
      step: 0,
      word: "connection",
      done: [],
      verb: "Establishing",
    });
    expect(derivePhase({ ...base, state: "running", wsOpen: true })).toEqual({
      step: 1,
      word: "secure path",
      done: [0],
      verb: "Establishing",
    });
    expect(derivePhase({ ...base, state: "running", wsOpen: true, pcConnected: true })).toEqual({
      step: 2,
      word: "video channel",
      done: [0, 1],
      verb: "Establishing",
    });
    expect(
      derivePhase({
        ...base,
        state: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
      }),
    ).toEqual({ step: 3, word: "input capture", done: [0, 1, 2], verb: "Establishing" });
    expect(
      derivePhase({
        ...base,
        state: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
        inputOpen: true,
      }),
    ).toEqual({ step: 4, word: "your stream", done: [0, 1, 2, 3], verb: "Opening" });
  });

  // The socket opening is not the whole of step 1: a session the control plane
  // has not started yet is still "connection", per §D's phase list.
  it("holds the first step while the control plane has not started the session", () => {
    expect(derivePhase({ ...base, state: "starting", wsOpen: true }).step).toBe(0);
    expect(derivePhase({ ...base, state: "pending", wsOpen: true }).step).toBe(0);
  });

  // Signals do not arrive in order (a snapshot can carry pcConnected before the
  // socket-open callback has been recorded). A later signal implies the earlier
  // ones, so the rail never walks backwards.
  it("treats a step as done when any later signal is true", () => {
    expect(derivePhase({ ...base, state: "starting", pcConnected: true })).toEqual({
      step: 2,
      word: "video channel",
      done: [0, 1],
      verb: "Establishing",
    });
    expect(derivePhase({ ...base, state: "assigned", inputOpen: true }).done).toEqual([0, 1, 2, 3]);
  });

  it("app booting holds step 3 with the app name", () => {
    expect(
      derivePhase({
        ...base,
        state: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
        appLaunchState: "starting",
        appName: "Blender",
      }).word,
    ).toBe("Blender");
  });

  // #484 §3.2: transport up is not the app being up. The input channel opening
  // must not promote the loader to "your stream" while the app is still booting.
  it("does not reach the handoff step while the app is still booting", () => {
    const booting = derivePhase({
      ...base,
      state: "running",
      wsOpen: true,
      pcConnected: true,
      firstFrame: true,
      inputOpen: true,
      appLaunchState: "starting",
      appName: "Blender",
    });
    expect(booting.step).toBe(3);
    expect(booting.verb).toBe("Starting");
    expect(booting.done).toEqual([0, 1, 2]);
  });

  it("names the game generically when the app has no title", () => {
    expect(
      derivePhase({ ...base, state: "running", wsOpen: true, appLaunchState: "starting" }).word,
    ).toBe("secure path");
    expect(
      derivePhase({
        ...base,
        state: "running",
        wsOpen: true,
        pcConnected: true,
        firstFrame: true,
        appLaunchState: "starting",
      }).word,
    ).toBe("the game");
  });

  // A launch state that is not "starting" is not a boot hold — game_running and
  // an unrecognised value both mean "nothing further to wait for".
  it("only holds on the starting launch state", () => {
    for (const appLaunchState of ["game_running", "client_only", "something_new", undefined]) {
      expect(
        derivePhase({
          ...base,
          state: "running",
          wsOpen: true,
          pcConnected: true,
          firstFrame: true,
          inputOpen: true,
          appLaunchState,
        }).step,
      ).toBe(4);
    }
  });
});

describe("stallPhaseForStep", () => {
  // launchStall.ts owns the budgets; this is the only mapping from the new
  // step index onto its phase vocabulary.
  it("maps the four steps onto the stall vocabulary", () => {
    expect(stallPhaseForStep(0)).toBe("host");
    expect(stallPhaseForStep(1)).toBe("stream");
    expect(stallPhaseForStep(2)).toBe("stream");
    expect(stallPhaseForStep(3)).toBe("app");
    expect(stallPhaseForStep(4)).toBe("app");
  });
});
