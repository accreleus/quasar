// The game page's state. Reuse, not a fork: profiles come from
// GET /v1/me/profiles?app_id, the selection is the band's own useLaunchDraft
// over optionsFor's model, and a launch goes through useLaunch.launchApp.
// What Play sends is launchPlan's: no profile wherever the server's own pick
// streams here (as Home's one-click Play), otherwise what the band sends. A Play
// from Change sends a profile only for a selection that differs from Play's.
// Only the per-game memory of the last launch comes from gameData.ts.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate, useNavigationType } from "react-router-dom";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App, ProfilePreferencesResponse, ProfilesResponse, Session } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { useToast } from "../../../components/Toast";
import { probeCodecs } from "../../../webrtc/capability";
import {
  blockingReasons,
  defaultDraft,
  optionsFor,
  resolveSelection,
  toWireCodec,
  type DraftCodec,
  type LaunchDraft,
} from "../launchOptions";
import { LIVE_STATES, blockedFamilyIds, coverClassAt, familyRootApp, sharedLibraryIds } from "../libraryGrid";
import { useLibraryCatalog } from "../libraryCatalog";
import { waitingPlayLabel } from "../capacityRetry";
import { recommendation, resolutionLabel } from "../home/launchOptionRules";
import { useLaunch } from "../home/useLaunch";
import { useLaunchDraft } from "../home/useLaunchDraft";
import { useLibraryExtras } from "../library/libraryExtras";
import { clearGameSettings, getGameSettings, putGameSettings, type GameSettings } from "./gameData";
import { launchPlan } from "./launchPlan";

function isTypingTarget(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el) return false;
  return el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName);
}

/** Wire codec (game-settings) → the panel's catalog vocabulary. */
const fromWire = (c: GameSettings["codec"]): DraftCodec => (c === "h265" ? "hevc" : c ?? "auto");

const sameDraft = (a: LaunchDraft, b: LaunchDraft) => a.codec === b.codec && a.fps === b.fps && a.height === b.height;

