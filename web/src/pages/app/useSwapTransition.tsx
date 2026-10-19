// Quick-switch swap-completion state machine (see SessionSwapTransition.tsx for
// the screen, swapOutcome.ts for the pure poll classifier this wraps in a timer).
//
// Held from the moment the user picks an app until the SERVER confirms the
// commit ("swap complete", or "app booting"/"app presented" after "swapping")
// and, if the new app is still booting, until it presents. Never on the swap POST's 200, which only
// means the control plane accepted the request (state_detail "swapping",
// control-plane/internal/session/swapper.go `Swap`). Outcome arrives async via
// the agent's session_state callback, hence this poll.
//
// #139: the poll only exists from startSwap until the screen clears (phases
// "switching" and "booting"); torn down by itself/unmount/outcome, never a standing timer.
import { useCallback, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { getSession } from "../../api/library";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { shaderPercent } from "./launchProgress";
import { swapOutcome } from "./swapOutcome";

export type SwapTransitionState =
  | { phase: "switching"; appName: string }
  // Committed, the new app is booting: held until `app presented` (control-api.md
  // §Warm Steam, "Quick switch"), with Steam's shader progress when it reports one.
  | { phase: "booting"; appName: string; shaderPercent: number | null }
  | { phase: "error"; appName: string; message: string }
  | { phase: "timeout"; appName: string };

/** Poll cadence while a swap is in flight — the 5s health poll is too coarse
 *  against an operation measured at 7.2s end to end on the reference host. */
const SWAP_POLL_MS = 1_000;

/** Client give-up bound, above the agent's longest swap: an in-place one is
 *  IN_PLACE_QUIT_BOUND 94 s (covers Steam's 60 s exit cloud sync) +
 *  IN_PLACE_LAUNCH_BOUND 5 s = 99 s (node-agent/src/session/runner.rs). The rest
 *  is slack for dispatch and a poll round-trip. Escape hatch only. */
const CLIENT_SWAP_TIMEOUT_MS = 110_000;

/** Most the screen stays up after the commit waiting for `app presented`. Fail-open. */
const BOOT_HOLD_MAX_MS = 180_000;

/** How long a terminal transition stays on screen before auto-clearing to
 *  reveal the actual session — long enough to read, short enough not to trap
 *  the user behind an unclosable full-bleed screen. */
const DISMISS_AFTER_MS = 3_500;

export interface UseSwapTransitionArgs {
  authToken: string | null | undefined;
  sessionId: string | undefined;
  /** Fired exactly once, only once the server has confirmed the swap — never
   *  optimistically. `appId` is the session's freshly-committed `app_id`. */
  onCommitted: (appId: string, appName: string) => void;
  /** Informational toast sink (SessionPage's existing sessionAlerts host). */
  onToast: (node: ReactNode) => void;
}

export interface UseSwapTransitionResult {
  /** null when no swap is in flight and none just finished — nothing to show. */
  transition: SwapTransitionState | null;
  /** Call the instant the user picks a target app, before the request is even
   *  sent — this is what "held from the moment the user picks" means. */
  startSwap: (target: { id: string; name: string }) => void;
  /** Call when the swap REQUEST itself was rejected (ownership/entitlement/
   *  host-mismatch, or a network failure) — the server never entered
   *  "swapping", so there is nothing to poll for; this is terminal immediately. */
  rejectSwap: (message: string) => void;
}

export function useSwapTransition({
  authToken,
  sessionId,
  onCommitted,
  onToast,
}: UseSwapTransitionArgs): UseSwapTransitionResult {
  const [transition, setTransition] = useState<SwapTransitionState | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Guards a tick whose GET outlives clearInterval when superseded/torn down.
  const activeRef = useRef(false);

  const stopPolling = useCallback(() => {
    if (intervalRef.current != null) {
      clearInterval(intervalRef.current);
      intervalRef.current = null;
    }
    activeRef.current = false;
  }, []);

  // Must not let a late tick setState after unmount, nor leave an interval running.
  useEffect(() => stopPolling, [stopPolling]);

  const startSwap = useCallback(
    (target: { id: string; name: string }) => {
      // Supersede any stray previous poll — the rail disables every tile while
      // one swap is in flight, so this is defensive, not a normal path.
      stopPolling();
      activeRef.current = true;
      setTransition({ phase: "switching", appName: target.name });

      const deadline = Date.now() + CLIENT_SWAP_TIMEOUT_MS;

      // ROOT CAUSE of the two-swap-in-a-row defect: swapper.Swap() runs
      // entitlement/reservation/home checks BEFORE committing
      // state_detail="swapping" (control-plane/internal/session/swapper.go),
      // so a poll tick can land in a window where GET /v1/sessions/{id} still
      // returns the PREVIOUS swap's terminal state_detail even though a new
      // swap was just requested — indistinguishable on the wire from this
      // swap's own outcome.
      //
      // Fix: `armed` tracks whether THIS swap's poll has seen "swapping"
      // committed at least once (swapper.go always sets it before dispatching
      // to the agent; only the agent's callback later writes a terminal
      // detail) — a terminal-looking read before that is provably stale data
      // from the previous swap. Scoped to this closure so it resets on every
      // startSwap call.
      let armed = false;
      // After the commit the poll only waits for `app presented`.
      let committed = false;
      let holdDeadline = 0;
      // Shown when the screen clears, not at the commit: the hold screen covers the toast.
      const nowPlaying = (
        <>
          <b>Now playing {target.name}.</b> Stream quality unchanged.
        </>
      );

      const tick = async () => {
        if (!activeRef.current) return;
        if (!authToken || !sessionId) {
          stopPolling();
          return;
        }
        if (committed && Date.now() >= holdDeadline) {
          stopPolling();
          setTransition(null);
          return;
        }
        if (!committed && Date.now() >= deadline) {
          stopPolling();
          setTransition({ phase: "timeout", appName: target.name });
          onToast(
            <>
              Switching to <b>{target.name}</b> is taking longer than expected. It may still
              finish. If the host never picked it up, the session will end shortly.
            </>,
          );
          return;
        }
        try {
          const { session } = await getSession(authToken, sessionId);
          if (!activeRef.current) return; // cancelled/superseded while this GET was in flight
          if (committed) {
            // Anything but booting (presented, ended, odd detail) lets the stream show.
            if (session.state === "running" && session.state_detail === "app booting") {
              const pct = shaderPercent(session);
              setTransition((prev) =>
                prev?.phase === "booting" && prev.shaderPercent === pct
                  ? prev
                  : { phase: "booting", appName: target.name, shaderPercent: pct },
              );
            } else {
              stopPolling();
              setTransition(null);
              if (session.state === "running" && session.state_detail === "app presented") onToast(nowPlaying);
            }
            return;
          }
          if (session.state === "running" && session.state_detail === "swapping") armed = true;
          const outcome = swapOutcome(session, armed);
          if (outcome.kind === "pending") return; // keep polling
          if (!armed) return; // terminal-looking, but from the PREVIOUS swap — see above; keep polling
          if (outcome.kind === "success") {
            onCommitted(session.app_id, target.name);
            if (session.state_detail === "app booting") {
              committed = true;
              holdDeadline = Date.now() + BOOT_HOLD_MAX_MS;
              setTransition({ phase: "booting", appName: target.name, shaderPercent: shaderPercent(session) });
            } else {
              stopPolling();
              setTransition(null);
              onToast(nowPlaying);
            }
          } else {
            stopPolling();
            setTransition({ phase: "error", appName: target.name, message: outcome.message });
            onToast(<>{outcome.message}</>);
          }
        } catch (err) {
          // Transient poll hiccup — keep trying rather than failing the whole swap.
          reportBestEffortFailure("silent-debug", "session: swap poll", err);
        }
      };

      void tick();
      intervalRef.current = setInterval(() => void tick(), SWAP_POLL_MS);
    },
    [authToken, sessionId, onCommitted, onToast, stopPolling],
  );

  const rejectSwap = useCallback(
    (message: string) => {
      stopPolling();
      setTransition((prev) => ({ phase: "error", appName: prev?.appName ?? "", message }));
      onToast(<>{message}</>);
    },
    [onToast, stopPolling],
  );

  // Auto-dismiss a terminal transition after a readable pause.
  useEffect(() => {
    // The poll ends the other two phases itself.
    if (!transition || transition.phase === "switching" || transition.phase === "booting") return;
    const t = setTimeout(() => setTransition(null), DISMISS_AFTER_MS);
    return () => clearTimeout(t);
  }, [transition]);

  return { transition, startSwap, rejectSwap };
}
