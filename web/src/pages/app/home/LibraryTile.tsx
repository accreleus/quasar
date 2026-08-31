// The library grid tile (v3 handoff §B "Tile anatomy"), moved out of
// libraryDetail.tsx when the v3 home split the page into components.
//
// The mock's tile is one 2:3 box: a gradient fallback carrying a glyph and the
// game's name, the cover art layered over it, a favourite heart, a Running
// chip and a hover play overlay. It is a single `<button class="tile">` there.
// It is a `<div>` here for one reason: a button cannot contain a button, and
// this tile has two actions — open the detail band, and launch. So the same
// layering the page has always used survives the restyle:
//   .lib-tile-surface  full-bleed, inset:0, the one tab stop per tile; opens the band
//   .lib-tile-play     tabindex=-1, layered above it; launches (gated)
// The play control stays out of the tab order and is pointer-inert until
// hover/focus-within (home.css) so an invisible centre button cannot launch on
// a stray tap; `P` on the focused tile is its keyboard route (AppHomeNext).
// The mock's `.playov` scrim is `.lib-tile::before` rather than a third
// element: it is decorative, and an element that exists only to be dimmed is
// an element the roving-focus queries have to keep stepping over.
//
// `data-open` mirrors the surface button's `aria-expanded` so CSS can style the
// tile from a child control's state without `:has()`.

import { useState } from "react";
import type { ReactNode } from "react";
import type { App } from "../../../api/types";
import { Chip } from "../../../components/Chip";
import { IconHeart, IconPlayGlyph } from "../../../components/icons";
import { artFor } from "../../../lib/appArtwork";
import { appGlyph } from "../../../lib/appGlyph";

/** The `.lib-tile` box a surface button belongs to. The surface button is
 * absolutely positioned and inset:0, so its own `offsetTop` is always 0 —
 * row detection has to measure the tile, not the control inside it. */
export function tileBoxOf(surface: Element | null | undefined): HTMLElement | null {
  return (surface?.closest(".lib-tile") as HTMLElement | null) ?? null;
}

export interface LibraryTileProps {
  app: App;
  isRunning: boolean;
  /** A live session on this app's home (parent or sibling, spec §2.2) is
   * running — a launch here would 409 home_in_use. Presentation only. */
  isBlocked: boolean;
  /** Launcher tile name to show while blocked; null when nothing is blocked. */
  blockedByName: string | null;
  isOpen: boolean;
  isBusy: boolean;
  coverClass: string;
  tileRef: (el: HTMLButtonElement | null) => void;
  onOpen: () => void;
  onPlay: () => void;
  /** The detail band, when this tile is the last one in the opened row. Kept
   *  as a sibling of the tile so it is always the next grid child. */
  detail?: ReactNode;
}

export function LibraryTile({
  app,
  isRunning,
  isBlocked,
  blockedByName,
  isOpen,
  isBusy,
  coverClass,
  tileRef,
  onOpen,
  onPlay,
  detail = null,
}: LibraryTileProps) {
  // The mock's `onerror="this.remove()"`: the gradient + glyph underneath is a
  // design, not a broken state, so a 404 cover falls back to it instead of
  // leaving a torn image icon.
  const [artFailed, setArtFailed] = useState(false);
  const art = artFailed ? null : artFor(app, "tile");
  const detailId = `lib-detail-${app.id}`;
  return (
    <>
      <div className="lib-tile" data-open={isOpen ? "true" : undefined}>
        {/* The fallback is always rendered and the cover sits on top of it —
            that is what makes the name visible on the product's default,
            artwork-less catalogue without a hover. */}
        <span className={`cover ${coverClass}`} aria-hidden>
          <span className="glyph">{appGlyph(app.name)}</span>
          <span className="fnm">{app.name}</span>
          {art && (
            /* #386: eager parallel full-res fetch of every tile hit 14.5 MB on
               a cold load (unusable over VPN). `loading="lazy"` on EVERY tile
               including row 1 is deliberate: the browser never defers an
               in-viewport image and shrinks its prefetch margin on a slow link,
               adapting better than a hardcoded row count. No width/height
               needed: `.cover` is `aspect-ratio: 2/3` with `.cover-img`
               `position:absolute; inset:0`, so layout cannot shift. */
            <img
              src={art}
              alt=""
              className="cover-img"
              loading="lazy"
              decoding="async"
              onError={() => setArtFailed(true)}
            />
          )}
        </span>
        {isRunning && (
          <span className="running">
            <Chip variant="success" dot>Running</Chip>
          </span>
        )}
        {/* spec §2.2: a live session on this tile's home (parent or sibling)
            is running. Presentation only; never both with `.running`. */}
        {isBlocked && (
          <span className="blocked">
            <Chip variant="warning" title={`${blockedByName} is already running`}>In use</Chip>
          </span>
        )}
        {app.favourite && (
          <span className="fav-marker">
            <IconHeart filled />
            <span className="sr-only">Favourite</span>
          </span>
        )}
        {/* Play vs Resume (UX §3.3): when this app owns the live session the
            press navigates instead of launching (a launch would 409), so it is
            labelled "Resume"; `data-action` drives home.css. Busy never
            disables Resume (navigation, not a new session); a blocked tile
            stays disabled — it has nothing to resume into. */}
        <button
          type="button"
          className="lib-tile-play"
          data-action={isRunning ? "resume" : "launch"}
          tabIndex={-1}
          disabled={isBlocked || (isBusy && !isRunning)}
          aria-label={isRunning ? `Resume ${app.name}` : `Play ${app.name}`}
          onClick={onPlay}
        >
          <IconPlayGlyph />
          {/* The mock has no running-tile state for this control; the visible
              word stays (UX §3.3) so a press that navigates instead of
              launching says so before it is pressed. */}
          {isRunning && <span className="lib-tile-play-label">Resume</span>}
        </button>
        <button
          type="button"
          className="lib-tile-surface"
          data-app-id={app.id}
          aria-label={app.name}
          aria-expanded={isOpen}
          aria-controls={detailId}
          onClick={onOpen}
          ref={tileRef}
        />
      </div>
      {detail}
    </>
  );
}