export function useGamePage(app: App) {
  const { token, isAdmin } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from;
  const { addToast } = useToast();
  const { apps, setApps } = useLibraryCatalog();
  const extrasMap = useLibraryExtras();
  const extras = extrasMap?.[app.id] ?? null;
  const codecCaps = useMemo(() => probeCodecs(), []);

  // ── Live session (resume beats launch; siblings are blocked) ────────────
  const [mySessions, setMySessions] = useState<Session[]>([]);
  useEffect(() => {
    if (!token) return;
    libraryApi
      .getMySessions(token)
      .then((r) => setMySessions(r.items ?? []))
      .catch(() => {});
  }, [token]);
  const liveSession = mySessions.find((s) => LIVE_STATES.includes(s.state)) ?? null;
  const liveApp = liveSession ? apps.find((a) => a.id === liveSession.app_id) ?? null : null;
  const isLive = liveSession?.app_id === app.id;
  const sharedHome = liveSession?.shared_home_app_id ?? null;
  const isBlocked = blockedFamilyIds(apps, liveSession?.app_id ?? null, sharedHome).has(app.id);
  const sharedLibrary = sharedLibraryIds(apps, sharedHome).has(app.id);
  const blockedByName = liveApp ? familyRootApp(apps, liveApp)?.name ?? liveApp.name : null;

  // ── Profiles + the remembered per-game selection ─────────────────────────
  const [profiles, setProfiles] = useState<ProfilesResponse | string | null>(null);
  const fetchProfiles = useCallback(
    async (a: Pick<App, "id">): Promise<ProfilesResponse | null> => {
      if (!token) return null;
      setProfiles(null);
      try {
        const data = await libraryApi.getProfiles(token, a.id);
        setProfiles(data);
        return data;
      } catch (err) {
        setProfiles(err instanceof ApiError ? err.message : "could not load stream profiles");
        return null;
      }
    },
    [token],
  );
  // Keyed by id: a favourite toggle replaces the app object, not the game.
  const appId = app.id;
  useEffect(() => {
    void fetchProfiles({ id: appId });
  }, [fetchProfiles, appId]);

  /** undefined = still loading; null = the read failed. */
  const [prefs, setPrefs] = useState<ProfilePreferencesResponse | null | undefined>(undefined);
  const prefsSeq = useRef(0);
  /** A read is in flight: the sheet's Play waits for it. */
  const [prefsBusy, setPrefsBusy] = useState(false);
  const loadPrefs = useCallback(() => {
    if (!token) return;
    const seq = ++prefsSeq.current;
    setPrefsBusy(true);
    libraryApi
      .getProfilePreferences(token)
      .then((p) => seq === prefsSeq.current && setPrefs(p))
      // A failed re-read keeps the last good read; only a failed first read is unknown.
      .catch(() => seq === prefsSeq.current && setPrefs((old) => old ?? null))
      .finally(() => seq === prefsSeq.current && setPrefsBusy(false));
  }, [token]);
  useEffect(() => {
    loadPrefs();
    return () => {
      prefsSeq.current++;
    };
  }, [loadPrefs]);
  const retryPrefs = useCallback(() => {
    setPrefs(undefined);
    loadPrefs();
  }, [loadPrefs]);

  /** undefined = still loading; null = nothing remembered. */
  const [saved, setSaved] = useState<GameSettings | null | undefined>(undefined);
  useEffect(() => {
    if (!token) return;
    let live = true;
    void getGameSettings(token, app.id).then((s) => live && setSaved(s));
    return () => {
      live = false;
    };
  }, [token, app.id]);

  const loaded = typeof profiles === "object" && profiles !== null ? profiles : null;
  const profilesError = typeof profiles === "string" ? profiles : null;
  const [remembered, setRemembered] = useState(false);
  const planned = useMemo(() => {
    if (!loaded || prefs === undefined) return null;
    const m = optionsFor({ app, data: loaded, caps: codecCaps, isAdmin });
    const plan = launchPlan({ app, model: m, recommendedId: loaded.recommended_id, prefs, isAdmin, h264: codecCaps.h264, remembered });
    return { plan, model: plan.seedId ? { ...m, seed: defaultDraft(m.space, plan.seedId) } : m };
  }, [loaded, prefs, app, codecCaps, isAdmin, remembered]);
  const model = planned?.model ?? null;
  const locked = planned?.plan.locked ?? null;
  const oneClick = planned?.plan.oneClick ?? false;

  const [optionsOpen, setOptionsOpen] = useState(false);
  /** The user chose something in the open sheet, even the row it opened on. */
  const [picked, setPicked] = useState(false);
  const selection = useLaunchDraft(model, app, optionsOpen);
  const { commit } = selection;

  // A new seed (the preferences are re-read when Change opens) moves the selection,
  // unless this game's own settings hold it. Nothing else is ever committed. A pick
  // the user made in the open sheet is theirs: the commit (which the draft follows) waits.
  const committed = selection.committed;
  const edited = optionsOpen && picked;
  useEffect(() => {
    if (model && !remembered && !edited && committed && !sameDraft(committed, model.seed)) commit(model.seed);
  }, [model, remembered, edited]); // not on `committed`: only a seed change moves it

  // Seed from the last launch of this game, once, after the band's own seed.
  const appliedRef = useRef(false);
  useEffect(() => {
    if (!model || saved === undefined || appliedRef.current) return;
    appliedRef.current = true;
    if (!saved || model.pinnedProfileId || locked) return;
    const codec = fromWire(saved.codec);
    const e = (model.space.entriesByCodec.get(codec) ?? []).find(
      (x) => x.profileId === saved.profile_id && x.eligibility !== "ineligible",
    );
    if (!e) return;
    commit({ codec, fps: e.fps, height: e.height });
    setRemembered(true);
  }, [model, saved, commit, locked]);

  const ready = Boolean(model || profilesError);
  const resolved = selection.resolved;
  const isDeadEnd = Boolean(loaded) && !profilesError && (!resolved || resolved.eligibility === "ineligible");
  const recommended = (model?.space.entriesByCodec.get("auto") ?? []).find((e) => e.recommended) ?? null;
  const rec = recommendation({
    state: profilesError ? "failed" : ready ? "loaded" : "loading",
    deadEnd: isDeadEnd,
    selection: resolved,
    codec: selection.committed?.codec,
    recommended,
  });
  const isRecommendation = selection.committed?.codec === "auto" && Boolean(resolved?.recommended);
  /** This game's own remembered settings are what Play will use. */
  const perGame = remembered && !isRecommendation && rec.tone === "ok";
  const recLabel = recommended ? `${resolutionLabel(recommended.height)} at ${recommended.fps} fps` : null;
  // With no profile sent the stream line says the server picks, so this line names no selection.
  const oneClickText =
    locked === "forced"
      ? "This game's settings are fixed by an admin."
      : locked === "overrides"
        ? "An admin has disabled user profile overrides."
        : recLabel && `The recommendation here is ${recLabel}.`;
  const recText =
    remembered && !isRecommendation && rec.tone === "ok"
      ? `Your last settings for this game.${recLabel ? ` Recommended here: ${recLabel}.` : ""}`
      : oneClick && rec.tone === "ok" && oneClickText
        ? oneClickText
        : rec.text;

  // ── Launch ────────────────────────────────────────────────────────────────
  const launch = useLaunch({
    token,
    apps,
    liveSession,
    canDecodeH264: codecCaps.h264,
    fetchProfiles,
    revealDetail: () => setOptionsOpen(true),
  });

  const play = useCallback(
    (next?: LaunchDraft | null) => {
      if (!model || !selection.committed) return;
      // The seed, unless this game's own settings hold the selection (the commit can lag the seed while a pick is open).
      const current = remembered ? selection.committed : model.seed;
      // A Play from Change that changed nothing is Play. A change is never
      // committed: a launch that works leaves the page, one that fails leaves Play as it was.
      const change = !locked && next && !sameDraft(next, current) ? next : null;
      const chosen = change ?? current;
      const r = resolveSelection(model.space, chosen);
      if (!r) return;
      const wireCodec = chosen.codec === "auto" || r.codec === null ? undefined : toWireCodec(r.codec);
      const profileId = model.pinnedProfileId ?? r.profileId;
      // Remember what was launched, unless it is just the default selection.
      const seed = model.seed;
      const isSeed = chosen.codec === seed.codec && chosen.fps === seed.fps && chosen.height === seed.height;
      if (token) {
        const write = isSeed
          ? saved
            ? clearGameSettings(token, app.id)
            : null
          : putGameSettings(token, app.id, { profile_id: profileId, codec: wireCodec, source: "remembered" });
        void write?.catch(() => {});
      }
      if (oneClick && !change) void launch.launchApp(app);
      else void launch.launchApp(app, profileId, wireCodec);
    },
    [model, selection.committed, remembered, locked, token, saved, app, launch, oneClick],
  );

  /** Back to the default selection, and forget this game's own settings. */
  const resetToRecommended = useCallback(() => {
    if (!model) return;
    commit(model.seed);
    setRemembered(false);
    if (token && saved) {
      setSaved(null);
      void clearGameSettings(token, app.id).catch(() => {});
    }
  }, [model, commit, token, saved, app.id]);

  const resume = useCallback(() => {
    if (liveSession) navigate(`/app/session/${liveSession.id}`);
  }, [liveSession, navigate]);

  // Auto (codec null) refused for its codec on a browser without H.264.
  const noH264 =
    !codecCaps.h264 &&
    resolved?.codec === null &&
    resolved.eligibility === "ineligible" &&
    resolved.reasons.some((r) => r.code === "codec_not_supported");

  const playDisabled = isLive ? false : launch.launching || isBlocked || !resolved || isDeadEnd;
  const playLabel = isLive
    ? "Resume session"
    : launch.waitingForSlot
      ? waitingPlayLabel(launch.waitingReason)
      : launch.launching
        ? "Launching…"
        : "Play";
  const onPlay = isLive ? resume : () => play();

  // A pick in the top-bar search or the palette (this game again included) leaves
  // focus in the field (the palette hands it back there), where P would type: move
  // it to Play, or to the title until Play is live, and then on only if it is still there.
  const playRef = useRef<HTMLButtonElement>(null);
  const titleRef = useRef<HTMLHeadingElement>(null);
  const handOffRef = useRef<"arrived" | "waiting" | null>(null);
  // Only a new arrival: Back or Forward onto that entry leaves focus where it is.
  const navType = useNavigationType();
  useEffect(() => {
    if ((from === "search" || from === "palette") && navType !== "POP") handOffRef.current = "arrived";
  }, [from, navType, location.key]);
  useEffect(() => {
    const h = handOffRef.current;
    if (!h) return;
    if (optionsOpen) handOffRef.current = null;
    else if (playDisabled) {
      if (h === "arrived") titleRef.current?.focus();
      handOffRef.current = "waiting";
    } else {
      handOffRef.current = null;
      if (h === "arrived" || document.activeElement === titleRef.current) playRef.current?.focus();
    }
  }, [playDisabled, optionsOpen, location.key]);

  // ── Options sheet ─────────────────────────────────────────────────────────
  const changeRef = useRef<HTMLButtonElement>(null);
  const closeOptions = useCallback(() => {
    setOptionsOpen(false);
    requestAnimationFrame(() => changeRef.current?.focus());
  }, []);
  // A re-read that locks the page takes the sheet away (its pick would not be sent) and shows the lock line.
  useEffect(() => {
    if (!locked || !optionsOpen) return;
    setOptionsOpen(false);
    requestAnimationFrame(() => playRef.current?.focus());
  }, [locked, optionsOpen]);

  const pick: typeof selection.edit = (patch, column) => {
    setPicked(true);
    selection.edit(patch, column);
  };

  // ── Favourite (optimistic, as the home page does it) ────────────────────
  const toggleFavourite = useCallback(async () => {
    if (!token) return;
    const next = !app.favourite;
    setApps((prev) => prev.map((a) => (a.id === app.id ? { ...a, favourite: next } : a)));
    try {
      if (next) await libraryApi.favouriteApp(token, app.id);
      else await libraryApi.unfavouriteApp(token, app.id);
    } catch (err) {
      setApps((prev) => prev.map((a) => (a.id === app.id ? { ...a, favourite: !next } : a)));
      addToast({
        variant: "danger",
        title: "Couldn't update favourite",
        body: err instanceof ApiError ? err.message : "Try again.",
      });
    }
  }, [token, app.id, app.favourite, setApps, addToast]);

  // ── Keys: P plays, Escape closes the sheet ──────────────────────────────
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || e.repeat) return;
      if (e.key === "Escape" && optionsOpen) {
        e.preventDefault();
        closeOptions();
      } else if ((e.key === "p" || e.key === "P") && !optionsOpen && !isTypingTarget(e.target) && !playDisabled) {
        e.preventDefault();
        onPlay();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  // A page, not a band: arrive at its top, whatever the library was scrolled to.
  useEffect(() => {
    window.scrollTo(0, 0);
  }, [app.id]);

  const back = useCallback(() => {
    if (from) navigate(-1);
    else navigate("/app/library");
  }, [from, navigate]);

  // Cover gradient keyed to name order, so a game's fallback matches its tile.
  const coverClassById = useMemo(() => {
    const m = new Map<string, string>();
    [...apps].sort((a, b) => a.name.localeCompare(b.name)).forEach((a, i) => m.set(a.id, coverClassAt(i)));
    return m;
  }, [apps]);

  const notes = {
    appName: app.name,
    risky: resolved?.eligibility === "risky",
    riskyReasons: resolved?.reasons ?? [],
    deadEnd: isDeadEnd && !noH264,
    deadEndReasons: resolved?.reasons?.length ? resolved.reasons : loaded ? blockingReasons(loaded) : [],
    isLive,
    isBlocked,
    blockedByName,
    sharedLibrary,
    liveSessionId: liveSession?.id ?? null,
    noH264,
    waitingForSlot: launch.waitingForSlot,
    waitingReason: launch.waitingReason,
    onRetryProfiles: () => void fetchProfiles(app),
  };

  return {
    app,
    apps,
    extras,
    extrasMap,
    coverClassById,
    // settings
    ready,
    model,
    profilesError,
    selection,
    oneClick,
    locked,
    rec: { tone: rec.tone, text: recText },
    perGame,
    recommended,
    resetToRecommended,
    notes,
    optionsOpen,
    /** The sheet's Play waits for the re-read Change started. */
    checking: prefsBusy,
    openOptions: () => {
      setOptionsOpen(true);
      setPicked(false);
      loadPrefs();
    },
    pick,
    retryPrefs,
    closeOptions,
    changeRef,
    // launch
    play,
    onPlay,
    playRef,
    titleRef,
    playDisabled,
    playLabel,
    launching: launch.launching,
    waitingForSlot: launch.waitingForSlot,
    waitingReason: launch.waitingReason,
    // misc
    toggleFavourite: () => void toggleFavourite(),
    back,
  };
}

export type GamePage = ReturnType<typeof useGamePage>;
