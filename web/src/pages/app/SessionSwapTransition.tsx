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
import { useEffect, useState } from "react";
import { QuasarMark } from "../../components/QuasarMark";
import type { SwapTransitionState } from "./useSwapTransition";

export interface SessionSwapTransitionProps {
  transition: SwapTransitionState | null;
}

// Longer than `.switcher`'s .35s opacity fade (session.css).
const FADE_OUT_MS = 400;

export function SessionSwapTransition({ transition }: SessionSwapTransitionProps) {
  const show = transition != null;
  // The shell stays mounted so the fade and the live region work; its content
  // is only in the document while a swap is in flight or fading out, so an idle
  // session's page text carries none of it.
  const [held, setHeld] = useState(transition);
  if (transition && transition !== held) setHeld(transition);
  useEffect(() => {
    if (transition) return;
    const t = setTimeout(() => setHeld(null), FADE_OUT_MS);
    return () => clearTimeout(t);
  }, [transition]);
  return (
    <div
      className={`switcher${show ? " show" : ""}`}
      role="status"
      aria-live="polite"
      aria-hidden={show ? undefined : "true"}
    >
      {held && (
        <>
          <QuasarMark size={72} />
          <div className="sw-nm">{held.appName}</div>
          {held.phase === "error" || held.phase === "timeout" ? (
            <div className="sw-err">
              {held.phase === "timeout"
                ? "Still waiting on a confirmation from the host. The switch may still finish. If the host never picked it up, the session will end shortly."
                : held.message}
            </div>
          ) : (
            <div className="sw-sub">Starting…</div>
          )}
        </>
      )}
    </div>
  );
}
