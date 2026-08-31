/**
 * The launch loader's phase machine (handoff-v3 §D, design spec §5.3).
 *
 * Pure, and driven by signals rather than copy: the four steps are the four
 * things that actually have to happen — the signalling socket opening, the
 * video peer connection reaching `connected`, the first decoded frame, and the
 * input DataChannel opening. The old machine read the phase out of the status
 * strings the transport happened to emit, so a string it did not recognise
 * dropped the whole screen back to "finding a host" (#482).
 *
 * One index drives both the headline word and the glyph rail, exactly as the
 * mock does, so the two can never disagree about where the handshake is.
 */

import type { LaunchPhase } from "./launchStall";

/** The accent word under the verb, one per step. Index 4 is the handoff. */
export const PHASE_WORDS = [
  "connection",
  "secure path",
  "video channel",
  "input capture",
  "your stream",
] as const;

/** The control-plane states that mean the session has not started running yet
 *  (control-api.md §4). Anything else — including `running` — is past them. */
const NOT_YET_RUNNING: readonly string[] = ["pending", "assigned", "starting"];

/**
 * `app_launch_state === "starting"` is the app process launched but not yet
 * presenting (#484 §3.2). Transport can be fully up 30-50 s before a cold app
 * draws anything, so this holds the rail one step short of the handoff.
 */
const APP_BOOTING = "starting";

export interface PhaseInputs {
  /** Control-plane `state` for this session; undefined before the first poll. */
  state?: string;
  /** The signalling WebSocket is open. */
  wsOpen: boolean;
  /** The video PeerConnection's ICE path is up. */
  pcConnected: boolean;
  /** The <video> element has decoded a frame. */
  firstFrame: boolean;
  /** The `input` DataChannel is open. */
  inputOpen: boolean;
  /** Control-plane `app_launch_state` (advisory; absent means unknown). */
  appLaunchState?: string;
  /** Title of the app being launched, for the app-boot copy. */
  appName?: string;
}

export interface LoaderPhase {
  /** 0-3 = the step now in flight; 4 = every step done, hand off. */
  step: number;
  /** The accent word on the second line. */
  word: string;
  /** Indices of the completed steps — always `[0..step-1]`. */
  done: number[];
  /** The first line. Never animates. */
  verb: string;
}

export function derivePhase(input: PhaseInputs): LoaderPhase {
  // Signals do not arrive in order, and a control-plane poll lags the transport
  // by up to a second. A step counts as done when its signal or any later one
  // is true, which makes the sequence monotone by construction — the rail can
  // never walk backwards on a late-arriving earlier signal.
  const inputDone = input.inputOpen;
  const frameDone = input.firstFrame || inputDone;
  const pcDone = input.pcConnected || frameDone;
  // §D: the first step is also still in flight while the control plane has not
  // started the session, however early the socket happens to open.
  const notRunning = input.state !== undefined && NOT_YET_RUNNING.includes(input.state);
  const wsDone = (input.wsOpen && !notRunning) || pcDone;

  const flags = [wsDone, pcDone, frameDone, inputDone];
  const firstPending = flags.indexOf(false);
  let step = firstPending === -1 ? 4 : firstPending;

  const booting = input.appLaunchState === APP_BOOTING;
  // The app arc: transport being up is not the app being up.
  if (booting && step > 3) step = 3;

  const appArc = booting && step === 3;
  return {
    step,
    word: appArc ? (input.appName ?? "the game") : PHASE_WORDS[step],
    done: [0, 1, 2, 3].slice(0, step),
    verb: appArc ? "Starting" : step === 4 ? "Opening" : "Establishing",
  };
}

/**
 * The step's phase in `launchStall.ts`'s vocabulary — the budgets and the
 * stall copy stay keyed on that, so this is the only bridge.
 *
 * Step 0 covers everything before the socket opens (scheduling and image
 * preparation); it reports `host`, and `resolveStall` re-points it to the game
 * once the control plane says a host owns the session, so a placed launch is
 * never told nothing picked it up. Steps 1-2 are the media path. Step 3 is
 * where a cold app boot waits, so it takes the app budget.
 */
export function stallPhaseForStep(step: number): LaunchPhase {
  if (step <= 0) return "host";
  if (step <= 2) return "stream";
  return "app";
}
