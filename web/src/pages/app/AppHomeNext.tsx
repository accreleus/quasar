// The logged-in landing page — built against
// design_handoff_v3/screens/home.html (spec §B).
//
// Sole `/app` landing page; the classic library page was retired (see
// libraryDetail.tsx header for what survived).
//
// THE VIEW IS THE ROUTE, NOT STATE: reads `location.pathname` ("/app" vs
// "/app/library") so the view is bookmarkable and back/forward works — no
// in-page view toggle.
//
// THE VIEW SLIDES, IT DOES NOT CUT. `.home-view` is keyed by view so a nav
// click mounts a fresh wrapper and home.css's enter animation runs; direction
// follows nav order (Home→Library = right, reverse = left).
// `prefers-reduced-motion: reduce` drops to an instant swap. The hero is
// Unmounted (not collapsed) across the switch — see Hero.tsx.
//
// Composition, not implementation. The parts live in ./home: Hero,
// FeaturedRail, LibraryGrid, LibraryTile, the three panels, and the two hooks
// that own launching (useLaunch) and the detail band (useDetailBand). What is
// left here is the page's own state, its keyboard model, and the wiring.
//
// DATA HONESTY: the featured rail is ranked and labelled SERVER-SIDE by
// GET /v1/me/highlights; this page holds no ranker of its own. See homeData.ts.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { flushSync } from "react-dom";
import { useLocation, useNavigate } from "react-router-dom";
import * as libraryApi from "../../api/library";
import { ApiError } from "../../api/client";
import type { App, Highlight, Session } from "../../api/types";
import { useAuth } from "../../auth/context";
import { Button } from "../../components/Button";
import { useToast } from "../../components/Toast";
import { probeCodecs } from "../../webrtc/capability";
import { buildRailCards } from "./homeData";
import {
  LIVE_STATES,
  blockedFamilyIds,
  coverClassAt,
  familyRootApp,
  filterLibraryApps,
  sortLibraryApps,
  type KindFilter,
} from "./libraryGrid";
import { useLibraryCatalog } from "./libraryCatalog";
import { useLibrarySearch } from "./librarySearchContext";
import type { SessionSummary } from "./sessionSummary";
import { DetailBand } from "./home/DetailBand";
import { Hero } from "./home/Hero";
import { FeaturedRail } from "./home/FeaturedRail";
import { LibraryGrid } from "./home/LibraryGrid";
import { LibraryTile } from "./home/LibraryTile";
import { groupBySource } from "./home/librarySources";
import { LibraryStates, SessionSummaryCard, StopSessionModal } from "./home/panels";
import { useDetailBand } from "./home/useDetailBand";
import { useHomeKeys } from "./home/useHomeKeys";
import { useLaunch } from "./home/useLaunch";
import "../../styles/home.css";

type HomeView = "home" | "library";

