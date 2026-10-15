// One `.lib-grid` that renders only the rows near the viewport and stands
// full-width spacers in for the rest, so a 1,500-title library does not mount
// 1,500 tiles and their art. Scroll height, scroll position and Page Down are
// unchanged: a spacer is exactly as tall as the rows it replaces.
//
// Rows are measured, not assumed: the column count is the computed
// `grid-template-columns` track list, the pitch a real tile's height plus the
// row gap. Rows are rendered while they lie within 2 screens and released once
// they are 3 screens away. Always rendered as well:
//   - the first grid's first row: the roving-focus model steps up and down by
//     "tiles in the first rendered row" (rovingFocus.ts),
//   - 2 rows either side of the focused tile, in whichever grid it is, so an
//     arrow key lands on the same tile as with every tile rendered,
//   - the open band's tile and the tile it hangs after (useDetailBand).
// Known loss: the browser's find-in-page only sees rendered tiles.

import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";
import type { ReactNode } from "react";
import type { App } from "../../../api/types";

/** A list this short is rendered whole (one or two rows: nothing to save). */
const PLAIN_UNDER = 24;
/** Tiles rendered on a cold mount, before the first measurement. */
const FIRST_RENDER = 60;

interface Metrics {
  cols: number;
  pitch: number;
  gap: number;
}
interface Win extends Metrics {
  start: number;
  end: number;
  /** Rows around the focused tile (any grid's), [fStart, fEnd). */
  fStart: number;
  fEnd: number;
  firstGrid: boolean;
}

// Last measurement, so a remount (back from a game page) starts at full height.
let lastMetrics: Metrics | null = null;
// Every mounted grid's re-measure: a tile taking focus runs them all, so the
// rows an arrow lands on next exist before the next key press.
const grids = new Set<() => void>();

/** Rows [start, end) covering the viewport ± `screens` screens; `slack` widens
 *  the top for an open band above the viewport, which pushes later rows down. */
export function rowsAround(
  m: Metrics,
  gridTop: number,
  rows: number,
  screens: number,
  slack: number,
  viewportH: number,
): [number, number] {
  const y = -gridTop;
  const start = Math.floor((y - screens * viewportH) / m.pitch) - slack;
  const end = Math.ceil((y + viewportH + screens * viewportH) / m.pitch);
  return [Math.max(0, Math.min(rows, start)), Math.max(0, Math.min(rows, end))];
}

