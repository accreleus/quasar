// The roving-focus model that spans the featured rail and the tile grid
// (UX assessment §3.5 item 9). These are the assertions that would otherwise
// only be provable by driving a browser:
//   · a region is walked by the GRID's existing measured-offsetTop maths, so a
//     responsive column count and a grouped Library view still work,
//   · vertical moves CROSS between regions and horizontal ones never do,
//   · a crossing lands under the item it left, not at a fixed column,
//   · the rail scrolls its own focus into view with clearance for the chevrons.

import { describe, expect, it } from "vitest";
import {
  dirForKey,
  nextRovingPos,
  railContentLeft,
  railScrollLeftFor,
  type RovingRegion,
} from "./roving";

/** A rail: one row (all offsetTops equal), laid out left to right at `pitch`. */
function rail(count: number, pitch = 300, origin = 0): RovingRegion {
  return {
    offsetTops: Array.from({ length: count }, () => 0),
    centers: Array.from({ length: count }, (_, i) => origin + i * pitch),
  };
}

/** A grid of `count` items at `perRow` columns, rows `rowHeight` apart. */
function grid(count: number, perRow: number, pitch = 240, rowHeight = 300): RovingRegion {
  return {
    offsetTops: Array.from({ length: count }, (_, i) => Math.floor(i / perRow) * rowHeight),
    centers: Array.from({ length: count }, (_, i) => (i % perRow) * pitch),
  };
}

describe("dirForKey", () => {
  it("maps the four arrows and owns nothing else", () => {
    expect(dirForKey("ArrowLeft")).toBe("left");
    expect(dirForKey("ArrowRight")).toBe("right");
    expect(dirForKey("ArrowUp")).toBe("up");
    expect(dirForKey("ArrowDown")).toBe("down");
    expect(dirForKey("p")).toBeNull();
    expect(dirForKey("Escape")).toBeNull();
    expect(dirForKey("Tab")).toBeNull();
  });
});

describe("nextRovingPos — within the rail", () => {
  const regions = [rail(5), grid(9, 3)];

  it("walks left and right along the rail", () => {
    expect(nextRovingPos(regions, { region: 0, index: 1 }, "right")).toEqual({
      region: 0,
      index: 2,
    });
    expect(nextRovingPos(regions, { region: 0, index: 1 }, "left")).toEqual({
      region: 0,
      index: 0,
    });
  });

  it("stops at the ends rather than wrapping", () => {
    expect(nextRovingPos(regions, { region: 0, index: 0 }, "left")).toBeNull();
    expect(nextRovingPos(regions, { region: 0, index: 4 }, "right")).toBeNull();
  });

  it("never leaves the rail sideways — a row's ends are ends", () => {
    // Arrowing off the end of the rail must NOT dump focus into the grid.
    expect(nextRovingPos(regions, { region: 0, index: 4 }, "right")).toBeNull();
  });

  it("has nothing above it", () => {
    expect(nextRovingPos(regions, { region: 0, index: 2 }, "up")).toBeNull();
  });
});

describe("nextRovingPos — within the grid", () => {
  // 9 tiles at 3 columns. Unchanged behaviour: this is libraryGrid.nextTileIndex.
  const regions = [rail(4), grid(9, 3)];

  it("moves down a row by the MEASURED column count, not an assumed one", () => {
    expect(nextRovingPos(regions, { region: 1, index: 1 }, "down")).toEqual({
      region: 1,
      index: 4,
    });
    // Same model, 4 columns wide — the stride follows the measurement.
    const wide = [rail(4), grid(12, 4)];
    expect(nextRovingPos(wide, { region: 1, index: 1 }, "down")).toEqual({
      region: 1,
      index: 5,
    });
  });

  it("moves up a row inside the grid without touching the rail", () => {
    expect(nextRovingPos(regions, { region: 1, index: 7 }, "up")).toEqual({
      region: 1,
      index: 4,
    });
  });

  it("has nothing below it", () => {
    expect(nextRovingPos(regions, { region: 1, index: 8 }, "down")).toBeNull();
  });
});

