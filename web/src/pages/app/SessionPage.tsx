// Live session view: browser↔node webrtcbin through the control-plane signaling
// relay (protocol/signaling.md P1-D).
//
// Signaling coords arrive via router state from AppHome's launch action; the
// token is single-use. A missing pair re-mints via
// `POST /v1/sessions/{id}/signaling-token` (#524) — see the resume effect below.
//
// Re-render isolation (#139): high-frequency telemetry lives only in sibling
// subtrees of <video> (the HUD bar's readout, the stats and input panes),
// subscribed via a stable ref fan-out — SessionPage never holds the per-tick
// snapshot, so the video/input subtree re-renders only on status/channelOpen/
// pointerLocked/stopping/hostLost/health, never per telemetry tick.

import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import "../../styles/session.css";
import "../../styles/hud.css";
import { getSession, mintSignalingToken, stopSession } from "../../api/library";
import { ApiError } from "../../api/client";
import type { ICEServer } from "../../api/types";
import { useAuth } from "../../auth/context";
import {
  keyboardLockSupported,
  pointerLockSupported,
  touchLookSupported,
} from "../../input/capture";
import {
  createSessionRuntime,
  IDLE_SNAPSHOT,
  type SessionRuntime,
  type SessionRuntimeSnapshot,
} from "./sessionRuntime";
import { MicCapture, MicCaptureError, withUntrustedCertContext } from "../../webrtc/mic";
import { probeCertificateTrust, type CertTrust } from "../../lib/certTrust";
import { useToast } from "../../components/Toast";
import { playoutOverride } from "../../webrtc/playout";
import type { TelemetrySnapshot } from "../../webrtc/telemetry";
import { Button } from "../../components/Button";
import { Hud, type HudHandle } from "./hud/Hud";
import { fallbackStreamRungs } from "./StreamResolutionControl";
import { SessionSwapController } from "./SessionSwapController";
import {
  useSessionToast,
  SessionToastHost,
  SessionBanner,
  SessionBannerHost,
} from "./sessionAlerts";
import { SessionLoader } from "./SessionLoader";
import { takenOverFailure, unreachableFailure } from "./sessionFailure";
import { useOverlaySummon } from "./useOverlaySummon";
import { useSessionStatus } from "./useSessionStatus";
import { useDisplayPatch } from "./useDisplayPatch";
import {
  loadDisplayPreferences,
  saveDisplayPreferences,
  type ScalingMode,
} from "../../settings/displayPreferences";
import { reportBestEffortFailure } from "../../lib/reportBestEffortFailure";
import { useScreenWakeLock } from "../../lib/screenWakeLock";
import { useDarkLock } from "../../settings/ThemeContext";
import { percentileFraction, recommendation } from "./sessionSummary";

/**
 * Stream size out of the tier string ("1920×1080@60" → {w:1920,h:1080}).
 * Null on a session opened without a tier (resume, deep link) — display
 * controls hide rather than guess a ceiling `PATCH .../display` would reject.
 * Separator is U+00D7 (×), matching the producer.
 */
export function parseTierSize(tier: string | undefined): { w: number; h: number } | null {
  const m = /^(\d+)×(\d+)/.exec(tier ?? "");
  if (!m) return null;
  const w = parseInt(m[1], 10);
  const h = parseInt(m[2], 10);
  return w > 0 && h > 0 ? { w, h } : null;
}

/**
 * #524: session states no fresh signaling envelope can rescue; these bounce to
 * the library instead of minting. `stopping` included: mint would likely 409.
 * Pre-`running` states are absent — the loader already renders launch progress.
 * `failed` is absent too: the poller's `launchFailureFromSession` verdict
 * already renders a specific failure screen for it; bouncing here would race
 * that with a worse, generic answer.
 */
const RESUME_DEAD_STATES: ReadonlySet<string> = new Set(["stopping", "stopped"]);

interface SessionState {
  signalingUrl: string;
  signalingToken: string;
  /** #509: STUN/TURN servers from `signaling.ice_servers`. Absent means host
   *  candidates only. */
  iceServers?: ICEServer[];
  appName?: string;
  /** Task 13: for the drawer's quick-switch rail (the "currently playing" tile). */
  appId?: string;
  /** AS-05: tier-selected playout₀ (ms); start point for the adaptive controller. */
  playout0Ms?: number;
  /** #146: assigned stream tier ("1920×1080@60"), shown in the diag panel. */
  tier?: string;
  /** Multi-codec §4: server-resolved session codec (wire id: h264|h265|av1). */
  resolvedCodec?: string;
  /** Mic spec §3.5: whether the server GRANTED a mic m-line for this session. */
  micGranted?: boolean;
  /** ?playout= override from the App page URL, carried through navigation. */
  playoutOverride?: number | null;
}

