// Windowed library grid: only rows near the viewport mount, spacers keep the
// scroll height, and the rows an arrow key or the open band needs stay put.

import { useMemo, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render } from "@testing-library/react";
import type { App } from "../../../api/types";
import { moveRovingFocus } from "./rovingFocus";
import { useDetailBand } from "./useDetailBand";
import { WindowedGrid, rowsAround } from "./WindowedGrid";

const COLS = 4;
const TILE_H = 300;
const GAP = 16;
const PITCH = TILE_H + GAP;

const apps = Array.from({ length: 400 }, (_, i) => ({ id: `a${i}`, name: `App ${i}` }) as unknown as App);

const rowOf = (el: HTMLElement) => Math.floor(Number(el.closest<HTMLElement>("[data-id]")!.dataset.id!.slice(1)) / COLS);

/** `scrolled`: how far the page is scrolled, so the grid's top is above the viewport. */
function fakeLayout(scrolled = 0) {
  vi.spyOn(window, "getComputedStyle").mockReturnValue({
    rowGap: `${GAP}px`,
    gridTemplateColumns: Array(COLS).fill("200px").join(" "),
  } as unknown as CSSStyleDeclaration);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const isTile = this.classList.contains("lib-tile");
    const h = isTile ? TILE_H : 0;
    const top = isTile || this.classList.contains("lib-tile-surface") ? rowOf(this) * PITCH - scrolled : -scrolled;
    return { top, bottom: top + h, height: h, left: 0, right: 0, width: 0, x: 0, y: top, toJSON() {} };
  });
  vi.spyOn(HTMLElement.prototype, "offsetTop", "get").mockImplementation(function (this: HTMLElement) {
    return this.classList.contains("lib-tile") ? rowOf(this) * PITCH : 0;
  });
}

const tile = (a: App) => (
  <div key={a.id} className="lib-tile" data-id={a.id}>
    <button className="lib-tile-surface" data-app-id={a.id}>
      {a.id}
    </button>
  </div>
);
const renderedIds = (c: HTMLElement) =>
  Array.from(c.querySelectorAll<HTMLElement>(".lib-tile")).map((t) => t.dataset.id);
const spacerPx = (c: HTMLElement) =>
  Array.from(c.querySelectorAll<HTMLElement>(".lib-gap")).reduce((n, s) => n + parseFloat(s.style.height), 0);

afterEach(() => vi.restoreAllMocks());

describe("rowsAround", () => {
  const m = { cols: COLS, pitch: PITCH, gap: GAP };
  it("covers the viewport ± screens and clamps to the grid", () => {
    expect(rowsAround(m, 0, 100, 2, 0, 800)).toEqual([0, Math.ceil((800 * 3) / PITCH)]);
    // A grid scrolled far above the viewport has an empty range at its end.
    const [s, e] = rowsAround(m, -1e6, 100, 2, 0, 800);
    expect(s).toBe(e);
  });
  it("widens the top for an open band", () => {
    const [plain] = rowsAround(m, -10000, 100, 2, 0, 800);
    const [slack] = rowsAround(m, -10000, 100, 2, 3, 800);
    expect(slack).toBe(plain - 3);
  });
});

describe("WindowedGrid", () => {
  it("renders every tile when layout cannot be measured", () => {
    const { container } = render(<WindowedGrid list={apps} tile={tile} pinIds={[]} />);
    expect(renderedIds(container)).toHaveLength(apps.length);
  });

  it("renders every tile of a short list", () => {
    fakeLayout();
    const { container } = render(<WindowedGrid list={apps.slice(0, 20)} tile={tile} pinIds={[]} />);
    expect(renderedIds(container)).toHaveLength(20);
    expect(container.querySelector(".lib-gap")).toBeNull();
  });

  it("mounts a window of rows and keeps the full scroll height with spacers", () => {
    fakeLayout();
    const { container } = render(<WindowedGrid list={apps} tile={tile} pinIds={[]} />);
    const ids = renderedIds(container);
    expect(ids.length).toBeLessThan(apps.length / 4);
    expect(ids[0]).toBe("a0");
    const rows = apps.length / COLS;
    const rendered = ids.length / COLS;
    // rendered rows + spacers + the row gaps between them == the whole grid
    const children = container.querySelector(".lib-grid")!.children.length - ids.length;
    expect(spacerPx(container) + rendered * PITCH - GAP + children * GAP).toBeCloseTo(rows * PITCH - GAP, 5);
  });

  it("keeps the pinned tile's row mounted wherever it is", () => {
    fakeLayout();
    const { container } = render(<WindowedGrid list={apps} tile={tile} pinIds={[null, "a301"]} />);
    const ids = renderedIds(container);
    expect(ids).toContain("a300");
    expect(ids).toContain("a303");
    expect(ids).toContain("a0");
  });
});