describe("nextRovingPos — crossing between rail and grid", () => {
  it("goes DOWN from a rail card into the grid's first row, under that card", () => {
    // Rail cards centred at 0/300/600/900; grid columns at 0/240/480/720.
    const regions = [rail(4), grid(12, 4)];
    expect(nextRovingPos(regions, { region: 0, index: 0 }, "down")).toEqual({
      region: 1,
      index: 0,
    });
    // 600 is nearest column centre 480 (index 2), not 720.
    expect(nextRovingPos(regions, { region: 0, index: 2 }, "down")).toEqual({
      region: 1,
      index: 2,
    });
    // 900 is past the last column — it clamps to the nearest, which is the last.
    expect(nextRovingPos(regions, { region: 0, index: 3 }, "down")).toEqual({
      region: 1,
      index: 3,
    });
  });

  it("goes UP from the grid's FIRST row into the rail, under that tile", () => {
    const regions = [rail(4), grid(12, 4)];
    expect(nextRovingPos(regions, { region: 1, index: 0 }, "up")).toEqual({
      region: 0,
      index: 0,
    });
    // Column centre 480 is nearest rail centre 600 (index 2), not 300.
    expect(nextRovingPos(regions, { region: 1, index: 2 }, "up")).toEqual({
      region: 0,
      index: 2,
    });
  });

  it("enters the rail's ONLY row however far the rail is scrolled", () => {
    // The rail scrolled right: its cards' viewport centres are all negative or
    // small. Crossing still lands somewhere in the rail rather than nowhere.
    const regions = [rail(4, 300, -450), grid(8, 4)];
    const to = nextRovingPos(regions, { region: 1, index: 3 }, "up");
    expect(to?.region).toBe(0);
    expect(to?.index).toBeGreaterThanOrEqual(0);
  });

  it("does not cross with a horizontal move in either direction", () => {
    const regions = [rail(3), grid(6, 3)];
    expect(nextRovingPos(regions, { region: 0, index: 2 }, "right")).toBeNull();
    expect(nextRovingPos(regions, { region: 1, index: 0 }, "left")).toBeNull();
  });

  it("finds no rail to cross into when the rail is not rendered", () => {
    // Library view unmounts the hero, so the grid is the only region there is.
    const regions = [grid(6, 3)];
    expect(nextRovingPos(regions, { region: 0, index: 1 }, "up")).toBeNull();
  });

  it("survives an out-of-range position instead of throwing", () => {
    expect(nextRovingPos([rail(2)], { region: 7, index: 0 }, "down")).toBeNull();
  });
});

describe("railScrollLeftFor", () => {
  const view = { scrollLeft: 0, clientWidth: 600 };

  it("leaves a card that is already comfortably visible alone", () => {
    expect(railScrollLeftFor(view, { contentLeft: 100, width: 280 }, 44)).toBeNull();
  });

  it("scrolls right so a card beyond the edge is fully visible, with clearance", () => {
    // 620..900 with 44px of clearance needs 944 visible → scrollLeft 344.
    expect(railScrollLeftFor(view, { contentLeft: 620, width: 280 }, 44)).toBe(344);
  });

  it("scrolls left when the card is behind the start of the track", () => {
    expect(
      railScrollLeftFor({ scrollLeft: 600, clientWidth: 600 }, { contentLeft: 300, width: 280 }, 44),
    ).toBe(256);
  });

  it("counts a card sitting under the chevron as NOT visible", () => {
    // Flush against the left edge — which is exactly where the browser's own
    // focus scroll-into-view leaves it, and exactly where the chevron is.
    expect(
      railScrollLeftFor({ scrollLeft: 300, clientWidth: 600 }, { contentLeft: 300, width: 280 }, 44),
    ).toBe(256);
  });

  it("never asks for a negative scrollLeft", () => {
    expect(
      railScrollLeftFor({ scrollLeft: 20, clientWidth: 600 }, { contentLeft: 0, width: 280 }, 44),
    ).toBe(0);
  });
});

describe("railContentLeft", () => {
  const rect = (left: number, width: number) => ({
    getBoundingClientRect: () => ({ left, width }),
  });

  it("measures from rects, so the container's own scroll is included", () => {
    // Track at viewport x=100, already scrolled 200. A card painted at x=140 is
    // therefore 240 from the START OF THE CONTENT, not 40.
    const track = { ...rect(100, 600), scrollLeft: 200 };
    expect(railContentLeft(track, rect(140, 280))).toEqual({ contentLeft: 240, width: 280 });
  });

  it("is independent of the offsetParent chain — the bug this exists for", () => {
    // `.home-feat-surface` is `position: absolute; inset: 0` inside its card,
    // so its offsetLeft is 0 for EVERY card. Rects give three distinct answers.
    const track = { ...rect(0, 600), scrollLeft: 0 };
    expect(railContentLeft(track, rect(0, 280)).contentLeft).toBe(0);
    expect(railContentLeft(track, rect(300, 280)).contentLeft).toBe(300);
    expect(railContentLeft(track, rect(600, 280)).contentLeft).toBe(600);
  });
});
