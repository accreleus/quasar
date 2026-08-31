// Session polling — health/host-lost/launch-progress (AS10-06, #484 §3.2/§3.3).
// Extracted verbatim out of SessionPage as a behaviour-preserving move: same
// poll cadences, fail-open rules, and setState ordering.
//
// `externalSize`/`externalResizeSupported` are also written from OUTSIDE this
// hook, by useDisplayPatch.ts on ack/revert (same PATCH endpoint the poll
// below reads) — their setters are returned raw for that hook to call
// directly, same pattern as `setLaunchFailure` (SessionPage's reconnect path).
import { useCallback, useEffect, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { getSession } from "../../api/library";
import type { Session } from "../../api/types";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { healthBanner, type HealthBanner } from "./streamHealth";
import { launchFailureFromSession, type LaunchFailure } from "./sessionFailure";

/** #484 §3.3: cap on how long the loader waits for "app presented" after
 * "app booting" before revealing anyway. Build-time constant — the design
 * rejected extending frozen `protocol/signaling.md` for this. Independent of
 * the server-side `QUASAR_APP_BOOT_TIMEOUT_SECS` (default 300s); not read
 * from it. */
const QUASAR_APP_PRESENT_REVEAL_MAX_MS = 180_000;

export interface SessionStatus {
  /** Set to true when a host-offline reap is detected (state=failed, state_detail=host_lost). */
  hostLost: boolean;
  /** AS10-06: the latest computed stream-health banner (null = healthy / none). */
  health: HealthBanner | null;
  /** Task 13: session.started_at, for the drawer header's elapsed timer. */
  startedAt: string | null;
  /** The session's pinned stream WxH, from the 5s poll. Null until the first poll answers. */
  polledStreamSize: { w: number; h: number } | null;
  /** Adaptive external resolution (§D5/§D6): what is ENCODED right now; null = still at launch size. */
  externalSize: { w: number; h: number } | null;
  /** Raw setter — also called by useDisplayPatch on ack/revert (see module header). */
  setExternalSize: Dispatch<SetStateAction<{ w: number; h: number } | null>>;
  /** The server's ordered rung list; null until the first poll answers. */
  streamRungs: [number, number][] | null;
  /** undefined (= supported) unless told otherwise. */
  externalResizeSupported: boolean | undefined;
  /** Raw setter — also called by useDisplayPatch on a 409 external_resize_unsupported. */
  setExternalResizeSupported: Dispatch<SetStateAction<boolean | undefined>>;
  /** ABR resolution ladder (T6b): who owns the external size right now. */
  externalOwner: "auto" | "pinned" | undefined;
  /** UX assessment §2.2: the loader's substage copy while a launch is still in progress. */
  launchDetail: string | null;
  /** UX assessment §2.2: the loader's terminal verdict. Sticky once set. */
  launchFailure: LaunchFailure | null;
  /** Raw setter — also called by SessionPage's WebRTC reconnect path on a failed reconnect. */
  setLaunchFailure: Dispatch<SetStateAction<LaunchFailure | null>>;
  /** #484 §3.2: has the app committed its first presented frame (or do we not need to wait for it)? */
  appPresented: boolean;
  /** #482: has a host accepted this session (`host_id` set, or state past `assigned`)? Sticky. */
  hostAssigned: boolean;
  /** #482: has the control plane reported `state === "running"`? Sticky. */
  sessionRunning: boolean;
  /** Poll once to find out why the WebRTC session dropped — called by
   * SessionPage's onStatus on a signaling-relay 4500 / host-offline / ICE failure. */
  pollHostLost: () => Promise<void>;
}

export function useSessionStatus(
  authToken: string | null | undefined,
  sessionId: string | undefined,
  stopping: boolean,
): SessionStatus {
  const [launchDetail, setLaunchDetail] = useState<string | null>("scheduling host");
  // Sticky once set; derived only from the control plane's session record
  // (launch poll below), never a client-side guess.
  const [launchFailure, setLaunchFailure] = useState<LaunchFailure | null>(null);
  // #484 §3.2: flips true on the first `running` tick whose detail isn't
  // literally "app booting" — covers both an app session reaching "app
  // presented" and a no-app/older-agent session on its first running tick
  // (fail-open rule 1: a stuck loader must be impossible; costs at most one
  // extra ~1s poll tick, matching the design's "≤1s" reveal-latency bound).
  const [appPresented, setAppPresented] = useState(false);
  const [hostLost, setHostLost] = useState(false);
  const [health, setHealth] = useState<HealthBanner | null>(null);
  // Not carried in router state (a fresh launch's started_at is still null
  // there); populated from the 5s health poll below once the server reports it.
  const [startedAt, setStartedAt] = useState<string | null>(null);
  // Null until first poll answers; the router-state tier string covers that
  // gap so display controls are usable immediately after launch.
  const [polledStreamSize, setPolledStreamSize] = useState<{ w: number; h: number } | null>(null);
  // Adaptive external resolution (§D5/§D6), all three off the same 5s poll:
  // streamRungs is null until first poll answers (client 16:9 fallback covers
  // a pre-amendment control plane in the meantime).
  const [externalSize, setExternalSize] = useState<{ w: number; h: number } | null>(null);
  const [streamRungs, setStreamRungs] = useState<[number, number][] | null>(null);
  const [externalResizeSupported, setExternalResizeSupported] = useState<boolean | undefined>(
    undefined,
  );
  // ABR resolution ladder (T6b), off the same poll as externalSize but set
  // UNCONDITIONALLY (not sticky): there's no acked ownership to preserve
  // between polls, the ladder can flip it at any time.
  const [externalOwner, setExternalOwner] = useState<"auto" | "pinned" | undefined>(undefined);
  // #482: both STICKY — once accepted/running stays true, since the launch
  // poll below stops once running. hostAssigned reads host_id (the field the
  // UI wrongly ignored per the #482 report), falling back to lifecycle state.
  const [hostAssigned, setHostAssigned] = useState(false);
  const [sessionRunning, setSessionRunning] = useState(false);

  // Called from both pollers; sticky and safe only because this hook's
  // lifetime is one session's (remounts per session id) — a shared/cached
  // store would need to reset these on session-id change.
  const notePlacement = useCallback((session: Session) => {
    const placed =
      session.host_id != null ||
      session.state === "starting" ||
      session.state === "running";
    if (placed) setHostAssigned(true);
    if (session.state === "running") setSessionRunning(true);
  }, []);

  // When the WebRTC session drops unexpectedly, poll once to find out why — if
  // the server says failed/host_lost we show a clear "host went offline" message.
  const pollHostLost = useCallback(async () => {
    if (!authToken || !sessionId || stopping) return;
    try {
      const { session } = await getSession(authToken, sessionId);
      if (session.state === "failed" && session.state_detail === "host_lost") {
        setHostLost(true);
      }
    } catch (err) {
      // best-effort; the status bar message is already informative
      reportBestEffortFailure("silent-debug", "session: host-lost poll", err);
    }
  }, [authToken, sessionId, stopping]);

  // AS10-06: 5s poll for computed stream health. Server computes, client only
  // displays and lets the user choose.
  useEffect(() => {
    if (!authToken || !sessionId) return;
    let cancelled = false;
    const poll = async () => {
      try {
        const { session } = await getSession(authToken, sessionId);
        if (cancelled) return;
        if (session.state === "failed" && session.state_detail === "host_lost") {
          setHostLost(true);
          return;
        }
        notePlacement(session as Session);
        // #484 §3.2: suppress the non-critical banners while the app hasn't
        // presented — healthBanner() itself decides which kinds that covers.
        setHealth(healthBanner(session as Session, appPresented));
        setStartedAt(session.started_at);
        // Stream WxH ceiling for `PATCH /v1/sessions/{id}/display` validation.
        // Read from `session.stream` (contract: "always the truth"), not the
        // router-state tier string (absent on a resumed/deep-linked session).
        //
        // Guarded and LAST in the tick: an unexpectedly absent block must not
        // silently take the health banner and elapsed timer down with it.
        const s = session.stream as
          | {
              width?: number;
              height?: number;
              external_width?: number;
              external_height?: number;
              rungs?: [number, number][];
              external_resize_supported?: boolean;
              external_owner?: "auto" | "pinned";
            }
          | undefined;
        if (s?.width && s.height) setPolledStreamSize({ w: s.width, h: s.height });
        if (s?.rungs) setStreamRungs(s.rungs);
        if (s?.external_resize_supported != null) {
          setExternalResizeSupported(s.external_resize_supported);
        }
        // Set only on presence (not sticky-reset): absence means either "back
        // at launch" or "control plane forgot after restart" (§D5) — keep the
        // last-acked value rather than flapping the control under the user.
        if (s?.external_width && s.external_height) {
          setExternalSize({ w: s.external_width, h: s.external_height });
        }
        // Unconditional: absent = "no known owner", so the drawer must stop
        // rendering the "Auto ·" chip immediately.
        setExternalOwner(s?.external_owner);
      } catch (err) {
        // best-effort; banner just won't update this tick
        reportBestEffortFailure("silent-debug", "session: health poll", err);
      }
    };
    void poll();
    const t = setInterval(() => void poll(), 5000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [authToken, sessionId, appPresented]);

  // Polls quickly during launch for agent substage detail. #484 §3.2: used to
  // stop at channelOpen — 30-50s before a cold app draws anything — so this is
  // gated on `appPresented` instead, reusing the same state_detail poll (no
  // new endpoint) to drive the loader's post-transport gate.
  useEffect(() => {
    if (!authToken || !sessionId || appPresented) return;
    let cancelled = false;
    const poll = async () => {
      try {
        const { session } = await getSession(authToken, sessionId);
        if (cancelled) return;
        // The control plane owns "this launch is over". Ask it, don't guess.
        const verdict = launchFailureFromSession(session);
        if (verdict) {
          setLaunchFailure((prev) => prev ?? verdict);
          return;
        }
        notePlacement(session);
        if (session.state === "assigned") setLaunchDetail("scheduling host");
        else if (session.state === "starting") setLaunchDetail(session.state_detail ?? "building pipeline");
        else if (session.state === "running" && session.state_detail === "app booting") setLaunchDetail("app booting");
        else if (session.state === "running" && session.state_detail === "app presented") setAppPresented(true);
        else if (session.state === "running") {
          // No app container, or an older agent that never sends "app
          // booting" — nothing further to wait for. Reveals on the SAME
          // tick that stops sending progress copy (fail-open rule 1).
          setLaunchDetail(null);
          setAppPresented(true);
        }
      } catch (err) {
        reportBestEffortFailure("silent-debug", "session: launch progress", err);
      }
    };
    void poll();
    const timer = setInterval(() => void poll(), 1_000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [authToken, sessionId, appPresented]);

  // #484 §3.2 fail-open rule 2: bounds how long the loader stays gated on
  // `appPresented` — armed from mount, disarmed once it flips true. A
  // well-behaved launch flips within ~1s of `running`, so this rarely fires.
  // On expiry: reveal anyway + console warning — a stuck loader must be
  // impossible.
  const revealCapTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (appPresented) {
      if (revealCapTimerRef.current) {
        clearTimeout(revealCapTimerRef.current);
        revealCapTimerRef.current = null;
      }
      return;
    }
    revealCapTimerRef.current = setTimeout(() => {
      console.warn(
        `session: app-present reveal cap (${QUASAR_APP_PRESENT_REVEAL_MAX_MS}ms) reached — revealing the stream anyway`,
      );
      setAppPresented(true);
    }, QUASAR_APP_PRESENT_REVEAL_MAX_MS);
    return () => {
      if (revealCapTimerRef.current) {
        clearTimeout(revealCapTimerRef.current);
        revealCapTimerRef.current = null;
      }
    };
  }, [appPresented]);

  return {
    hostLost,
    health,
    startedAt,
    polledStreamSize,
    externalSize,
    setExternalSize,
    streamRungs,
    externalResizeSupported,
    setExternalResizeSupported,
    externalOwner,
    launchDetail,
    launchFailure,
    setLaunchFailure,
    appPresented,
    hostAssigned,
    sessionRunning,
    pollHostLost,
  };
}
