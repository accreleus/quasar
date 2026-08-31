// Launching a session from the home page: the POST, #494's capacity retry, and
// the one-click eligibility gate every entry point funnels through.
//
// Lifted out of AppHomeNext.tsx verbatim when the v3 home split the page into
// components — the page composes, this owns the launch.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App, ProfilesResponse, Session } from "../../../api/types";
import { useToast } from "../../../components/Toast";
import { playoutOverride } from "../../../webrtc/playout";
import { decideCapacityRetry } from "../capacityRetry";
import { isRecommendationEligible } from "../launchOptions";
import { presentLaunchError } from "../libraryGrid";

/** #494's retry-wait timeout, abortable and observable. `timeoutRef` is cleared
 *  on settle so an unmount cleanup can `clearTimeout` it directly; the abort
 *  listener also rejects so the awaiting caller unwinds instead of staying
 *  suspended forever. */
function abortableSleep(
  ms: number,
  signal: AbortSignal,
  timeoutRef: { current: ReturnType<typeof setTimeout> | null },
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException("Aborted", "AbortError"));
      return;
    }
    const id = setTimeout(() => {
      timeoutRef.current = null;
      resolve();
    }, ms);
    timeoutRef.current = id;
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(id);
        timeoutRef.current = null;
        reject(new DOMException("Aborted", "AbortError"));
      },
      { once: true },
    );
  });
}

export interface UseLaunchOptions {
  token: string | null;
  apps: readonly App[];
  /** The caller's live session, if any — resume beats launch (see quickLaunch). */
  liveSession: Session | null;
  /** Whether this browser can decode h264 at all; without it, one-click has
   *  nothing to offer and the band explains why. */
  canDecodeH264: boolean;
  /** Evaluates launch profiles for `app`; the detail band owns the state, so
   *  the result lands there as well as here. */
  fetchProfiles: (app: App) => Promise<ProfilesResponse | null>;
  /** Opens `app`'s detail band — where a gated launch has to end up. */
  revealDetail: (app: App, expandOptions: boolean) => void;
}

export interface UseLaunch {
  launching: boolean;
  /** #494: retrying a `capacity_exhausted` bounce rather than failing. */
  waitingForSlot: boolean;
  /** The app whose profiles the one-click path is evaluating right now. */
  resolvingId: string | null;
  launchApp: (
    app: App,
    profileId?: string,
    streamCodec?: "h264" | "h265" | "av1",
  ) => Promise<void>;
  quickLaunch: (app: App) => Promise<void>;
}

