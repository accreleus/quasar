// The hero's featured rail (v3 handoff §B "Hero + featured rail"), moved out
// of AppHomeNext.tsx when the v3 home split the page into components.
//
// Horizontally scroll-snapped cards with prev/next controls that disappear at
// the ends. `cards` arrives ranked and labelled by the server
// (`GET /v1/me/highlights` → homeData.buildRailCards); this file renders, it
// does not rank.
//
// Every card is a real focusable button (surface + layered play control, since
// a <button> cannot contain a <button>); arrows are pointer-only and removed
// from the DOM (not just faded) once there is nothing to scroll to.
//
// Roving tabindex: one card is `tabIndex=0` (last focused), the rest are `-1`,
// so Tab enters/leaves once while arrows walk it (AppHomeNext.moveFocus) —
// most of the track is off-screen, so a tab stop per card would be a tab stop
// per invisible target. Driven by `onFocus`, not the arrow handler, so it stays
// correct however focus arrived.
//
// Not ported from the mock: the poster's optional `.tier` chip. Nothing on
// `Highlight` or `AppListItem` says what tier a card would launch at, and the
// mock's chip is a static string.

import { useCallback, useEffect, useState } from "react";
import type { RefObject } from "react";
import type { App } from "../../../api/types";
import { IconChevronLeft, IconChevronRight, IconPlayGlyph } from "../../../components/icons";
import { artFor } from "../../../lib/appArtwork";
import { appGlyph } from "../../../lib/appGlyph";
import type { RailCard } from "../homeData";
import { coverClassAt } from "../libraryGrid";

/** Whether the caller has asked for no animation. Rail scrolling honours it
 *  the same way the page's own transitions do. */
