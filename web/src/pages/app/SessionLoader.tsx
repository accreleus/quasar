/**
 * The launch screen: full-bleed dark scene on top of the session video
 * (handoff-v3 §D). Visuals are the accretion quasar plus a four-step glyph
 * rail; the steps are real transport signals, not status prose
 * (`loaderPhases.ts`).
 *
 * Caller (SessionPage) must: pass the four transport signals from the session
 * runtime, set `streaming` when the reveal gate opens, and call useDarkLock().
 */

import { useCallback, useEffect, useReducer, useRef, useState } from "react";
import "./SessionLoader.css";
import { Button } from "../../components/Button";
import { QuasarMark } from "../../components/QuasarMark";
import { CollapsibleLogTail } from "../../components/SessionFailureDetail";
import type { LaunchFailure } from "./sessionFailure";
import { AccretionVisual } from "./AccretionVisual";
import { GlyphRail } from "./GlyphRail";
import { derivePhase, stallPhaseForStep } from "./loaderPhases";
import {
  resolveStall,
  statusPhase,
  type LaunchPhase,
  type TransportState,
} from "./launchStall";

// Stall vocabulary/decision lives in launchStall.ts; re-exported here (original home).
export { PHASE_STALL_COPY, PHASE_STALL_MS, TRANSPORT_STALL_MS } from "./launchStall";
export type { LaunchPhase, TransportState } from "./launchStall";

/** §D: `is-locking` (status drop, quasar collapse, aperture) before the fade. */
const LOCK_TO_STREAM_MS = 1180;
/** The word leaves, the text is replaced, the word comes back. */
const STAGE_SWAP_MS = 110;

interface SessionLoaderProps {
  statusMsg: string;
  /** The page's reveal gate (channel open and the app has presented). The only
   *  thing that starts the handoff. */
  streaming: boolean;
  appName?: string;
  /** Terminal verdict (control plane or exhausted transport recovery). Non-null is the
   *  ONLY thing that makes the loader terminal — it never decides that on its own. */
  failure?: LaunchFailure | null;
  /** Stop-and-return: tears the session down server-side and leaves the page. */
  onExit: () => void;
  /** Test seam — overrides the per-phase stall budget. */
  stallMs?: number;
  /** #482: host_id set server-side. Once true, no stall may claim nothing picked it up. */
  hostAssigned?: boolean;
  /** #482: control plane reports `state === "running"`. */
  sessionRunning?: boolean;
  /** #482: video PeerConnection's live ICE state; `failed`/`disconnected` (or running-but-
   *  not-connected) turns the stall copy from a scheduling story into a transport one. */
  iceState?: TransportState | null;
  /** Control-plane `state` / `app_launch_state` for this session. */
  sessionState?: string;
  appLaunchState?: string;
  // The four transport signals the phase machine walks (sessionRuntime.ts).
  wsOpen?: boolean;
  pcConnected?: boolean;
  firstFrame?: boolean;
  inputOpen?: boolean;
}