export function useLaunch({
  token,
  apps,
  liveSession,
  canDecodeH264,
  fetchProfiles,
  revealDetail,
}: UseLaunchOptions): UseLaunch {
  const navigate = useNavigate();
  const { addToast, removeToast } = useToast();
  const [launching, setLaunching] = useState(false);
  const [waitingForSlot, setWaitingForSlot] = useState(false);
  const [resolvingId, setResolvingId] = useState<string | null>(null);

  // #494's retry loop schedules a setTimeout outliving a single render; this
  // stops it touching state after navigation. Reset to true on every effect run
  // (not just initial) because React 18 StrictMode's phantom
  // mount→cleanup→remount would otherwise leave it false for the component's
  // whole real lifetime, hanging the retry loop on "Waiting for a slot…"
  // forever.
  const mountedRef = useRef(true);
  // The in-flight retry timeout and launch AbortController — at most one of
  // each, owned by whichever launchApp call is active (`launching` gates
  // concurrency). Unmount cleanup clears/aborts both: an un-aborted
  // POST /v1/sessions can still land server-side with nobody left waiting.
  const retryTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const launchAbortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (retryTimeoutRef.current !== null) {
        clearTimeout(retryTimeoutRef.current);
        retryTimeoutRef.current = null;
      }
      launchAbortRef.current?.abort();
    };
  }, []);

  const launchApp = useCallback(
    async (app: App, profileId?: string, streamCodec?: "h264" | "h265" | "av1") => {
      if (!token || launching) return;
      setLaunching(true);
      setWaitingForSlot(false); // clear any stale copy from a previous attempt
      const req: libraryApi.LaunchRequest = { app_id: app.id, mic: true };
      if (profileId) req.profile_id = profileId;
      if (streamCodec) req.stream = { codec: streamCodec };

      // #494: abort an in-flight POST /v1/sessions (or a pending retry wait) if
      // the page navigates away mid-launch. One controller per launchApp call;
      // unmount aborts whichever is current.
      const controller = new AbortController();
      launchAbortRef.current = controller;

      // capacity_exhausted is "full right now" (503) — the other user's slot
      // frees on its own (observed ~15s), so the first bounce retries quietly
      // instead of erroring. `firstBounceAt` anchors the retry budget to the
      // whole wait, not each attempt. `waitingToastId` covers quickLaunch paths
      // (tile, rail) with no detail panel open.
      let firstBounceAt: number | null = null;
      let waitingToastId: string | null = null;
      const dismissWaitingToast = () => {
        if (waitingToastId !== null) {
          removeToast(waitingToastId);
          waitingToastId = null;
        }
      };
      for (;;) {
        try {
          const { session, signaling } = await libraryApi.launchSession(
            token,
            req,
            controller.signal,
          );
          dismissWaitingToast();
          launchAbortRef.current = null;
          navigate(`/app/session/${session.id}`, {
            state: {
              signalingUrl: signaling.url,
              signalingToken: signaling.token,
              // #509: the deployment's STUN/TURN servers travel with the coords
              // they were minted alongside. Absent means none configured.
              iceServers: signaling.ice_servers ?? [],
              appName: app.name,
              appId: app.id,
              playout0Ms: session.stream.playout0_ms,
              tier: `${session.stream.width}×${session.stream.height}@${session.stream.fps}`,
              resolvedCodec: session.stream.codec,
              micGranted: session.stream.mic === true,
              playoutOverride: playoutOverride(),
            },
          });
          return;
        } catch (err) {
          if (err instanceof ApiError && err.code === "capacity_exhausted") {
            const now = Date.now();
            if (firstBounceAt === null) firstBounceAt = now;
            const decision = decideCapacityRetry({
              elapsedMs: now - firstBounceAt,
              retryAfterSeconds: err.retryAfterSeconds,
            });
            if (decision.kind === "retry") {
              if (!mountedRef.current) return;
              setWaitingForSlot(true);
              if (waitingToastId === null) {
                waitingToastId = addToast({
                  variant: "info",
                  title: "Waiting for a slot to free up…",
                  body: `${app.name} will launch as soon as one is free.`,
                });
              }
              try {
                await abortableSleep(decision.delayMs, controller.signal, retryTimeoutRef);
              } catch {
                // Aborted — the page was navigated away from mid-wait.
                // mountedRef is already false by the time this settles (the
                // abort only ever comes from the unmount cleanup), so the check
                // right below returns before touching state.
              }
              if (!mountedRef.current) return;
              continue; // one more attempt, still inside the loop
            }
            // Gave up after ~60s of retrying — fall through to the ordinary
            // error presentation below, exactly as any other launch failure.
          }
          dismissWaitingToast();
          launchAbortRef.current = null;
          if (!mountedRef.current) return;
          if (err instanceof ApiError) {
            const presented = presentLaunchError(apps, app, err.code, err.message, err.sessionId);
            addToast({
              variant: presented.variant,
              title: presented.title,
              body: presented.body,
              duration: presented.sessionId ? 10000 : undefined,
              action: presented.sessionId
                ? {
                    label: "Go to session",
                    onClick: () => navigate(`/app/session/${presented.sessionId}`),
                  }
                : undefined,
            });
          } else {
            addToast({ variant: "danger", title: "Launch failed", body: "Launch failed." });
          }
          setLaunching(false);
          setWaitingForSlot(false);
          return;
        }
      }
    },
    [token, launching, navigate, addToast, removeToast, apps],
  );

  // ── One-click launch, gated on the eligibility warning (#385 item 2) ──────
  // Every one-click entry point (tile, rail, `P` shortcut) funnels through
  // here — a shortcut that skips the warning is a hole, not a convenience.
  const quickLaunch = useCallback(
    async (app: App) => {
      // Resume before launch: the live app's own tile would 409 `home_in_use`
      // on launch, so play on it returns to that session instead (like the rail
      // card already does). Lives at this funnel because every one-click route
      // passes through it. Only the live app itself — sibling tiles sharing its
      // home are `blockedFamilyIds` and must never resume into a different
      // app's session.
      if (liveSession && liveSession.app_id === app.id) {
        navigate(`/app/session/${liveSession.id}`);
        return;
      }
      if (!token || launching || resolvingId) return;
      if (!canDecodeH264) {
        revealDetail(app, false);
        return;
      }
      setResolvingId(app.id);
      const data = await fetchProfiles(app);
      setResolvingId(null);
      if (!data || !isRecommendationEligible(data)) {
        revealDetail(app, false);
        return;
      }
      await launchApp(app);
    },
    [
      token,
      launching,
      resolvingId,
      canDecodeH264,
      revealDetail,
      fetchProfiles,
      launchApp,
      liveSession,
      navigate,
    ],
  );

  // Memoised: the page hands this object to `useCallback` dependency lists, and
  // a fresh object every render would make every one of them inert.
  return useMemo(
    () => ({ launching, waitingForSlot, resolvingId, launchApp, quickLaunch }),
    [launching, waitingForSlot, resolvingId, launchApp, quickLaunch],
  );
}
