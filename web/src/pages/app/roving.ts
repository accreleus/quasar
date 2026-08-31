// One roving-focus model spanning the featured rail and the tile grid
// (UX assessment §3.5 item 9). No v3 mock covers TV/controller navigation.
//
// DOM-free and direction-based, not key-based: the Gamepad API is polling (a
// rAF loop, no events), so a future controller poller calls the same move(dir)
// the keyboard does. Regions, not one flat row list: the rail and the grid have
// different geometry, and regions keep the grid's measured-offsetTop maths
// untouched while making rail↔grid crossing one testable rule.
// AppHomeNext measures, calls, and focuses.

import { nextTileIndex, type NavKey } from "./libraryGrid";

/** A direction, not a key (see header). */
export type RoveDir = "left" | "right" | "up" | "down";

const DIR_TO_NAV: Record<RoveDir, NavKey> = {
  left: "ArrowLeft",
  right: "ArrowRight",
  up: "ArrowUp",
  down: "ArrowDown",
};

const KEY_TO_DIR: Record<string, RoveDir> = {
  ArrowLeft: "left",
  ArrowRight: "right",
  ArrowUp: "up",
  ArrowDown: "down",
};

/** `key` → direction, or null for a key this model does not own. The only
 *  place a keyboard vocabulary meets the model. */
export function dirForKey(key: string): RoveDir | null {
  return KEY_TO_DIR[key] ?? null;
}

/**
 * One region's measured geometry, in DOM order. Items sharing an `offsetTops`
 * value share a row (as `libraryGrid.nextTileIndex` requires); a rail passes
 * all-equal values. `centers` is per-item horizontal centre in viewport
 * coordinates, used only when crossing regions — viewport, because the rail is
 * scrolled and an offsetLeft match would aim at an off-screen card.
 */
export interface RovingRegion {
  offsetTops: readonly number[];
  centers: readonly number[];
}

export interface RovingPos {
  region: number;
  index: number;
}

function indicesInRow(offsetTops: readonly number[], rowTop: number): number[] {
  const out: number[] = [];
  for (let i = 0; i < offsetTops.length; i++) if (offsetTops[i] === rowTop) out.push(i);
  return out;
}

/** The candidate whose centre is closest to `center`; the first candidate when
 *  nothing has been measured (jsdom, or a region that has not laid out yet). */
function nearestByCenter(
  candidates: readonly number[],
  centers: readonly number[],
  center: number,
): number {
  let best = candidates[0];
  let bestDist = Infinity;
  for (const i of candidates) {
    const d = Math.abs((centers[i] ?? 0) - center);
    if (d < bestDist) {
      bestDist = d;
      best = i;
    }
  }
  return best;
}

/**
 * Where `dir` moves focus from `from`, or null to stay. Within a region this is
 * `libraryGrid.nextTileIndex` verbatim; only a vertical move that leaves the
 * region crosses into the adjacent one, landing on the horizontally nearest
 * item. Horizontal moves never cross regions — the rail's ends are ends
 * (launcher blueprint jumps regions with an explicit control).
 */
export function nextRovingPos(
  regions: readonly RovingRegion[],
  from: RovingPos,
  dir: RoveDir,
): RovingPos | null {
  const region = regions[from.region];
  if (!region) return null;

  const within = nextTileIndex(region.offsetTops, from.index, DIR_TO_NAV[dir]);
  if (within != null) return { region: from.region, index: within };

  if (dir !== "up" && dir !== "down") return null;

  const step = dir === "down" ? 1 : -1;
  const targetIndex = from.region + step;
  const target = regions[targetIndex];
  if (!target || target.offsetTops.length === 0) return null;

  const rowTop =
    dir === "down" ? target.offsetTops[0] : target.offsetTops[target.offsetTops.length - 1];
  const row = indicesInRow(target.offsetTops, rowTop);
  const center = region.centers[from.index] ?? 0;
  return { region: targetIndex, index: nearestByCenter(row, target.centers, center) };
}

/**
 * The scrollLeft needed for `item` to be fully visible with `pad` px clearance,
 * or null when it already is. A roving model must scroll its own focus into
 * view; the browser's free version lands the card flush against the edge under
 * the chevrons/fade, and `pad` buys it out (scroll-snap then settles it).
 * `item.contentLeft` is in scroll space, not `offsetLeft` — the surface is
 * absolutely positioned so offsetLeft carries no scroll term and read "already
 * visible" for every card; see `railContentLeft`.
 */
export function railScrollLeftFor(
  view: { scrollLeft: number; clientWidth: number },
  item: { contentLeft: number; width: number },
  pad = 0,
): number | null {
  const left = item.contentLeft - pad;
  const right = item.contentLeft + item.width + pad;
  if (left < view.scrollLeft) return Math.max(0, left);
  if (right > view.scrollLeft + view.clientWidth) return Math.max(0, right - view.clientWidth);
  return null;
}

/** `item`'s position in `track`'s scroll space. Rects, not offsets: two
 *  viewport rects plus the container's scrollLeft is offsetParent-independent. */
export function railContentLeft(
  track: { getBoundingClientRect(): { left: number }; scrollLeft: number },
  item: { getBoundingClientRect(): { left: number; width: number } },
): { contentLeft: number; width: number } {
  const t = track.getBoundingClientRect();
  const i = item.getBoundingClientRect();
  return { contentLeft: i.left - t.left + track.scrollLeft, width: i.width };
}
