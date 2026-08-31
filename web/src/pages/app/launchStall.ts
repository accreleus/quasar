// What a stalled launch is allowed to blame (#482). A scheduling reason may only
// be shown while the session is genuinely unplaced; once it is running with no
// ICE path, the transport is the suspect (`statusPhase` falls back to "host" for
// unknown status strings, which used to blame GPU capacity for a media failure).
// Pure: the loader owns the clocks, this owns the decision.

/** `"app"` (#484 §3.2): compositor streaming but the app hasn't drawn a frame
 *  yet (`state_detail === "app booting"`). */
export type LaunchPhase = "host" | "game" | "stream" | "app";

/**
 * The phase named by the live status string. The status still knows things no
 * transport signal does (image pull, app boot), so it keeps the budget where it
 * recognises the phase; `stallPhaseForStep` supplies the one it cannot. It does
 * not drive any visible copy — the rail and the headline come from the signals
 * (`loaderPhases.ts`).
 *
 * Matches the strings the agent and QuasarSession emit (webrtc/session.ts).
 */
export function statusPhase(statusMsg: string): LaunchPhase {
  // #484 §3.2: container up, app hasn't presented yet. Checked first so it wins
  // over a stale transport status sharing the same string.
  if (statusMsg.includes("app booting")) return "app";
  if (statusMsg.includes("scheduling host")) return "host";
  if (
    statusMsg.includes("preparing resources") ||
    statusMsg.includes("image") ||
    statusMsg.includes("container started") ||
    statusMsg.includes("first frame")
  ) {
    return "game";
  }
  if (
    statusMsg.includes("media pipeline") ||
    statusMsg.includes("answer sent") ||
    statusMsg.includes("awaiting ICE") ||
    statusMsg.includes("connected") ||
    statusMsg.includes("ws open") ||
    statusMsg.includes("waiting for offer")
  ) {
    return "stream";
  }
  // Not an error: an unrecognised status is the initial state, covered by the
  // host budget.
  return "host";
}

/** Stall budget per PHASE, not per launch: a launch that keeps advancing never
 *  stalls. Generous on purpose — a false "stuck" on a cold image pull would be
 *  the new defect. */
export const PHASE_STALL_MS: Record<LaunchPhase, number> = {
  host: 30_000,
  game: 120_000,
  stream: 45_000,
  // matches `game`: a cold app boot is the same order as container prep
  app: 120_000,
};

/** Media-path budget for a running session. Much shorter than any phase budget:
 *  LAN ICE completes in tens of ms, and the failure this catches never self-heals. */
export const TRANSPORT_STALL_MS = 12_000;

/** What a stall in each phase means, in the user's terms. */
export const PHASE_STALL_COPY: Record<LaunchPhase, { title: string; message: string }> = {
  host: {
    title: "Still looking for a host",
    message:
      "No host has picked this session up yet. Every GPU may be busy with another session, or the host agent may be offline.",
  },
  game: {
    title: "Still starting the game",
    message:
      "The host has this session but the game hasn't finished starting. A first launch downloads and unpacks the game image, which can take a while.",
  },
  stream: {
    title: "Still opening the stream",
    message:
      "The game is running but the video connection hasn't completed. This is usually a network or firewall problem between this device and the host.",
  },
  // not the `stream` copy: the stream IS open here, the app hasn't drawn yet
  app: {
    title: "Still starting the game",
    message:
      "The stream is connected and the game is starting. A first launch has to set up its files, which can take a minute.",
  },
};

/** The causes an operator can actually check, most likely first. */
export const TRANSPORT_STALL_COPY = {
  title: "The host is ready, but the video path is not",
  message:
    "This session is running on the host, but your browser and the host could not establish a media connection. The usual causes are that this device and the host are on different networks, that a firewall is blocking UDP, or that no STUN or TURN server is configured. Without one, media only works on a shared LAN or VPN; see the deployment guide's networking notes.",
} as const;

/** The ICE connection states the decision reads. Mirrors RTCIceConnectionState. */
export type TransportState =
  | "new"
  | "checking"
  | "connected"
  | "completed"
  | "failed"
  | "disconnected"
  | "closed";

const ICE_UP: readonly TransportState[] = ["connected", "completed"];
/** Only `failed` reports on sight. `disconnected` is transient (the
 *  RecoveryController restarts ICE through it, so a WiFi blip would flash the
 *  verdict); left out of ICE_UP too, so sitting in it still trips the transport
 *  clock after the normal budget. */
const ICE_BROKEN: readonly TransportState[] = ["failed"];

export interface StallInputs {
  /** The phase derived from the live status string (`statusPhase`). */
  phase: LaunchPhase;
  /** Does the control plane say a host owns this session (`host_id` set)? */
  hostAssigned: boolean;
  /** Does the control plane say `state === "running"`? */
  sessionRunning: boolean;
  /** Latest video-PeerConnection ICE state; null before the PC exists. */
  iceState: TransportState | null;
  /** ms the launch has spent in `phase`. */
  phaseElapsedMs: number;
  /**
   * ms since the control plane first reported the session running. 0 when it
   * has not (so the transport clock has not started).
   */
  transportElapsedMs: number;
  /** Caller/test seam — overrides the per-phase budget (not the transport one). */
  stallMs?: number;
}

export interface StallVerdict {
  /** `"transport"` verdicts are about the media path, never about scheduling. */
  kind: "phase" | "transport";
  title: string;
  message: string;
}

const transportVerdict: StallVerdict = { kind: "transport", ...TRANSPORT_STALL_COPY };

const phaseVerdict = (phase: LaunchPhase): StallVerdict => ({
  kind: "phase",
  ...PHASE_STALL_COPY[phase],
});

/** The stall verdict, or null while inside budget. Order matters: terminal ICE
 *  `failed` first, then running-without-ICE past the transport budget, and only
 *  then the per-phase budget — with a `host` verdict re-pointed once a host is
 *  known to own the session. */
export function resolveStall(input: StallInputs): StallVerdict | null {
  const { phase, hostAssigned, sessionRunning, iceState, phaseElapsedMs, transportElapsedMs } = input;

  // 1. Terminal ICE (`failed` only), reported at once.
  if (iceState !== null && ICE_BROKEN.includes(iceState)) return transportVerdict;

  const iceUp = iceState !== null && ICE_UP.includes(iceState);

  // 2. Running, not connected, past the transport budget.
  if (sessionRunning && !iceUp && transportElapsedMs >= TRANSPORT_STALL_MS) return transportVerdict;

  // 3. The per-phase budget.
  const budget = input.stallMs ?? PHASE_STALL_MS[phase];
  if (phaseElapsedMs < budget) return null;

  if (phase === "host" && hostAssigned) {
    // A host owns it, so the scheduling copy is a lie. Which truth applies
    // depends on how far the session got: running means the media path is the
    // only thing left to blame; anything earlier is still bring-up.
    return sessionRunning ? transportVerdict : phaseVerdict("game");
  }

  return phaseVerdict(phase);
}