describe("WindowedGrid scrolled into a long library", () => {
  const SCROLLED = 20000;
  const mountedRows = (c: HTMLElement) => [...new Set(renderedIds(c).map((id) => Math.floor(Number(id!.slice(1)) / COLS)))];

  it("keeps the first row and the viewport window, with nothing between", () => {
    fakeLayout(SCROLLED);
    const { container } = render(
      <div className="home-lib">
        <WindowedGrid list={apps} tile={tile} pinIds={[]} />
      </div>,
    );
    const rows = mountedRows(container);
    expect(rows[0]).toBe(0);
    expect(rows[1]).toBeGreaterThan(40);
    expect(renderedIds(container)).not.toContain("a4");
  });

  it("ArrowUp from the top of the window lands on the row above, not the first row", () => {
    fakeLayout(SCROLLED);
    const { container } = render(
      <div className="home-lib">
        <WindowedGrid list={apps} tile={tile} pinIds={[]} />
      </div>,
    );
    const top = mountedRows(container)[1];
    const from = container.querySelector<HTMLElement>(`[data-app-id="a${top * COLS + 1}"]`)!;
    act(() => from.focus());
    expect(moveRovingFocus("up", { railTrack: null, grids: container, smooth: false })).toBe(true);
    expect(document.activeElement?.getAttribute("data-app-id")).toBe(`a${(top - 1) * COLS + 1}`);
  });

  it("ArrowDown from the first row lands on the second", () => {
    fakeLayout(SCROLLED);
    const { container } = render(
      <div className="home-lib">
        <WindowedGrid list={apps} tile={tile} pinIds={[]} />
      </div>,
    );
    act(() => container.querySelector<HTMLElement>('[data-app-id="a1"]')!.focus());
    expect(moveRovingFocus("down", { railTrack: null, grids: container, smooth: false })).toBe(true);
    expect(document.activeElement?.getAttribute("data-app-id")).toBe(`a${COLS + 1}`);
  });
});

describe("the detail band in a windowed grid", () => {
  const SCROLLED = 20000;
  let band: ReturnType<typeof useDetailBand>;

  // `startFiltered` hides a201 behind a kind filter that `onClearFilter` lifts.
  function Page({ startFiltered = false }: { startFiltered?: boolean }) {
    const [filtered, setFiltered] = useState(startFiltered);
    const list = useMemo(() => (filtered ? apps.filter((a) => a.id !== "a201") : apps), [filtered]);
    band = useDetailBand({
      token: null,
      visibleLists: [list],
      onClearFilter: () => setFiltered(false),
      viewKey: filtered ? "game" : "all",
      smoothScroll: false,
    });
    return (
      <div className="home-lib">
        <WindowedGrid
          list={list}
          pinIds={[band.openId, band.insertAfterId]}
          tile={(a) => (
            <>
              <div key={a.id} className="lib-tile" data-id={a.id}>
                <button className="lib-tile-surface" data-app-id={a.id} ref={band.setTileRef(a.id)}>
                  {a.id}
                </button>
              </div>
              {band.insertAfterId === a.id && <div key={`d-${a.id}`} className="detail" data-for={band.openId} />}
            </>
          )}
        />
      </div>
    );
  }

  it("opens a tile that is not mounted under its own row", () => {
    fakeLayout(SCROLLED);
    const { container } = render(<Page />);
    expect(renderedIds(container)).not.toContain("a201");
    act(() => band.open(apps[201], apps));
    // Row 50 is a200-a203; the old shared -1 offset anchored after the last title in the library.
    expect(band.insertAfterId).toBe("a203");
    const detail = container.querySelector<HTMLElement>(".detail")!;
    expect(detail.dataset.for).toBe("a201");
    expect(detail.previousElementSibling?.getAttribute("data-id")).toBe("a203");
  });

  it("opens a filtered-out tile under its own row once the filter is lifted", () => {
    fakeLayout(SCROLLED);
    const { container } = render(<Page startFiltered />);
    act(() => band.reveal(apps[201], false));
    expect(band.openId).toBe("a201");
    expect(band.insertAfterId).toBe("a203");
    expect(container.querySelector<HTMLElement>(".detail")!.previousElementSibling?.getAttribute("data-id")).toBe("a203");
  });

  it("opens a mounted tile under its own row", () => {
    fakeLayout(SCROLLED);
    const { container } = render(<Page />);
    const id = renderedIds(container)[5]!;
    const row = Math.floor(Number(id.slice(1)) / COLS);
    act(() => band.open(apps[Number(id.slice(1))], apps));
    expect(band.insertAfterId).toBe(`a${row * COLS + COLS - 1}`);
  });
});
