// SessionSwapController — the ENTIRE quick-switch wiring in one place.
//
// Exists because an earlier shape threaded useSwapTransition's
// transition/startSwap/rejectSwap by hand into the strip and the drawer
// separately — on live hardware the transition screen never appeared and the
// strip updated identity as if the swap had already committed, because the
// click reached a different, stale copy of the wiring than the one rendered.
// Isolated component tests couldn't catch it (each supplies its own wiring).
//
// So the wiring is ONE component, imported once — guarded by
// swapWiring.test.tsx, which renders THIS component and drives a real click.
import type { ReactNode } from "react";
import { GamesPane } from "./hud/panes/GamesPane";
import { SessionSwapTransition } from "./SessionSwapTransition";
import { useSwapTransition } from "./useSwapTransition";

export interface SessionSwapControllerProps {
  sessionId: string | undefined;
  authToken: string | null | undefined;
  /** The session's current app — only its `id` gates whether a rail renders
   *  at all (mirrors the old SessionPage ternary: no session/app id yet, no
   *  rail to offer). */
  currentApp: { id?: string; name: string };
  /** Fired exactly once, only once the server confirms the swap — never
   *  optimistically. See useSwapTransition.ts. */
  onCommitted: (appId: string, appName: string) => void;
  onToast: (node: ReactNode) => void;
  /** Extra side effect to run the instant a swap starts — SessionPage uses
   *  this to collapse the HUD shelf. Kept separate from the transition state
   *  machine itself, which knows nothing about the HUD. */
  onSwapStart?: () => void;
  /** Render prop: caller gets only the two derived values its children need
   *  (the Games pane, the bar's swappingTo). The transition screen is rendered here,
   *  so no second call site can render it with a stale `transition` value. */
  children: (args: { quickSwitch: ReactNode | null; swappingTo: string | null }) => ReactNode;
}

export function SessionSwapController({
  sessionId,
  authToken,
  currentApp,
  onCommitted,
  onToast,
  onSwapStart,
  children,
}: SessionSwapControllerProps) {
  const { transition, startSwap, rejectSwap } = useSwapTransition({
    authToken,
    sessionId,
    onCommitted,
    onToast,
  });

  const quickSwitch =
    sessionId && currentApp.id ? (
      <GamesPane
        sessionId={sessionId}
        currentAppId={currentApp.id}
        swapPending={transition?.phase === "switching"}
        onSwapStart={(target) => {
          onSwapStart?.();
          startSwap(target);
        }}
        onSwapRejected={rejectSwap}
      />
    ) : null;

  const swappingTo = transition?.phase === "switching" ? transition.appName : null;

  return (
    <>
      {children({ quickSwitch, swappingTo })}
      {/* z-index 60, above the HUD (20); onSwapStart above collapses the
          shelf. Held until the poll observes the server's own outcome, never
          on the request alone — see useSwapTransition.ts. */}
      <SessionSwapTransition transition={transition} />
    </>
  );
}
