// Moving focus across the home page's two focusable regions — the featured
// rail and the tile grid — as one model.
//
// roving.ts owns the arithmetic and is pure (and says why this is regions, not
// one flat row list); this is the DOM half: it measures the two regions, hands
// them over, and moves focus to what comes back.
//
// Takes a direction, not an event: the native client's Gamepad API polls
// inside a rAF loop with no KeyboardEvent, so a future poller can call this
// directly; the page's keydown handler is just a key→direction map. Returns
// whether focus moved, so the caller decides on preventDefault.

import { tileBoxOf } from "./LibraryTile";
import {
  nextRovingPos,
  railContentLeft,
  railScrollLeftFor,
  type RoveDir,
  type RovingPos,
  type RovingRegion,
} from "../roving";

/** Clearance kept between a focused rail card and the rail edge. The prev/next
 *  chevrons overhang the track by 44px (home.css `left/right: -8px`), so a card
 *  scrolled flush to the edge sits underneath them. */
const RAIL_FOCUS_PAD = 44;

export interface RovingRefs {
  /** The rail's scroll container. Null in the Library view, where there is no rail. */
  railTrack: HTMLElement | null;
  /** The box holding every grid — one in Home view, one per source in Library view. */
  grids: HTMLElement | null;
  /** Whether to animate the page-level scroll that brings the rail into view. */
  smooth: boolean;
}

export function moveRovingFocus(dir: RoveDir, refs: RovingRefs): boolean {
  const active = document.activeElement as HTMLElement | null;
  if (!active) return false;

  const railItems = Array.from(
    refs.railTrack?.querySelectorAll<HTMLButtonElement>(".home-feat-surface") ?? [],
  );
  const tiles = Array.from(
    refs.grids?.querySelectorAll<HTMLButtonElement>(".lib-tile-surface") ?? [],
  );
  // Order is the model: rail above grid on screen. An empty region is dropped,
  // not kept as a zero-length row — in Library view the rail is unmounted, so
  // ArrowUp from the top row must find nothing.
  const groups = [railItems, tiles].filter((g) => g.length > 0);
  const railRegion = railItems.length > 0 ? 0 : -1;

  let from: RovingPos | null = null;
  for (let r = 0; r < groups.length && !from; r++) {
    const i = groups[r].indexOf(active as HTMLButtonElement);
    if (i >= 0) from = { region: r, index: i };
  }
  if (!from) return false;

  const regions: RovingRegion[] = groups.map((g, r) => ({
    // The rail is one row by construction (flex track) — measuring it risks a
    // sub-pixel split. The grid uses measured `offsetTop` off the tile box, so
    // column count is irrelevant.
    offsetTops: r === railRegion ? g.map(() => 0) : g.map((el) => tileBoxOf(el)?.offsetTop ?? 0),
    // Viewport centres, used only for the rail↔grid crossing ("down from this
    // card" lands under it); viewport not offset, since the rail scrolls.
    centers: g.map((el) => {
      const box = el.getBoundingClientRect();
      return box.left + box.width / 2;
    }),
  }));

  const to = nextRovingPos(regions, from, dir);
  if (!to) return false;
  const target = groups[to.region]?.[to.index];
  if (!target) return false;

  if (to.region !== railRegion) {
    target.focus();
    return true;
  }

  // The browser's own focus scroll-into-view pins the card under the chevrons;
  // take both axes ourselves instead. The track only handles vertical scroll
  // (crossing from the grid); horizontal is ours below.
  target.focus({ preventScroll: true });
  const track = refs.railTrack;
  // `scroll-margin-top` on the track (home.css) keeps `nearest` from parking it
  // under the sticky topbar.
  track?.scrollIntoView?.({ block: "nearest", behavior: refs.smooth ? "smooth" : "auto" });
  if (track) {
    const left = railScrollLeftFor(track, railContentLeft(track, target), RAIL_FOCUS_PAD);
    if (left != null) {
      // Instant, not smooth: a keystroke/gamepad burst restarts a smooth
      // scroll's animation each time, so it lags behind focus. Not about
      // scroll-snap — `smooth` and `auto` settle at the same offset in this
      // geometry (Chrome 150, measured); check `document.visibilityState`
      // before trusting any scroll-animation measurement, since a hidden tab
      // never runs the animation at all.
      if (typeof track.scrollTo === "function") track.scrollTo({ left, behavior: "auto" });
      else track.scrollLeft = left;
    }
  }
  return true;
}