function prefersReducedMotion(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

export function AppHomeNext() {
  const { token, isAdmin } = useAuth();
  const navigate = useNavigate();
  const location = useLocation() as {
    pathname: string;
    state: { sessionSummary?: SessionSummary } | null;
  };
  const summary = location.state?.sessionSummary;
  // Home vs Library is derived from the URL — see the header. `startsWith` so a
  // trailing slash ("/app/library/") resolves the same way.
  const view: HomeView = location.pathname.startsWith("/app/library") ? "library" : "home";
  const { addToast } = useToast();
  const { query: search } = useLibrarySearch();

  // Catalogue, loading and error come from the layout's one read (see
  // ./libraryCatalog) — the command palette in the shell above renders the same
  // list, and two components fetching /v1/apps is two answers to one question.
  const { apps, loading, error, reload: loadApps, setApps } = useLibraryCatalog();
  const [mySessions, setMySessions] = useState<Session[]>([]);
  const [highlights, setHighlights] = useState<Highlight[]>([]);
  const [stopTarget, setStopTarget] = useState<Session | null>(null);
  const [stopping, setStopping] = useState(false);
  const [kindFilter, setKindFilter] = useState<KindFilter>("all");

  // ── Home ↔ Library enter direction ────────────────────────────────────────
  // Derived during render, not in an effect: an effect would paint the new view
  // at rest before snapping it to the animation start (a visible flash). React
  // discards the first output, so the keyed wrapper below mounts already
  // carrying its direction. "none" on first load.
  const [prevView, setPrevView] = useState<HomeView>(view);
  const [enterDir, setEnterDir] = useState<"none" | "right" | "left">("none");
  if (prevView !== view) {
    setPrevView(view);
    setEnterDir(view === "library" ? "right" : "left");
  }

  const gridsRef = useRef<HTMLDivElement>(null);
  // The rail's scroll container lives in FeaturedRail but the roving-focus
  // model is the PAGE's — one model spanning both regions is the point — so the
  // page owns the ref and lends it to the rail.
  const railTrackRef = useRef<HTMLDivElement>(null);
  const codecCaps = useMemo(() => probeCodecs(), []);

  // ── The caller's own sessions ────────────────────────────────────────────
  useEffect(() => {
    if (!token) return;
    libraryApi
      .getMySessions(token)
      .then((res) => setMySessions(res.items ?? []))
      .catch(() => {
        /* endpoint unavailable or no sessions — only the live clock's fallback
           source is lost, and the rail itself comes from /v1/me/highlights */
      });
  }, [token]);

  // Independent fetch/state so a rail failure (500, network) never takes the
  // library grid down with it.
  useEffect(() => {
    if (!token) return;
    libraryApi
      .getHighlights(token)
      .then((res) => setHighlights(res.items ?? []))
      .catch(() => setHighlights([]));
  }, [token]);

  const liveSession = useMemo(
    () => mySessions.find((s) => LIVE_STATES.includes(s.state)) ?? null,
    [mySessions],
  );
  const liveApp = useMemo(
    () => (liveSession ? apps.find((a) => a.id === liveSession.app_id) ?? null : null),
    [liveSession, apps],
  );

  // Single-writer lock UX — presentation only; the server's 409 home_in_use is
  // the actual gate (CLAUDE.md invariant #6).
  const blockedIds = useMemo(
    () => blockedFamilyIds(apps, liveSession?.app_id ?? null),
    [apps, liveSession],
  );
  const blockedByName = useMemo(
    () => (liveApp ? familyRootApp(apps, liveApp)?.name ?? liveApp.name : null),
    [apps, liveApp],
  );

  // Cover colour keyed to the NAME order so a favourite toggle cannot recolour
  // the whole grid.
  const appsByName = useMemo(() => [...apps].sort((a, b) => a.name.localeCompare(b.name)), [apps]);
  const coverClassById = useMemo(() => {
    const m = new Map<string, string>();
    appsByName.forEach((a, i) => m.set(a.id, coverClassAt(i)));
    return m;
  }, [appsByName]);

  const sortedApps = useMemo(() => sortLibraryApps(appsByName), [appsByName]);
  const filteredApps = useMemo(
    () => filterLibraryApps(sortedApps, kindFilter, search),
    [sortedApps, kindFilter, search],
  );

  /** The grids currently on screen, in DOM order. Home view is one flat grid;
   *  Library view is one grid per source group. */
  const sourceGroups = useMemo(() => groupBySource(filteredApps), [filteredApps]);
  const visibleLists = useMemo(
    () => (view === "library" ? sourceGroups.map((g) => g.apps) : [filteredApps]),
    [view, sourceGroups, filteredApps],
  );

  const detail = useDetailBand({
    token,
    visibleLists,
    onClearFilter: () => setKindFilter("all"),
    smoothScroll: !prefersReducedMotion(),
  });
  const launch = useLaunch({
    token,
    apps,
    liveSession,
    canDecodeH264: codecCaps.h264,
    fetchProfiles: detail.fetchProfiles,
    revealDetail: detail.reveal,
  });
  const busy = launch.launching || launch.resolvingId !== null;

  // Filtering or switching view closes any open band — its anchor row may no
  // longer exist.
  const resetBand = detail.reset;
  useEffect(() => {
    resetBand();
  }, [kindFilter, search, view, resetBand]);

  // ── Featured rail ─────────────────────────────────────────────────────────
  // `highlights` arrives ranked and labelled; this is a join + copy layer
  // only — no sort, no re-prioritisation (homeData.ts header).
  const appById = useMemo(() => new Map(apps.map((a) => [a.id, a])), [apps]);
  const kindById = useMemo(() => new Map(apps.map((a) => [a.id, a.kind])), [apps]);
  const featured = useMemo(
    () =>
      buildRailCards({
        highlights,
        knownAppIds: new Set(appById.keys()),
        sessions: mySessions,
        kinds: kindById,
      }),
    [highlights, appById, kindById, mySessions],
  );

  /**
   * The rail must earn its space: with a catalogue no larger than the rail
   * itself, every card duplicates a tile already on screen. Exception: a live
   * session's card is the one-press resume control, so it stays even then.
   */
  const showRail =
    featured.length > 0 &&
    (apps.length > featured.length || featured.some((c) => Boolean(c.sessionId)));

  /** The hero's promise ("launch anything") is false over an empty/failed
   *  catalogue, so it is not rendered then. */
  const showHero = view === "home" && !error && (loading || apps.length > 0);

  /**
   * Only segments that can match something — computed over the whole sorted
   * catalogue, not the filtered set, so typing in search can't make segments
   * disappear underneath the cursor.
   */
  const kindOptions = useMemo(() => {
    const has = (k: KindFilter) => filterLibraryApps(sortedApps, k, "").length > 0;
    return (
      [
        { value: "all" as const, label: "All" },
        { value: "favourite" as const, label: "Favourites" },
        { value: "game" as const, label: "Games" },
        { value: "desktop" as const, label: "Desktops" },
        { value: "launcher" as const, label: "Launchers" },
      ] as const
    ).filter((o) => o.value === "all" || has(o.value));
  }, [sortedApps]);

  // One app and one segment each is noise around a single tile.
  const showKindFilter = !error && sortedApps.length > 1 && kindOptions.length > 1;

  // Un-favouriting the last favourite (or a catalogue reload) can retire the
  // segment that is currently selected. Fall back to "All" rather than leaving
  // the page filtered by a control that is no longer on screen.
  useEffect(() => {
    if (!kindOptions.some((o) => o.value === kindFilter)) setKindFilter("all");
  }, [kindOptions, kindFilter]);

  const confirmStopSession = useCallback(async () => {
    if (!token || !stopTarget || stopping) return;
    setStopping(true);
    try {
      await libraryApi.stopSession(token, stopTarget.id);
      setMySessions((prev) => prev.filter((s) => s.id !== stopTarget.id));
      setStopTarget(null);
    } catch (err) {
      addToast({
        variant: "danger",
        title: "Couldn't stop session",
        body: err instanceof ApiError ? err.message : "Try again.",
      });
    } finally {
      setStopping(false);
    }
  }, [token, stopTarget, stopping, addToast]);

  const toggleFavourite = useCallback(
    async (app: App) => {
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
    },
    [token, addToast],
  );

  const { openId, optionsOpen, close: closeDetail, setOptionsOpen, optionsToggleRef } = detail;
  const quickLaunch = launch.quickLaunch;
  // flushSync, not a bare setState: the band's interior is `inert` while the
  // overlay is open, and focusing an inert element is a no-op — the close has
  // to reach the DOM before focus returns to Adjust.
  const closeOptions = useCallback(() => {
    flushSync(() => setOptionsOpen(false));
    optionsToggleRef.current?.focus();
  }, [setOptionsOpen, optionsToggleRef]);
  const launchFromKey = useCallback((app: App) => void quickLaunch(app), [quickLaunch]);
  const resumeFromKey = useCallback(
    (sessionId: string) => navigate(`/app/session/${sessionId}`),
    [navigate],
  );
  useHomeKeys({
    apps,
    featured,
    blockedIds,
    railTrackRef,
    gridsRef,
    optionsOpen,
    bandOpen: openId !== null,
    smoothScroll: !prefersReducedMotion(),
    onCloseOptions: closeOptions,
    onCloseBand: closeDetail,
    onLaunch: launchFromKey,
    onResume: resumeFromKey,
  });

  const openApp = openId ? appById.get(openId) ?? null : null;

  const renderDetail = useCallback(
    (app: App): ReactNode =>
      openApp && detail.insertAfterId === app.id ? (
        <DetailBand
          // Keyed by app: two apps in one row share this slot, and a carried-over
          // draft would show app A's selection under app B's name.
          key={openApp.id}
          ref={detail.detailRef}
          app={openApp}
          codecCaps={codecCaps}
          launching={launch.launching}
          waitingForSlot={launch.waitingForSlot}
          profiles={detail.profilesFor(openApp.id)}
          optionsOpen={optionsOpen}
          optionsToggleRef={optionsToggleRef}
          onToggleOptions={() => setOptionsOpen(!optionsOpen)}
          onCloseOptions={closeOptions}
          onRetryProfiles={() => void detail.fetchProfiles(openApp)}
          onClose={closeDetail}
          onConfirmProfile={(profileId, streamCodec) =>
            void launch.launchApp(openApp, profileId, streamCodec)
          }
          onToggleFavourite={() => void toggleFavourite(openApp)}
          isBlocked={blockedIds.has(openApp.id)}
          blockedByName={blockedByName}
          liveSessionId={liveSession?.id ?? null}
          isLive={liveSession?.app_id === openApp.id}
          onResume={() => {
            if (liveSession) navigate(`/app/session/${liveSession.id}`);
          }}
        />
      ) : null,
    [
      openApp,
      detail,
      codecCaps,
      launch,
      optionsOpen,
      setOptionsOpen,
      optionsToggleRef,
      closeOptions,
      closeDetail,
      toggleFavourite,
      blockedIds,
      blockedByName,
      liveSession,
      navigate,
    ],
  );

  const renderGrid = useCallback(
    (list: App[], key: string) => (
      <div className="lib-grid" key={key}>
        {list.map((app) => (
          <LibraryTile
            key={app.id}
            app={app}
            isRunning={liveSession?.app_id === app.id}
            isBlocked={blockedIds.has(app.id)}
            blockedByName={blockedByName}
            isOpen={openId === app.id}
            isBusy={busy}
            coverClass={coverClassById.get(app.id) ?? coverClassAt(0)}
            tileRef={detail.setTileRef(app.id)}
            onOpen={() => detail.open(app, list)}
            onPlay={() => void quickLaunch(app)}
            detail={renderDetail(app)}
          />
        ))}
      </div>
    ),
    [
      liveSession,
      blockedIds,
      blockedByName,
      openId,
      busy,
      coverClassById,
      detail,
      quickLaunch,
      renderDetail,
    ],
  );

  // ── Render ────────────────────────────────────────────────────────────────
  return (
    <div className="home" data-view={view}>
      {summary && (
        <SessionSummaryCard
          summary={summary}
          onDismiss={() => navigate("/app", { replace: true, state: null })}
        />
      )}

      {/* Keyed by view so React mounts a fresh wrapper per nav (lets the
          one-shot CSS animation run). Summary/error/stop-modal stay outside
          it — a modal inside an `overflow: clip` box is asking for trouble. */}
      <div className="home-view" key={view} data-enter={enterDir}>
        {showHero && (
          <Hero hasRail={showRail}>
            {showRail && (
              <FeaturedRail
                cards={featured}
                trackRef={railTrackRef}
                appById={appById}
                coverClassById={coverClassById}
                busy={busy}
                blockedIds={blockedIds}
                onOpen={(app) => detail.reveal(app, false)}
                onPlay={(card, app) => {
                  if (card.sessionId) navigate(`/app/session/${card.sessionId}`);
                  else void quickLaunch(app);
                }}
              />
            )}
          </Hero>
        )}

        <LibraryGrid
          view={view}
          count={loading || error ? null : filteredApps.length}
          showFilter={showKindFilter}
          kindFilter={kindFilter}
          kindOptions={kindOptions}
          onKindFilter={setKindFilter}
          actions={
            liveSession && (
              <Button variant="ghost" onClick={() => setStopTarget(liveSession)}>
                Stop session
              </Button>
            )
          }
          groups={view === "library" ? sourceGroups : null}
          apps={filteredApps}
          renderGrid={renderGrid}
          gridsRef={gridsRef}
        >
          <LibraryStates
            loading={loading}
            error={error}
            total={apps.length}
            matched={filteredApps.length}
            search={search}
            filtered={kindFilter !== "all"}
            isAdmin={isAdmin}
            onRetry={loadApps}
            onAddApps={() => navigate("/admin/library/apps")}
            onClearFilter={() => setKindFilter("all")}
          />
        </LibraryGrid>
      </div>

      {stopTarget && (
        <StopSessionModal
          stopping={stopping}
          onCancel={() => setStopTarget(null)}
          onConfirm={() => void confirmStopSession()}
        />
      )}
    </div>
  );
}
