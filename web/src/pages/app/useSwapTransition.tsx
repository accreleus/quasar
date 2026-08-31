// Quick-switch swap-completion state machine (see SessionSwapTransition.tsx for
// the screen, swapOutcome.ts for the pure poll classifier this wraps in a timer).
//
// Held from the moment the user picks an app until the SERVER reports
// state_detail "swap complete" — never on the swap POST's 200, which only
// means the control plane accepted the request (state_detail "swapping",
// control-plane/internal/session/swapper.go `Swap`). Outcome arrives async via
// the agent's session_state callback, hence this poll.
//
// #139: poll only exists while phase === "switching"; started by startSwap,
// torn down by itself/unmount/terminal outcome — never a standing timer.
import { useCallback, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { getSession } from "../../api/library";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { swapOutcome } from "./swapOutcome";

export type SwapTransitionState =
  | { phase: "switching"; appName: string }
  | { phase: "error"; appName: string; message: string }
  | { phase: "timeout"; appName: string };

/** Poll cadence while a swap is in flight — the 5s health poll is too coarse
 *  against an operation measured at 7.2s end to end on the reference host. */
const SWAP_POLL_MS = 1_000;

/** Client give-up bound. Node-agent's own swap budget (node-agent/src/session/
 * runner.rs) is SWAP_COMPOSITOR_TIMEOUT (20s fixed) + swap_app_ready_timeout
 * (45s default, `QUASAR_SWAP_APP_READY_TIMEOUT_MS`) = 65s before the agent
 * itself rolls back. 75s adds ~10s slack for control-plane dispatch/ack + poll
 * round-trip, so this should never fire before the server answers — pure
 * escape hatch for a dropped poll / unreachable control plane. A host raising
 * `QUASAR_SWAP_APP_READY_TIMEOUT_MS` above default can still hit this on a
 * slow title even though the agent would have succeeded — known limitation. */
const CLIENT_SWAP_TIMEOUT_MS = 75_000;

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

      const tick = async () => {
        if (!activeRef.current) return;
        if (!authToken || !sessionId) {
          stopPolling();
          return;
        }
        if (Date.now() >= deadline) {
          stopPolling();
          setTransition({ phase: "timeout", appName: target.name });
          onToast(
            <>
              Switching to <b>{target.name}</b> is taking longer than expected. The session is
              unaffected — check back in a moment.
            </>,
          );
          return;
        }
        try {
          const { session } = await getSession(authToken, sessionId);
          if (!activeRef.current) return; // cancelled/superseded while this GET was in flight
          if (session.state === "running" && session.state_detail === "swapping") armed = true;
          const outcome = swapOutcome(session);
          if (outcome.kind === "pending") return; // keep polling
          if (!armed) return; // terminal-looking, but from the PREVIOUS swap — see above; keep polling
          stopPolling();
          if (outcome.kind === "success") {
            onCommitted(session.app_id, target.name);
            setTransition(null);
            onToast(
              <>
                <b>Now playing {target.name}.</b> Stream quality unchanged.
              </>,
            );
          } else {
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
    if (!transition || transition.phase === "switching") return;
    const t = setTimeout(() => setTransition(null), DISMISS_AFTER_MS);
    return () => clearTimeout(t);
  }, [transition]);

  return { transition, startSwap, rejectSwap };
}
