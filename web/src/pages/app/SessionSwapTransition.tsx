// Quick-switch transition screen. The v3 mock swaps the HUD bar's content in
// place instead (design_handoff_v3/screens/session-overlay-v3.html, `.swapping`
// — ported in hud/HudBar.tsx) and has no equivalent for the error and timeout
// phases, which are this screen's job.
// Pure render — see useSwapTransition.ts for the state machine. `transition`:
//   "switching" — swap accepted, waiting on the server's confirmation
//   "error"     — the swap failed; the previous app is (still) running
//   "timeout"   — the client gave up waiting for a server answer
//
// No focusable content by design; doesn't touch SessionDrawer's
// aria-hidden/focus logic.
import { QuasarMark } from "../../components/QuasarMark";
import type { SwapTransitionState } from "./useSwapTransition";

export interface SessionSwapTransitionProps {
  transition: SwapTransitionState | null;
}

export function SessionSwapTransition({ transition }: SessionSwapTransitionProps) {
  const show = transition != null;
  return (
    <div
      className={`switcher${show ? " show" : ""}`}
      role="status"
      aria-live="polite"
      aria-hidden={show ? undefined : "true"}
    >
      <QuasarMark size={72} />
      <div className="sw-nm">{transition?.appName ?? ""}</div>
      {transition && (transition.phase === "error" || transition.phase === "timeout") ? (
        <div className="sw-err">
          {transition.phase === "timeout"
            ? "Still waiting on a confirmation from the host. The session is unaffected — check back in a moment."
            : transition.message}
        </div>
      ) : (
        <div className="sw-sub">Starting…</div>
      )}
    </div>
  );
}