function prefersReducedMotion(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

export interface FeaturedRailProps {
  cards: readonly RailCard[];
  /** The scroll container, owned by the page: the roving-focus model spans the
   *  rail and the grid, so it has to be able to scroll this one itself. */
  trackRef: RefObject<HTMLDivElement | null>;
  appById: ReadonlyMap<string, App>;
  coverClassById: ReadonlyMap<string, string>;
  busy: boolean;
  blockedIds: ReadonlySet<string>;
  onOpen: (app: App) => void;
  onPlay: (card: RailCard, app: App) => void;
}

export function FeaturedRail({
  cards,
  trackRef,
  appById,
  coverClassById,
  busy,
  blockedIds,
  onOpen,
  onPlay,
}: FeaturedRailProps) {
  const [edges, setEdges] = useState({ prev: false, next: false });
  // The rail's single tab stop. Re-resolved by id every render so a refreshed
  // highlights list dropping the remembered card falls back to the first,
  // rather than leaving the rail with no tab stop.
  const [activeId, setActiveId] = useState<string | null>(null);
  const tabStopId = cards.find((c) => c.appId === activeId)?.appId ?? cards[0]?.appId ?? null;

  const measure = useCallback(() => {
    const el = trackRef.current;
    if (!el) return;
    const max = el.scrollWidth - el.clientWidth;
    // The mock's own rule: prev appears past 20px of scroll, next disappears
    // within 20px of the end.
    setEdges({ prev: el.scrollLeft > 20, next: el.scrollLeft < max - 20 });
  }, [trackRef]);

  useEffect(() => {
    measure();
    if (typeof window === "undefined") return;
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [measure, cards.length]);

  // Smooth is fine here: the step is near-page (never less than one card), so
  // it always clamps/snaps past the current position, and `smooth`/`auto`
  // settle identically in this geometry (Chrome 150, measured) — nothing to win
  // by dropping the animation. (moveFocus uses `auto` for a different reason:
  // keystroke bursts, not snapping — see the note there.) The step follows the
  // Track's width rather than the mock's flat 600px, which is two cards on a
  // desktop and four on an ultrawide.
  const scrollRail = useCallback(
    (dir: -1 | 1) => {
      const el = trackRef.current;
      if (!el) return;
      el.scrollBy?.({
        left: dir * Math.max(280, el.clientWidth * 0.8),
        behavior: prefersReducedMotion() ? "auto" : "smooth",
      });
    },
    [trackRef],
  );

  return (
    <div className="home-rail">
      <div className="home-rail-track" ref={trackRef} onScroll={measure}>
        {cards.map((card, i) => {
          const app = appById.get(card.appId);
          if (!app) return null;
          return (
            <FeaturedCard
              key={card.appId}
              card={card}
              app={app}
              coverClass={coverClassById.get(app.id) ?? coverClassAt(i)}
              /* #386: first card is this page's LCP element (no hero image);
                 eager + high-priority for that reason, every other poster is
                 lazy like the grid tiles. */
              priority={i === 0}
              disabled={busy || (blockedIds.has(app.id) && !card.sessionId)}
              isTabStop={card.appId === tabStopId}
              onFocus={() => setActiveId(card.appId)}
              onOpen={() => onOpen(app)}
              onPlay={() => onPlay(card, app)}
            />
          );
        })}
      </div>
      {edges.next && <div className="home-rail-fade" aria-hidden />}
      {edges.prev && (
        <button
          type="button"
          className="home-rail-btn prev"
          aria-label="Scroll featured left"
          onClick={() => scrollRail(-1)}
        >
          <IconChevronLeft strokeWidth={1.8} />
        </button>
      )}
      {edges.next && (
        <button
          type="button"
          className="home-rail-btn next"
          aria-label="Scroll featured right"
          onClick={() => scrollRail(1)}
        >
          <IconChevronRight strokeWidth={1.8} />
        </button>
      )}
    </div>
  );
}

interface FeaturedCardProps {
  card: RailCard;
  app: App;
  coverClass: string;
  priority: boolean;
  disabled: boolean;
  /** The rail's one tab stop (roving tabindex — see FeaturedRail). */
  isTabStop: boolean;
  onFocus: () => void;
  onOpen: () => void;
  onPlay: () => void;
}

/**
 * One `.feat` card: a 2:3 poster over a gradient fallback, then the mock's
 * three-line body — kicker (why this card is here), action (what pressing it
 * does), name (which game), muted at the foot.
 */
function FeaturedCard({
  card,
  app,
  coverClass,
  priority,
  disabled,
  isTabStop,
  onFocus,
  onOpen,
  onPlay,
}: FeaturedCardProps) {
  const [artFailed, setArtFailed] = useState(false);
  const art = artFailed ? null : artFor(app, "tile");
  const playLabel = card.sessionId ? `Resume ${app.name}` : `Play ${app.name}`;
  return (
    <article className="home-feat" data-variant={card.reason}>
      <div className="home-feat-poster">
        <span className={`cover ${coverClass}`} aria-hidden>
          <span className="glyph">{appGlyph(app.name)}</span>
          {art && (
            <img
              src={art}
              alt=""
              className="cover-img"
              decoding="async"
              onError={() => setArtFailed(true)}
              {...(priority ? { fetchpriority: "high" } : { loading: "lazy" as const })}
            />
          )}
        </span>
        <button
          type="button"
          className="home-feat-play"
          tabIndex={-1}
          disabled={disabled}
          aria-label={playLabel}
          onClick={onPlay}
        >
          <IconPlayGlyph />
        </button>
      </div>
      <div className="home-feat-body">
        <div className={`home-feat-kicker ${card.kicker.variant}`}>
          {card.kicker.variant === "live" && <span className="live-dot" aria-hidden />}
          {card.kicker.text}
        </div>
        <div className="home-feat-action">{card.action}</div>
        <div className="home-feat-name">{app.name}</div>
      </div>
      {/* `data-app-id` mirrors `.lib-tile-surface`'s: it is what the page's one
          keydown handler resolves `P` against, on the rail as on the grid. */}
      <button
        type="button"
        className="home-feat-surface"
        data-app-id={app.id}
        tabIndex={isTabStop ? 0 : -1}
        aria-label={`${app.name}, ${card.kicker.text}. Show details`}
        onFocus={onFocus}
        onClick={onOpen}
      />
    </article>
  );
}