export function SessionPage() {
  const { id: sessionId } = useParams<{ id: string }>();
  const { state } = useLocation() as { state: SessionState | null };
  const { token: authToken } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();

  // UI-08: dark-lock this page — loader and live session must always be dark.
  useDarkLock();

  const videoRef = useRef<HTMLVideoElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  // The HUD owns whether it is open and on which section; this page only asks
  // it to open (summon chord, stage click, touch button) and to collapse.
  const hudRef = useRef<HudHandle>(null);
  // Mic spec §3.4: start() only from a user gesture; stop() releases the device
  // so the OS recording indicator goes off.
  const micRef = useRef<MicCapture | null>(null);
  if (micRef.current === null) micRef.current = new MicCapture();

  // Stable telemetry fan-out: isolated consumers (SessionStrip, DiagPanel,
  // gamepad readout, the `quality` subscriber below) each register a setState
  // handler here. SessionPage never holds the per-tick snapshot itself, keeping
  // the video/input subtree off the 1 Hz telemetry path (#139).
  const telemetrySubsRef = useRef<Set<(snap: TelemetrySnapshot) => void>>(new Set());

  const [signalCoords, setSignalCoords] = useState(() => ({
    url: state?.signalingUrl ?? "",
    token: state?.signalingToken ?? "",
    replacement: false,
    // #509 — kept in the same state object as its url/token so re-seating
    // coords can never leave a stale ICE list behind.
    iceServers: state?.iceServers ?? [],
  }));
  // Two booleans, not one (see input/capture.ts header): `pointerLocked` is the
  // real lock, gating the floating session-menu button (a touch user's only way
  // out); `inputCaptured` is what the chrome reflects (strip/drawer labels).
  // They diverge on a device with no Pointer Lock API.
  const [stopping, setStopping] = useState(false);
  // AS10-14: scaling mode (persisted) and display refresh estimate (from telemetry).
  const [scalingMode, setScalingMode] = useState<ScalingMode>(
    () => loadDisplayPreferences().scalingMode,
  );
  // Mic spec §3.4: never persisted/restored — every session starts mic-off.
  const [micOn, setMicOn] = useState(false);
  const [micBusy, setMicBusy] = useState(false);
  // Certificate-trust verdict for THIS origin (lib/certTrust.ts), "unknown"
  // until the async probe lands. Advisory only: it re-words the Esc hint and
  // the mic permission error for the bypassed-certificate case, which no
  // synchronous feature detect can see (isSecureContext is true there).
  const [certTrust, setCertTrust] = useState<CertTrust>("unknown");
  // Ref mirror so the keyboard-lock-refused callback (wired once per runtime
  // generation) reads the live verdict, not the value captured at wiring time.
  const certTrustRef = useRef<CertTrust>("unknown");
  useEffect(() => {
    let cancelled = false;
    void probeCertificateTrust().then((verdict) => {
      certTrustRef.current = verdict;
      if (!cancelled) setCertTrust(verdict);
    });
    return () => {
      cancelled = true;
    };
  }, []);
  // Sticky: keyboard.lock() was observed refused this session. Drives the
  // honest Esc wording even though keyboardLockSupported() (pure API presence)
  // says the feature exists.
  const [kbLockRefused, setKbLockRefused] = useState(false);
  // Currently-running app: seeded from router state, re-seated by a quick-switch
  // swap (final review, finding 3) — router state is write-once, and the
  // server's `app_id` still names the OLD app until the agent confirms the swap
  // (control-plane swapper.go), so neither is live identity on its own.
  const [currentApp, setCurrentApp] = useState<{ id?: string; name: string }>(() => ({
    id: state?.appId,
    name: state?.appName ?? "Session",
  }));

  // Resume without router state (#524): arriving via Resume/bookmark/reload
  // carries no router state, so read the session and, if still attachable,
  // mint via `POST /v1/sessions/{id}/signaling-token`. Must bounce with a
  // toast, never silently, on 404 / terminal / mint 409 "not reconnectable".
  //
  // `replacement` must always be true here: it maps to `requestOfferOnOpen`
  // (sessionRuntime L1 → webrtc/session.ts); false would sit waiting on a host
  // offer that was already sent to the now-gone peer, hanging forever. A true
  // one's `restart_ice` is what makes media flow again; on the still-booting
  // arm the worst case is a benign double offer, absorbed by #505's suppression.
  //
  // The mint is single-use, so this must run at most once per mount — enforced
  // by the ref, not the effect deps, because StrictMode runs the effect body
  // twice and a second mint would burn the first one's token. Ignored once
  // sessionRuntime's own replacement-mint path (L4) re-seats `signalCoords`.
  //
  // Two tabs, last attach wins, loser is told (#526): the relay's Register
  // still displaces the incumbent peer, but now via explicit takeover — control
  // plane closes the displaced socket with 4410 (not 1000), transport maps it
  // to recovery phase `superseded`, sessionRuntime L6 renders
  // `takenOverFailure()` instead of minting.
  //
  // Name stays "Session" on resume: `GET /v1/apps` isn't fetched for one
  // string; `app_id` is seated below so the quick-switch rail resolves it.
  const resumeAttemptedRef = useRef(false);
  // Must not toast/navigate from an unmounted page on a route change mid-GET.
  // A `let cancelled` in this effect's own cleanup would be wrong under
  // StrictMode: the effect tears down and reruns, so the once-only (via the
  // ref) async chain would find itself pre-cancelled. Tracks component
  // lifetime via an empty-dep effect instead (same as AppHomeNext's `mountedRef`).
  const resumeMountedRef = useRef(true);
  useEffect(() => {
    resumeMountedRef.current = true;
    return () => {
      resumeMountedRef.current = false;
    };
  }, []);
  useEffect(() => {
    if (signalCoords.url && signalCoords.token) return;
    if (resumeAttemptedRef.current) return;
    resumeAttemptedRef.current = true;

    const cancelled = () => !resumeMountedRef.current;

    const bounce = (body: string) => {
      if (cancelled()) return;
      addToast({ variant: "danger", title: "Can't resume that session", body });
      navigate("/app", { replace: true });
    };

    if (!authToken || !sessionId) {
      navigate("/app", { replace: true });
      return;
    }

    void (async () => {
      try {
        const { session } = await getSession(authToken, sessionId);
        if (cancelled()) return;
        if (RESUME_DEAD_STATES.has(session.state)) {
          bounce("That session has already ended. Launch it again from your library.");
          return;
        }
        // A `failed` session is left to the poller's launch-failure verdict
        // (see RESUME_DEAD_STATES) — no mint, no bounce, no toast.
        if (session.state === "failed") return;
        const res = await mintSignalingToken(authToken, sessionId);
        if (cancelled()) return;
        const signaling = res?.signaling;
        // Same shape check the runtime applies: a 2xx whose body isn't a
        // signaling envelope is a failed resume, not a TypeError at the user.
        if (!signaling?.url || !signaling?.token) {
          bounce("The server did not return usable signaling coordinates.");
          return;
        }
        // Seat the app id so the strip and the quick-switch rail are not stuck
        // on an empty identity.
        setCurrentApp((prev) => (prev.id ? prev : { id: session.app_id, name: prev.name }));
        setSignalCoords({
          url: signaling.url,
          token: signaling.token,
          replacement: true,
          iceServers: signaling.ice_servers ?? [],
        });
      } catch (err) {
        if (cancelled()) return;
        // Only SERVER-authored text is ever shown (same rule as the runtime's
        // mint failure); anything else is a client fault the user can do
        // nothing with.
        if (err instanceof ApiError) {
          bounce(
            err.status === 404
              ? "That session no longer exists."
              : err.message || "The server refused to reconnect this session.",
          );
        } else {
          reportBestEffortFailure("silent-debug", "session: resume without router state", err);
          bounce("Could not reach the server to reconnect this session.");
        }
      }
    })();
  }, [signalCoords, authToken, sessionId, navigate, addToast]);

  // Session status polling (host-lost, AS10-06 health, launch progress, #484
  // §3.2 reveal-cap) lives in useSessionStatus.ts. `pollHostLost` and
  // `setLaunchFailure` are called from the WebRTC mount effect below: a
  // signaling-relay disconnect polls once to explain itself, a failed
  // reconnect posts its own terminal verdict.
  const {
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
  } = useSessionStatus(authToken, sessionId, stopping);

  // sessionRuntime.ts: one runtime instance per transport generation (law L1)
  // — a replacement token re-seats signalCoords, destroying this runtime and
  // creating the next.
  const runtimeRef = useRef<SessionRuntime | null>(null);
  const [runtimeSnap, setRuntimeSnap] = useState<SessionRuntimeSnapshot>(IDLE_SNAPSHOT);
  // The page holds no second copy of what the transport owns.
  const {
    status,
    channelOpen,
    pointerLocked,
    inputCaptured,
    recovery,
    clientUnsupported,
    iceState,
    wsOpen,
    pcConnected,
    firstFrame,
  } = runtimeSnap;

  useEffect(() => {
    if (!signalCoords.url || !signalCoords.token || !videoRef.current) return;

    const rt = createSessionRuntime({
      sessionId: sessionId ?? "",
      authToken: authToken ?? "",
      signaling: signalCoords,
      videoEl: videoRef.current,
      playout0Ms: state?.playout0Ms,
      // `?playout=` pins the buffer and keeps the adaptive controller off.
      playoutOverrideMs: state?.playoutOverride ?? playoutOverride(),
      // Parsed from tier "W×H@fps"; undefined falls back to an absolute decode ceiling.
      tierFps: state?.tier ? parseInt(state.tier.split("@")[1] ?? "", 10) || undefined : undefined,
      callbacks: {
        // capture.ts owns the chord while the pointer is locked: it releases
        // the lock, swallows the key, then calls back here. The HUD's own
        // listener answers the unlocked case (hudKeys.ts).
        onSummonOverlay: () => hudRef.current?.open(),
        // Never silently degrade: a refused lock means Esc quits the game, so
        // say it before the user finds out the hard way. The bypassed-cert case
        // does NOT arrive here — lock() resolves there (lib/certTrust.ts).
        onKeyboardLockRefused: (error) => {
          reportBestEffortFailure("silent-debug", "session: keyboard.lock", error);
          setKbLockRefused(true);
          addToast({
            variant: "danger",
            title: "Esc will leave the game",
            body:
              certTrustRef.current === "untrusted-cert"
                ? "The browser refused to hand Esc to the game — this address uses " +
                  "a certificate your device doesn't trust. Download the server " +
                  "certificate from /v1/tls/certificate.pem, trust it, then reload."
                : "The browser refused to hand Esc to the game (Keyboard Lock), so " +
                  "pressing Esc releases input instead of reaching the game. " +
                  "Re-entering fullscreen usually restores it.",
          });
        },
        onDisconnectSuspected: () => void pollHostLost(),
        onReplacementSignaling: ({ url, token, iceServers }) =>
          setSignalCoords({ url, token, replacement: true, iceServers }),
        onReconnectFailed: (detail) =>
          setLaunchFailure((prev) => prev ?? unreachableFailure(detail)),
        // #526: another attach won this session. Terminal HERE only — the
        // session and the app are still running, in the tab that took it. The
        // page states it and stops; it must not re-mint (sessionRuntime L6).
        onSessionTakenOver: () => setLaunchFailure((prev) => prev ?? takenOverFailure()),
      },
    });
    runtimeRef.current = rt;
    // Forwards to the page's stable fan-out set so a child that registered in
    // its own effect (children mount first) is never dropped.
    const unsubscribeTelemetry = rt.registerTelemetry((snap) => {
      for (const fn of telemetrySubsRef.current) fn(snap);
    });
    const unsubscribe = rt.subscribe(() => setRuntimeSnap(rt.getSnapshot()));
    rt.start();
    setRuntimeSnap(rt.getSnapshot());

    return () => {
      unsubscribeTelemetry();
      unsubscribe();
      rt.destroy();
      runtimeRef.current = null;
      setRuntimeSnap(IDLE_SNAPSHOT);
    };
  }, [signalCoords]);

  // Mic spec §3.4: release the device and reset to off on transport
  // rebuild/teardown — never carried across a reconnect.
  useEffect(() => {
    return () => {
      micRef.current?.stop();
      setMicOn(false);
    };
  }, [signalCoords]);

  // Must toggle, not just request: the drawer's row relabels to "Release
  // input" while captured, and in fallback mode it's a primary way out.
  const handleGrab = useCallback(() => {
    if (!videoRef.current) return;
    // Unmute on user gesture (browsers require it for audio autoplay).
    videoRef.current.muted = false;
    void videoRef.current.play();

    // Synchronous truth, not last-rendered state — a gesture handler must not
    // read a stale render (sessionRuntime.isCaptured).
    const rt = runtimeRef.current;
    if (rt?.isCaptured()) {
      rt.release();
      return;
    }
    // engage() itself runs synchronously inside the gesture (requestPointerLock
    // requires that); only its RESULT is awaited. Null means the channel is not
    // open, and every control offering this is disabled in that state.
    const engaging = rt?.engage();
    if (!engaging) return;

    void engaging.then((result) => {
      if (result.mode === "fallback") {
        // On touch, gestures ARE the mouse (input/capture.ts) — this is the
        // only place a tablet player is told so. Without touch, say plainly
        // there's no Pointer Lock and no look-around, or it reads as a bug.
        addToast({
          variant: "info",
          title: "Input captured",
          body: touchLookSupported()
            ? "Drag to look, tap to click, press and hold for right-click, and drag " +
              "two fingers to scroll. Your keyboard and controller are going to the " +
              "game too. Tap the menu button to stop."
            : "This browser has no mouse capture (Pointer Lock) and no touchscreen, " +
              "so mouse look isn’t available here. Your keyboard, controller, clicks " +
              "and scrolling are going to the game. Press Esc or use the session " +
              "menu to stop.",
        });
        return;
      }
      if (result.mode === "failed") {
        // Must not silently degrade a lock-capable browser: say what happened.
        reportBestEffortFailure("silent-debug", "session: requestPointerLock", result.error);
        addToast({
          variant: "danger",
          title: "Couldn’t capture input",
          body:
            "Your browser refused to lock the mouse pointer. Click the picture and " +
            "try Capture input again.",
        });
      }
    });

    // The HUD collapses itself when capture engages (Hud.tsx): the picture is
    // the point of capturing, and a shelf over it is not.
  }, [addToast]);

  // Mic spec §3.4: enable runs from the button click and is the only path
  // that calls getUserMedia; disable detaches the sender track and stops the device.
  const handleToggleMic = useCallback(async () => {
    const mic = micRef.current;
    const sess = runtimeRef.current;
    if (!mic || micBusy) return;
    setMicBusy(true);
    try {
      if (micOn) {
        await sess?.detachMicTrack();
        mic.stop();
        setMicOn(false);
        return;
      }
      if (!sess?.hasMicSlot()) {
        addToast({
          variant: "danger",
          title: "Microphone unavailable",
          body: "This session did not negotiate a microphone channel. Relaunch the app to try again.",
        });
        return;
      }
      const track = await mic.start(loadDisplayPreferences().preferredMicDeviceId);
      // Device disappeared (unplug / OS revoke): fall back to off. The track is
      // already dead — MicCapture has released whatever remained.
      mic.onEnded = () => {
        setMicOn(false);
        void runtimeRef.current?.detachMicTrack();
        addToast({
          variant: "info",
          title: "Microphone disconnected",
          body: "The capture device went away, so the microphone was turned off.",
        });
      };
      await sess.attachMicTrack(track);
      setMicOn(true);
    } catch (err) {
      mic.stop();
      setMicOn(false);
      let detail =
        err instanceof MicCaptureError
          ? err.detail
          : { kind: "unknown" as const, title: "Microphone failed", message: "The microphone could not be started." };
      // Bypassed-certificate origins pass microphoneSupported() (secure
      // context is true there), so the denial arrives as permission-denied
      // with advice — "fix the address-bar permission" — the browser won't
      // honour on an origin it distrusts. Re-word it to the actionable truth.
      if (certTrustRef.current === "untrusted-cert") {
        detail = withUntrustedCertContext(detail);
      }
      addToast({ variant: "danger", title: detail.title, body: detail.message });
    } finally {
      setMicBusy(false);
    }
  }, [micOn, micBusy, addToast]);

  // Release the capture device on unmount — a mic left hot after leaving the
  // session page would keep the OS recording indicator lit.
  useEffect(() => {
    const mic = micRef.current;
    return () => {
      mic?.stop();
    };
  }, []);

  const handleStop = useCallback(async () => {
    setStopping(true);
    // Close WS + PC immediately so the user sees the stop is happening.
    // destroy() stops capture, telemetry, playout, bench and the tracer and
    // closes the transport with `bye` (this is an explicit stop, never a
    // replacement handoff — sessionRuntime L4).
    runtimeRef.current?.destroy();
    // Release the mic immediately on an explicit stop (spec §3.4).
    micRef.current?.stop();
    setMicOn(false);
    // Best-effort teardown on the server (non-blocking; navigate regardless).
    if (authToken && sessionId) {
      try {
        await stopSession(authToken, sessionId);
      } catch (err) {
        if (!(err instanceof ApiError)) console.warn("stop session:", err);
      }
    }
    const runtimeSummary = runtimeRef.current?.summary() ?? { elapsedMs: 0, fps: [], latency: [] };
    const fpsP50 = percentileFraction(runtimeSummary.fps, 0.5);
    const targetFps = state?.tier ? parseInt(state.tier.split("@")[1] ?? "", 10) || null : null;
    navigate("/app", {
      state: {
        sessionSummary: {
          appName: currentApp.name,
          durationSeconds: Math.max(1, Math.round(runtimeSummary.elapsedMs / 1_000)),
          fpsP50,
          fpsP95: percentileFraction(runtimeSummary.fps, 0.95),
          latencyP50Ms: percentileFraction(runtimeSummary.latency, 0.5),
          latencyP95Ms: percentileFraction(runtimeSummary.latency, 0.95),
          endReason: "Stopped by user",
          recommendation: recommendation(fpsP50, targetFps),
        },
      },
    });
  }, [authToken, sessionId, navigate, state, currentApp.name]);

  // Adding/removing a subscriber never re-renders SessionPage (mutates the
  // ref's Set only).
  const registerTelemetry = useCallback(
    (onUpdate: (snap: TelemetrySnapshot) => void) => {
      telemetrySubsRef.current.add(onUpdate);
      return () => {
        telemetrySubsRef.current.delete(onUpdate);
      };
    },
    [],
  );

  const { toast, push: pushToast } = useSessionToast();

  // The HUD owns its own open state (it is the only thing that can be open);
  // this page keeps a boolean only to dim the stage behind it. One class per
  // open/close is not a re-render path — telemetry never comes up here (#139).
  const [hudOpen, setHudOpen] = useState(false);

  // Bootstrap affordances (final review, finding 1): the HUD shelf is the only
  // route to "Controller & input" / display controls, reachable via the
  // keyboard chord (useOverlaySummon; capture.ts owns the captured case), a
  // stage click, or the touch summon button. None holds state or subscribes to
  // telemetry (#139).
  //
  // Must release() explicitly here: without Pointer Lock the summon button is
  // tappable during capture, and opening with the keyboard still captured
  // would fight Tab/Enter with the menu and leave the gamepad driving the game
  // behind it. No-op when already released (the desktop case).
  const openHud = useCallback(() => {
    runtimeRef.current?.release();
    hudRef.current?.open();
  }, []);
  useOverlaySummon(openHud);

  const handleStageClick = useCallback(() => {
    // Gated on capture state, not document.pointerLockElement: that property
    // is permanently null without Pointer Lock, which would throw the shelf
    // open on every tap meant for the game. The floating session-menu button
    // is the way out on those devices instead.
    hudRef.current?.stageClick();
  }, []);

  const handleRelease = useCallback(() => {
    runtimeRef.current?.release();
  }, []);

  // AS10-14: persist scaling mode on change; purely CSS, no stream resize.
  const handleScalingChange = useCallback((mode: ScalingMode) => {
    setScalingMode(mode);
    saveDisplayPreferences({ scalingMode: mode });
  }, []);

  // Render resolution + interface size (control-api.md session-display-update)
  // lives in useDisplayPatch.ts. `externalSize`/`externalResizeSupported` live
  // in useSessionStatus above (the 5s poll can also change them); this hook
  // gets their raw setters and writes them on ack/revert.
  const {
    renderSize,
    uiScale,
    displayBusy,
    streamChanging,
    handleRenderSizeChange,
    handleStreamSizeChange,
    handleUiScaleChange,
  } = useDisplayPatch({
    authToken,
    sessionId,
    addToast,
    setExternalSize,
    setExternalResizeSupported,
  });

  // AS10-14: request fullscreen on the video container; release pointer lock on exit.
  // Toggle: if already fullscreen, exit instead of re-requesting (no-op otherwise).
  const handleFullscreen = useCallback(() => {
    if (document.fullscreenElement) {
      document.exitFullscreen().catch((err: unknown) => {
        console.warn("exit fullscreen failed:", err);
      });
      return;
    }
    const el = containerRef.current;
    if (!el) return;
    el.requestFullscreen().catch((err: unknown) => {
      console.warn("fullscreen request failed:", err);
    });
  }, []);

  // Tracks fullscreen state for the drawer's fullscreen/exit-fullscreen button.
  const [fullscreen, setFullscreen] = useState(false);

  // AS10-14: on fullscreen exit, release pointer lock if held (correct order per spec:
  // fullscreen is released first by the browser, then we clean up pointer lock).
  useEffect(() => {
    const onFullscreenChange = () => {
      setFullscreen(!!document.fullscreenElement);
      if (!document.fullscreenElement && document.pointerLockElement) {
        document.exitPointerLock();
      }
      // Entering fullscreen while captured engages Keyboard Lock (Esc → game);
      // exiting disengages it. capture.ts owns the lock; we just nudge it.
      runtimeRef.current?.syncKeyboardLock();
    };
    document.addEventListener("fullscreenchange", onFullscreenChange);
    return () => document.removeEventListener("fullscreenchange", onFullscreenChange);
  }, []);

  // Live identity — follows a quick-switch swap (finding 3), unlike the
  // launch-time router state it is seeded from.
  const appTitle = currentApp.name;

  // How long the loader stays mounted after the reveal gate opens: the v3
  // handoff is 1180ms of lock (quasar collapse + aperture) then a 220ms fade.
  // Unmounting earlier cuts the white iris off mid-wipe.
  const LOADER_UNMOUNT_MS = 1_400;
  // #484 §3.2: the reveal gate is channelOpen AND appPresented, not
  // channelOpen alone — the compositor's transport comes up 30-50s before a
  // cold app draws anything, and revealing on transport alone is the defect.
  const revealReady = channelOpen && appPresented;
  const [loaderDone, setLoaderDone] = useState(false);
  const loaderDoneTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    if (revealReady && !loaderDone) {
      loaderDoneTimerRef.current = setTimeout(() => setLoaderDone(true), LOADER_UNMOUNT_MS);
    }
    return () => {
      if (loaderDoneTimerRef.current) clearTimeout(loaderDoneTimerRef.current);
    };
  }, [revealReady, loaderDone]);

  // #434: hold a screen wake lock while the session is LIVE. Liveness is the
  // page's existing notion — the input DataChannel being open is what every
  // other "we are streaming" decision here keys off (SessionStrip's
  // `streaming=`, the loader dismissal, the quality clamp) — minus the two
  // states where the picture is already gone. No second definition of "live".
  //
  // MUST stay above the hostLost early-return below: hooks cannot be
  // conditional. (A dead host also flips `hostLost`, which drops it anyway.)
  useScreenWakeLock(channelOpen && !stopping && !hostLost);

  // Host-lost overlay: replaces the normal session UI when the host crashed.
  if (hostLost) {
    return (
      <div className="session-root session-lost">
        <div className="session-lost-card">
          <h2>Host went offline</h2>
          <p className="muted">
            The game host lost its connection. Your session has ended — no data was
            lost on your end. Launch again and the scheduler will pick a healthy host.
          </p>
          <Button variant="primary" onClick={() => navigate("/app")}>
            Back to library
          </Button>
        </div>
      </div>
    );
  }

  // #85: the display-cadence warning used to be computed here from the tier
  // and `displayRefreshHz` alone. It now lives in StatsPane, which already
  // subscribes to the telemetry it needs to check whether frames are in fact
  // being dropped — the check the old predicate never made.

  // Poll is authoritative; the tier string is only the pre-first-poll seed.
  const streamSize = polledStreamSize ?? parseTierSize(state?.tier);

  // Server owns this list; the client-side 16:9 ladder is a stand-in until the
  // first poll answers (or for a control plane older than the amendment).
  const effectiveRungs =
    streamRungs ?? (streamSize ? fallbackStreamRungs(streamSize) : []);
  // Badge is about divergence from launch size; at launch the tier already says it.
  const badgeExternalSize =
    externalSize && streamSize && (externalSize.w !== streamSize.w || externalSize.h !== streamSize.h)
      ? externalSize
      : null;

  // Whether any of the three banner blocks below is on screen. The HUD takes
  // no banner-state input, so it is carried as a class on the shared ancestor
  // instead — `.session-root.banner-on` pushes a top-docked HUD down (hud.css).
  const bannerOn =
    health != null ||
    clientUnsupported ||
    (recovery != null && ["degraded", "reconnecting", "failed"].includes(recovery.phase));

  // Same shape, for the mic "hot" pill vs the toast host (`.session-root.mic-on`).
  const rootClassName =
    "session-root" +
    (bannerOn ? " banner-on" : "") +
    (micOn ? " mic-on" : "") +
    (hudOpen ? " hud-open" : "");

  return (
    <div className={rootClassName} ref={containerRef}>
      {/* Must stay mounted/unreordered: a shift in <video>'s reconciliation
          slot (e.g. from loaderDone/health changes) remounts it, clearing
          srcObject and breaking the stream. onClick lives on the wrapper, not
          <video>, for the same reason — not focusable on purpose, no
          persistent on-screen button by design. */}
      <div className="session-video-wrap" onClick={handleStageClick}>
        <video
          ref={videoRef}
          className="session-video"
          data-scaling-mode={scalingMode}
          muted
          playsInline
          autoPlay
        />
      </div>

      {/* The launch/status line stays bottom-anchored and click-through. */}
      <div className="session-bottom-stack">
        {!channelOpen && <div className="session-hint muted">{status}</div>}
      </div>

      {/* Actionable notices: full-width at the top, stacked so two live ones
          can never cover each other (handoff §E). `banner-on` above offsets a
          top-docked HUD. */}
      {bannerOn && (
        <SessionBannerHost>
          {/* AS10-06: stream health. Warning (degrading) is non-blocking info;
              critical (unsustainable) offers Stop / Retry — the user always
              chooses, the client never auto-acts. */}
          {health && (
            <SessionBanner
              variant={health.kind === "critical" ? "critical" : "warning"}
              title={health.title}
              message={health.message}
              actions={
                health.actionable && (
                  <>
                    <Button variant="ghost" onClick={() => navigate("/app")}>
                      Retry
                    </Button>
                    <Button
                      variant="primary"
                      disabled={stopping}
                      onClick={() => void handleStop()}
                    >
                      {stopping ? "Stopping…" : "Stop"}
                    </Button>
                  </>
                )
              }
            />
          )}

          {/* Multi-codec spec §6.1: client_unsupported. */}
          {clientUnsupported && (
            <SessionBanner
              title={<>This stream isn&rsquo;t supported on your device</>}
              message={
                <>
                  Your browser received video data but couldn&rsquo;t decode it — the
                  negotiated codec or profile isn&rsquo;t supported here. Stop and try a
                  different stream quality.
                </>
              }
              actions={
                <Button variant="primary" disabled={stopping} onClick={() => void handleStop()}>
                  {stopping ? "Stopping…" : "Stop"}
                </Button>
              }
            />
          )}

          {recovery && ["degraded", "reconnecting", "failed"].includes(recovery.phase) && (
            <SessionBanner
              variant={recovery.phase === "failed" ? "critical" : "warning"}
              title={
                recovery.phase === "failed" ? "Connection recovery stopped" : "Recovering connection"
              }
              message={recovery.message}
              actions={
                recovery.phase === "failed" ? (
                  <Button variant="primary" onClick={() => navigate("/app")}>
                    Back to library
                  </Button>
                ) : (
                  <Button variant="ghost" onClick={() => runtimeRef.current?.cancelRecovery()}>
                    Cancel
                  </Button>
                )
              }
            />
          )}
        </SessionBannerHost>
      )}

      {/* UI-08: connection loader overlay — full-bleed dark screen until the
          stream is BOTH connected and the app has presented (#484 §3.2).
          Unmounted from DOM after the fade-out completes to free resources. */}
      {!loaderDone && (
        <SessionLoader
          statusMsg={launchDetail ?? status}
          streaming={revealReady}
          appName={state?.appName}
          failure={launchFailure}
          // The four launch signals the phase machine walks. The input
          // DataChannel opening is channelOpen — there is no second notion of it.
          wsOpen={wsOpen}
          pcConnected={pcConnected}
          firstFrame={firstFrame}
          inputOpen={channelOpen}
          sessionState={sessionRunning ? "running" : hostAssigned ? "assigned" : "pending"}
          // The poll's "app booting" is the control plane's app_launch_state
          // "starting" (#484 §3.2): the app is up, it has not presented.
          appLaunchState={launchDetail === "app booting" ? "starting" : undefined}
          // #482: without these the loader falls back to "finding a host" for
          // anything the status string doesn't recognise.
          hostAssigned={hostAssigned}
          sessionRunning={sessionRunning}
          iceState={iceState}
          // handleStop tears the session down server-side so "Back to
          // library" from a dead launch never orphans a session.
          onExit={() => void handleStop()}
        />
      )}

      {/* The HUD is a sibling of <video>, subscribing to the telemetry fan-out
          directly (#139). SessionSwapController owns all quick-switch wiring as
          one unit so the bar's `swappingTo` and the Games pane can never see a
          different `transition` than the overlay renders. */}
      <SessionSwapController
        sessionId={sessionId}
        authToken={authToken}
        currentApp={currentApp}
        onCommitted={(appId, appName) => setCurrentApp({ id: appId, name: appName })}
        onToast={pushToast}
        onSwapStart={() => hudRef.current?.close()}
      >
        {({ quickSwitch, swappingTo }) => (
          <Hud
            ref={hudRef}
            register={registerTelemetry}
            channelOpen={channelOpen}
            appPresented={appPresented}
            appName={appTitle}
            tier={state?.tier}
            resolvedCodec={state?.resolvedCodec}
            sessionId={sessionId}
            inputCaptured={inputCaptured}
            onGrab={handleGrab}
            onRelease={handleRelease}
            pointerLockAvailable={pointerLockSupported()}
            touchLook={touchLookSupported()}
            // Without Pointer Lock, Esc is capture.ts's own release gesture and
            // is never forwarded — so it always releases, fullscreen or not.
            escReleases={
              !pointerLockSupported() || !fullscreen || !keyboardLockSupported() || kbLockRefused
            }
            escInsecureContext={pointerLockSupported() && !keyboardLockSupported()}
            // API presence alone cannot see these two, and both used to render
            // the cheerful fullscreen hint exactly where it was wrong: on a
            // bypassed-certificate origin the Keyboard Lock API exists
            // (isSecureContext is true there), and a refused lock() leaves the
            // API "supported" while Esc quits.
            escCertUntrusted={certTrust === "untrusted-cert"}
            escLockRefused={kbLockRefused}
            micGranted={state?.micGranted === true}
            micOn={micOn}
            micBusy={micBusy}
            onToggleMic={() => void handleToggleMic()}
            fullscreen={fullscreen}
            onFullscreen={handleFullscreen}
            stopping={stopping}
            onStop={() => void handleStop()}
            swappingTo={swappingTo}
            games={quickSwitch}
            scalingMode={scalingMode}
            onScalingChange={handleScalingChange}
            startedAt={startedAt}
            streamSize={streamSize}
            // Null = "match the stream" / still at launch size — the pane
            // resolves it against `streamSize`, so this page never duplicates
            // the stream size into state.
            externalSize={externalSize}
            onStreamSizeChange={handleStreamSizeChange}
            streamRungs={effectiveRungs}
            externalResizeSupported={externalResizeSupported}
            // ABR resolution ladder (T6b): server truth only, never
            // client-side-simulate. `session.stream.external_owner`.
            externalOwner={externalOwner}
            streamAdapting={streamChanging}
            renderSize={renderSize}
            onRenderSizeChange={handleRenderSizeChange}
            uiScale={uiScale}
            onUiScaleChange={handleUiScaleChange}
            displayBusy={displayBusy}
            badgeExternalSize={badgeExternalSize}
            onOpenChange={setHudOpen}
          />
        )}
      </SessionSwapController>

      {/* Touch route into the shelf (UX assessment §2.3): the Ctrl+Alt+Shift+Q
          chord bootstrap can't be pressed on a phone. Shown only on a
          coarse-pointer device (CSS `@media (hover: none) and (pointer:
          coarse)`, else `display:none`) except while captured without a
          pointer lock, where `.always` force-shows it — it must not depend on
          the media query alone to decide whether a captured user can escape.

          Gated on real pointer-lock state, not capture: a locked pointer can't
          reach this button (it hides on desktop during play), but where nothing
          is ever locked, taps still land here — the only route back for a
          controller-only tablet player. */}
      {!pointerLocked && !hudOpen && (
        <button
          type="button"
          className={`session-summon${inputCaptured ? " always" : ""}`}
          aria-label="Session menu"
          title="Session menu"
          onClick={openHud}
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true">
            <path d="M4 7h16M4 12h16M4 17h16" />
          </svg>
        </button>
      )}

      <SessionToastHost toast={toast} />

      {/* Rendered outside every auto-hiding surface on purpose: a live
          microphone must not be able to become invisible, including under the
          "never show the strip" preference. */}
      {micOn && (
        <div className="mic-hot" role="status" aria-label="Microphone is on" title="Microphone is on">
          <span className="dot" aria-hidden="true" />
          MIC
        </div>
      )}
    </div>
  );
}