export function WindowedGrid({
  list,
  tile,
  pinIds,
}: {
  list: readonly App[];
  tile: (app: App) => ReactNode;
  pinIds: readonly (string | null)[];
}) {
  const ref = useRef<HTMLDivElement>(null);
  const measured = useRef<Metrics | null>(null);
  const [win, setWin] = useState<Win | null>(() =>
    lastMetrics
      ? { ...lastMetrics, start: 0, end: Math.ceil(FIRST_RENDER / lastMetrics.cols), fStart: 0, fEnd: 0, firstGrid: false }
      : null,
  );
  // Layout never ran (display:none, a test DOM): render the whole list.
  const [unmeasured, setUnmeasured] = useState(false);
  const plain = list.length <= PLAIN_UNDER;

  // `sync`: commit before the next paint (a scroll jump must not show spacers).
  const update = useCallback((sync = false) => {
    const g = ref.current;
    if (!g) return;
    // A grid scrolled far off renders only spacers: reuse its last measurement.
    let cur = measured.current;
    const first = g.querySelector<HTMLElement>(":scope > .lib-tile");
    if (first) {
      const cs = getComputedStyle(g);
      const gap = parseFloat(cs.rowGap) || 0;
      const cols = Math.max(1, cs.gridTemplateColumns.split(" ").filter(Boolean).length);
      // A row is as tall as its tallest tile; over 150 rows one short column
      // drifts the spacers by pixels.
      let h = 0;
      for (let el: Element | null = first, i = 0; el && i < 2 * cols + 1; el = el.nextElementSibling, i++)
        if (el.classList.contains("lib-tile")) h = Math.max(h, el.getBoundingClientRect().height);
      cur = { cols, pitch: h + gap, gap };
      if (cur.pitch <= gap) {
        setUnmeasured(true);
        return;
      }
      measured.current = cur;
      lastMetrics = cur;
    }
    if (!cur) return;
    const m = cur;
    setUnmeasured(false);
    const vh = window.innerHeight;
    const rows = Math.ceil(list.length / m.cols);
    const top = g.getBoundingClientRect().top;
    const band = g.querySelector<HTMLElement>(":scope > .detail");
    const slack = band ? Math.ceil(band.offsetHeight / m.pitch) + 1 : 0;
    let fStart = 0;
    let fEnd = 0;
    const a = document.activeElement;
    if (a?.classList.contains("lib-tile-surface")) {
      const fr = a.getBoundingClientRect();
      const clamp = (r: number) => Math.max(0, Math.min(rows, r));
      fStart = clamp(Math.floor((fr.top - top - 2 * m.pitch) / m.pitch) - slack);
      fEnd = clamp(Math.ceil((fr.bottom - top + 2 * m.pitch) / m.pitch));
    }
    const firstGrid = g.closest(".home-lib")?.querySelector(".lib-grid") === g;
    const apply = (f: (prev: Win | null) => Win | null) => (sync ? flushSync(() => setWin(f)) : setWin(f));
    apply((prev) => {
      // Hysteresis: keep the rendered rows while they still cover ±1 screen and
      // lie within ±3, then re-cut at ±2, so a scroll re-renders about once a screen.
      const [ns, ne] = rowsAround(m, top, rows, 1, slack, vh);
      const covers = !!prev && prev.cols === m.cols && prev.pitch === m.pitch && prev.start <= ns && prev.end >= ne;
      const [os, oe] = rowsAround(m, top, rows, 3, slack, vh);
      if (
        covers &&
        prev.start >= os &&
        prev.end <= oe &&
        prev.fStart === fStart &&
        prev.fEnd === fEnd &&
        prev.firstGrid === firstGrid
      )
        return prev;
      const [start, end] = rowsAround(m, top, rows, 2, slack, vh);
      return { ...m, start, end, fStart, fEnd, firstGrid };
    });
  }, [list]);

  // Before paint: the first frame is already windowed (and at full height).
  useLayoutEffect(() => {
    if (plain) return;
    update();
    let raf = 0;
    const queue = () => {
      if (!raf) raf = requestAnimationFrame(() => ((raf = 0), update()));
    };
    const onScroll = () => update(true);
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("resize", queue);
    const ro = typeof ResizeObserver === "function" ? new ResizeObserver(queue) : null;
    if (ref.current) ro?.observe(ref.current);
    grids.add(update);
    return () => {
      grids.delete(update);
      cancelAnimationFrame(raf);
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("resize", queue);
      ro?.disconnect();
    };
  }, [plain, update]);

  if (plain || unmeasured) return <div className="lib-grid" ref={ref}>{list.map(tile)}</div>;
  const onFocus = () => grids.forEach((g) => g());
  if (!win) return <div className="lib-grid" ref={ref}>{list.slice(0, FIRST_RENDER).map(tile)}</div>;

  const { cols, pitch, gap } = win;
  const rows = Math.ceil(list.length / cols);
  const keep = new Set<number>();
  for (let r = win.start; r < Math.min(win.end, rows); r++) keep.add(r);
  for (let r = win.fStart; r < Math.min(win.fEnd, rows); r++) keep.add(r);
  if (win.firstGrid) keep.add(0);
  for (const id of pinIds) {
    const i = id ? list.findIndex((a) => a.id === id) : -1;
    if (i >= 0) keep.add(Math.floor(i / cols));
  }
  const out: ReactNode[] = [];
  const spacer = (from: number, to: number) => (
    <div key={`gap-${from}`} className="lib-gap" aria-hidden style={{ height: (to - from) * pitch - gap }} />
  );
  let cursor = 0;
  for (const r of [...keep].sort((a, b) => a - b)) {
    if (r > cursor) out.push(spacer(cursor, r));
    for (const app of list.slice(r * cols, r * cols + cols)) out.push(tile(app));
    cursor = r + 1;
  }
  if (cursor < rows) out.push(spacer(cursor, rows));

  return (
    <div className="lib-grid" ref={ref} onFocus={onFocus}>
      {out}
    </div>
  );
}