export function SessionLoader({
  statusMsg,
  streaming,
  appName,
  failure = null,
  onExit,
  stallMs,
  hostAssigned = false,
  sessionRunning = false,
  iceState = null,
  sessionState,
  appLaunchState,
  wsOpen = false,
  pcConnected = false,
  firstFrame = false,
  inputOpen = false,
}: SessionLoaderProps) {
  const { step, word, verb } = derivePhase({
    state: sessionState,
    wsOpen,
    pcConnected,
    firstFrame,
    inputOpen,
    appLaunchState,
    appName,
  });

  // Scene: idle → locking (quasar collapse + aperture) → streaming (fade out).
  const [scene, setScene] = useState<"idle" | "locking" | "streaming">("idle");
  // Only `streaming` (the page's reveal gate, bounded by the reveal cap) starts
  // the handoff: step 4 must never, since all four transport signals can be true
  // while the app has not presented, and revealing then shows an empty
  // compositor scene (#484 §3.2). Latched, and the lock timer is cleared only on
  // unmount — the gate can retract on a late poll, and a handoff that un-starts
  // strands the scene mid-lock at opacity 0.
  const [handoffReady, setHandoffReady] = useState(false);
  const lockTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (!streaming || handoffReady) return;
    setHandoffReady(true);
    setScene("locking");
    lockTimerRef.current = setTimeout(() => setScene("streaming"), LOCK_TO_STREAM_MS);
  }, [streaming, handoffReady]);

  useEffect(
    () => () => {
      if (lockTimerRef.current) clearTimeout(lockTimerRef.current);
    },
    [],
  );

  // The word leaves, the copy and the rail are replaced together, the word comes
  // back: one visible index paints both, so the headline and the rail can never
  // disagree about where the handshake is. The verb never animates.
  const [visible, setVisible] = useState({ step, word, verb });
  const [changing, setChanging] = useState(false);
  const swapKey = `${step}:${word}:${verb}`;
  const pendingRef = useRef(swapKey);
  useEffect(() => {
    if (swapKey === pendingRef.current) return;
    pendingRef.current = swapKey;
    setChanging(true);
    const t = setTimeout(() => {
      setVisible({ step, word, verb });
      setChanging(false);
    }, STAGE_SWAP_MS);
    return () => clearTimeout(t);
  }, [swapKey, step, word, verb]);

  // Stall detection: two clocks, one decision (resolveStall, launchStall.ts).
  // PHASE clock keys to the launch phase, not mount: it restarts on every phase
  // change so a stall is reported against the phase that actually stopped.
  // TRANSPORT clock (#482) starts when the control plane first reports the
  // session running and is much shorter: a running host with no media arriving
  // is a media problem regardless of phase budget.
  // A stall is never terminal — the loader keeps running; "Keep waiting" re-arms
  // one more full budget on both clocks.
  const [waitAgain, setWaitAgain] = useState(0);
  // The status string still knows things no signal does (image pull, app boot),
  // so it keeps the budget where it recognises the phase; the step supplies the
  // one it cannot, which is how far the transport actually got.
  const fromStatus = statusPhase(statusMsg);
  const launchPhase: LaunchPhase = fromStatus === "host" ? stallPhaseForStep(step) : fromStatus;

  const phaseStartRef = useRef(Date.now());
  const transportStartRef = useRef<number | null>(sessionRunning ? Date.now() : null);
  const phaseKeyRef = useRef(`${launchPhase}:${waitAgain}`);
  const phaseKey = `${launchPhase}:${waitAgain}`;
  const waitAgainRef = useRef(waitAgain);
  // Re-armed during render, not an effect: an effect runs after the render that
  // already saw the new phase, flashing the previous phase's stall copy for one paint.
  if (phaseKeyRef.current !== phaseKey) {
    phaseKeyRef.current = phaseKey;
    phaseStartRef.current = Date.now();
  }
  // TRANSPORT clock does NOT re-arm on a phase flip: ICE negotiation moves the
  // derived phase several times, and keying to phase would push the verdict out
  // indefinitely on exactly the launches it exists to catch. Re-arms only on
  // "keep waiting" and the session becoming running.
  if (waitAgainRef.current !== waitAgain) {
    waitAgainRef.current = waitAgain;
    transportStartRef.current = sessionRunning ? Date.now() : null;
  }
  if (sessionRunning && transportStartRef.current === null) transportStartRef.current = Date.now();
  if (!sessionRunning) transportStartRef.current = null;

  // Wall-time clocks; this only forces the re-render that reads them.
  const [, tick] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    if (handoffReady || failure) return;
    const t = setInterval(() => tick(), 1_000);
    return () => clearInterval(t);
  }, [handoffReady, failure]);

  const now = Date.now();
  const verdict =
    handoffReady || failure
      ? null
      : resolveStall({
          phase: launchPhase,
          hostAssigned,
          sessionRunning,
          iceState,
          phaseElapsedMs: now - phaseStartRef.current,
          transportElapsedMs:
            transportStartRef.current === null ? 0 : now - transportStartRef.current,
          stallMs,
        });
  const stalled = verdict != null;

  // Stall block's primary action is destructive (onExit tears the session down).
  // A verdict that appears/retracts/reappears re-mounts the block; autoFocus on
  // every re-mount risks a stray Enter killing a healthy launch during a transient
  // dip, so focus once per stall generation. Ref write is in an effect, not render,
  // so a double-invoked render can't swallow the first focus.
  const stallGeneration = verdict ? `${phaseKey}:${verdict.kind}` : null;
  const focusedGenerationRef = useRef<string | null>(null);
  const focusStallExit = stallGeneration !== null && focusedGenerationRef.current !== stallGeneration;
  useEffect(() => {
    if (stallGeneration !== null) focusedGenerationRef.current = stallGeneration;
  }, [stallGeneration]);

  // "Keep waiting" can't help a `failed` ICE path (resolveStall reports it on
  // sight, so the wait would end instantly) — cancel is the only honest choice.
  const canKeepWaiting = !(verdict?.kind === "transport" && iceState === "failed");

  const keepWaiting = useCallback(() => setWaitAgain((n) => n + 1), []);

  const terminal = failure != null;

  // `inert` is not in React 18's JSX attribute set; setting it on the node keeps
  // the faded-out scene out of focus order and the accessibility tree together.
  const rootRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = rootRef.current;
    if (!el) return;
    if (scene === "streaming") el.setAttribute("inert", "");
    else el.removeAttribute("inert");
  }, [scene]);

  // Kept in DOM briefly after streaming so the fade animation can play out
  const rootClass = [
    "sl-root",
    scene === "locking" ? "is-locking" : "",
    scene === "streaming" ? "is-streaming" : "",
    terminal ? "is-failed" : "",
    stalled ? "is-stalled" : "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div
      ref={rootRef}
      className={rootClass}
      role={terminal ? "alert" : "status"}
      aria-live="polite"
      aria-label={terminal ? "Session could not start" : "Establishing stream connection"}
      // Hidden from AT once streaming (video takes over)
      aria-hidden={scene === "streaming" ? "true" : undefined}
    >
      <header className="sl-lockup">
        <QuasarMark size={38} className="sl-mark" />
        <div className="sl-wordmark">Quasar</div>
      </header>

      {!terminal && <AccretionVisual />}

      {/* Both the progress and the failure copy live inside .sl-root (fixed,
          z-index 50): a failure UI rendered outside it is unreachable underneath. */}
      <section className="sl-status">
        {terminal ? (
          <>
            <h1>
              <span className="sl-verb">{failure.title}</span>
            </h1>
            <p className="sl-fail-msg">{failure.message}</p>
            {failure.detail && <p className="sl-fail-detail">{failure.detail}</p>}
            {failure.logTail && (
              <div className="note warn sl-fail-log">
                <CollapsibleLogTail text={failure.logTail} />
              </div>
            )}
            {/* autoFocus not a ref: block mounts exactly when the loader goes terminal,
                so mount-time focus lands keyboard/AT users on the way out. */}
            <div className="sl-actions">
              <Button variant="primary" autoFocus onClick={onExit}>
                Back to library
              </Button>
            </div>
          </>
        ) : (
          <>
            <h1>
              <span className="sl-verb">{visible.verb}</span>
              <span className={changing ? "sl-stage changing" : "sl-stage"}>{visible.word}</span>
            </h1>
            <div className="sl-status-foot">
              <GlyphRail step={visible.step} />
            </div>

            {/* Not terminal — progress copy above stays; this just names the stuck phase. */}
            {verdict && (
              <div className="sl-stall" role="alert">
                <p className="sl-stall-title">{verdict.title}</p>
                <p className="sl-stall-msg">{verdict.message}</p>
                <div className="sl-actions">
                  <Button variant="primary" autoFocus={focusStallExit} onClick={onExit}>
                    Cancel and go back
                  </Button>
                  {canKeepWaiting && (
                    <Button variant="ghost" onClick={keepWaiting}>
                      Keep waiting
                    </Button>
                  )}
                </div>
              </div>
            )}
          </>
        )}
      </section>

      <div className="sl-aperture" aria-hidden="true" />
    </div>
  );
}
