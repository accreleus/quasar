// The game page's state. Reuse, not a fork: profiles come from
// GET /v1/me/profiles?app_id, the selection is the band's own useLaunchDraft
// over optionsFor's model, and a launch goes through useLaunch.launchApp.
// Only the per-game memory of the last launch and the play history come from
// gameData.ts.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App, ProfilesResponse, Session } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { useToast } from "../../../components/Toast";
import { probeCodecs } from "../../../webrtc/capability";
import {
  blockingReasons,
  optionsFor,
  resolveSelection,
  toWireCodec,
  type DraftCodec,
  type LaunchDraft,
} from "../launchOptions";
import { LIVE_STATES, blockedFamilyIds, coverClassAt, familyRootApp } from "../libraryGrid";
import { useLibraryCatalog } from "../libraryCatalog";
import { formatBitrate, recommendation, resolutionLabel } from "../home/launchOptionRules";
import { useLaunch } from "../home/useLaunch";
import { useLaunchDraft } from "../home/useLaunchDraft";
import {
  clearGameSettings,
  getGameSettings,
  invalidateGameExtras,
  putGameSettings,
  useGameExtras,
  type GameSettings,
} from "./gameData";

function isTypingTarget(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el) return false;
  return el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName);
}

/** Wire codec (game-settings) → the panel's catalog vocabulary. */
const fromWire = (c: GameSettings["codec"]): DraftCodec => (c === "h265" ? "hevc" : c ?? "auto");

export function useGamePage(app: App) {
  const { token, isAdmin } = useAuth();
  const navigate = useNavigate();
  const location = useLocation() as { state: { from?: string } | null };
  const { addToast } = useToast();
  const { apps, setApps } = useLibraryCatalog();
  const extrasMap = useGameExtras();
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
  const isBlocked = blockedFamilyIds(apps, liveSession?.app_id ?? null).has(app.id);
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
  const model = useMemo(
    () => (loaded ? optionsFor({ app, data: loaded, caps: codecCaps, isAdmin }) : null),
    [loaded, app, codecCaps, isAdmin],
  );

  const [optionsOpen, setOptionsOpen] = useState(false);
  const selection = useLaunchDraft(model, app, optionsOpen);
  const { commit } = selection;

  // Seed from the last launch of this game, once, after the band's own seed.
  const [remembered, setRemembered] = useState(false);
  const appliedRef = useRef(false);
  useEffect(() => {
    if (!model || saved === undefined || appliedRef.current) return;
    appliedRef.current = true;
    if (!saved || model.pinnedProfileId) return;
    const codec = fromWire(saved.codec);
    const e = (model.space.entriesByCodec.get(codec) ?? []).find(
      (x) => x.profileId === saved.profile_id && x.eligibility !== "ineligible",
    );
    if (!e) return;
    commit({ codec, fps: e.fps, height: e.height });
    setRemembered(true);
  }, [model, saved, commit]);

  const ready = Boolean(model || profilesError) && saved !== undefined;
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
  const recText =
    remembered && !isRecommendation && rec.tone === "ok"
      ? `Your last settings for this game.${recommended ? ` Recommended here: ${resolutionLabel(recommended.height)} at ${recommended.fps} fps.` : ""}`
      : rec.text;

  /** "1440p · 120 fps · 25 Mbps · Auto · AV1", or null until it is known. */
  const sentence =
    ready && resolved
      ? [
          resolutionLabel(resolved.height),
          `${resolved.fps} fps`,
          formatBitrate(resolved.bitrateKbps),
          selection.spec.codec,
        ].join(" · ")
      : null;

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
      const chosen = next ?? selection.committed;
      if (!model || !chosen) return;
      const r = resolveSelection(model.space, chosen);
      if (!r) return;
      commit(chosen);
      const wireCodec = chosen.codec === "auto" || r.codec === null ? undefined : toWireCodec(r.codec);
      const profileId = model.pinnedProfileId ?? r.profileId;
      // Remember what was launched, unless it is just the device's recommendation.
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
      invalidateGameExtras();
      void launch.launchApp(app, profileId, wireCodec);
    },
    [model, selection.committed, commit, token, saved, app, launch],
  );

  /** Back to the device recommendation, and forget this game's own settings. */
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
      ? launch.waitingReason === "host"
        ? "Waiting for a host…"
        : "Waiting for a slot…"
      : launch.launching
        ? "Launching…"
        : "Play";
  const onPlay = isLive ? resume : () => play();

  // ── Options sheet ─────────────────────────────────────────────────────────
  const changeRef = useRef<HTMLButtonElement>(null);
  const closeOptions = useCallback(() => {
    setOptionsOpen(false);
    requestAnimationFrame(() => changeRef.current?.focus());
  }, []);

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
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return;
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
    if (location.state?.from) navigate(-1);
    else navigate("/app/library");
  }, [location.state, navigate]);

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
    sentence,
    rec: { tone: rec.tone, text: recText },
    perGame,
    recommended,
    resetToRecommended,
    notes,
    optionsOpen,
    openOptions: () => setOptionsOpen(true),
    closeOptions,
    changeRef,
    // launch
    play,
    onPlay,
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
