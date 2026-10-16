import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { moveRovingFocus } from "./rovingFocus";

// Two letter sections of 8 tiles at 6 columns (a full row, then two). jsdom has
// no layout, so each tile carries its row top and column in data attributes.
function mount(): HTMLElement {
  const grids = document.createElement("div");
  for (const [letter, top] of [["A", 0], ["B", 300]] as const) {
    const section = document.createElement("section");
    section.dataset.letterSection = letter;
    for (let i = 0; i < 8; i++) {
      const box = document.createElement("div");
      box.className = "lib-tile";
      box.dataset.top = String(top + (i < 6 ? 0 : 100));
      box.dataset.col = String(i % 6);
      const surface = document.createElement("button");
      surface.className = "lib-tile-surface";
      surface.id = `${letter}${i}`;
      box.append(surface);
      section.append(box);
    }
    grids.append(section);
  }
  document.body.append(grids);
  return grids;
}

const proto = HTMLElement.prototype;
const original = Object.getOwnPropertyDescriptor(proto, "offsetTop");

beforeEach(() => {
  Object.defineProperty(proto, "offsetTop", { configurable: true, get() { return Number(this.dataset.top ?? 0); } });
  proto.getBoundingClientRect = function (this: HTMLElement) {
    const col = Number((this.closest(".lib-tile") as HTMLElement | null)?.dataset.col ?? 0);
    return { left: col * 100, width: 100 } as DOMRect;
  };
});
afterEach(() => {
  if (original) Object.defineProperty(proto, "offsetTop", original);
  else delete (proto as unknown as Record<string, unknown>).offsetTop;
  document.body.replaceChildren();
});

describe("moveRovingFocus across letter sections", () => {
  const press = (grids: HTMLElement, from: string, dir: "down" | "up") => {
    document.getElementById(from)!.focus();
    expect(moveRovingFocus(dir, { railTrack: null, grids, smooth: false })).toBe(true);
    return document.activeElement?.id;
  };

  it("steps down within a section's own rows, to the nearest column", () => {
    expect(press(mount(), "A4", "down")).toBe("A7");
  });

  it("crosses into the next section's first row, not a flat stride away", () => {
    expect(press(mount(), "A6", "down")).toBe("B0");
  });

  it("goes up into the previous section's last row", () => {
    expect(press(mount(), "B2", "up")).toBe("A7");
  });
});
