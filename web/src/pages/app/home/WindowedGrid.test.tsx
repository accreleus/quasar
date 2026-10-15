// Windowed library grid: only rows near the viewport mount, spacers keep the
// scroll height, and the rows an arrow key or the open band needs stay put.

import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import type { App } from "../../../api/types";
import { WindowedGrid, rowsAround } from "./WindowedGrid";

const COLS = 4;
const TILE_H = 300;
const GAP = 16;
const PITCH = TILE_H + GAP;

const apps = Array.from({ length: 400 }, (_, i) => ({ id: `a${i}`, name: `App ${i}` }) as unknown as App);

function fakeLayout() {
  vi.spyOn(window, "getComputedStyle").mockReturnValue({
    rowGap: `${GAP}px`,
    gridTemplateColumns: Array(COLS).fill("200px").join(" "),
  } as unknown as CSSStyleDeclaration);
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
    const h = this.classList.contains("lib-tile") ? TILE_H : 0;
    return { top: 0, bottom: h, height: h, left: 0, right: 0, width: 0, x: 0, y: 0, toJSON() {} };
  });
}

const tile = (a: App) => (
  <div key={a.id} className="lib-tile" data-id={a.id}>
    {a.id}
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
