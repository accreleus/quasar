// The band is anchored after the last tile of its own row, and a resize
// recomposes the rows — so the anchor has to be re-measured, debounced, or the
// band sits mid-row until the next click.

import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import * as libraryApi from "../../../api/library";
import type { App } from "../../../api/types";
import { useDetailBand } from "./useDetailBand";

vi.mock("../../../api/library");

const mocked = vi.mocked(libraryApi);

const app = (id: string): App => ({ id, name: id }) as unknown as App;
const APPS = [app("a1"), app("a2"), app("a3")];
/** Stable identity: the resize effect keys on it, and a fresh array each render
 *  would tear down the pending timer under test. */
const LISTS = [APPS];

/** Measured row geometry, mutated between renders to model a resize. */
const offsetTops = new Map<string, number>();

/** A tile as LibraryTile renders it: the ref is the inner surface, the measured
 *  box is its `.lib-tile` ancestor (tileBoxOf). */
function tile(id: string): HTMLButtonElement {
  const box = document.createElement("div");
  box.className = "lib-tile";
  Object.defineProperty(box, "offsetTop", { get: () => offsetTops.get(id) ?? 0 });
  const surface = document.createElement("button");
  box.appendChild(surface);
  document.body.appendChild(box);
  return surface;
}

function mount() {
  const hook = renderHook(() =>
    useDetailBand({
      token: "tok",
      visibleLists: LISTS,
      onClearFilter: () => {},
      smoothScroll: false,
    }),
  );
  act(() => {
    for (const a of APPS) hook.result.current.setTileRef(a.id)(tile(a.id));
  });
  return hook;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.resetAllMocks();
  // Never settles: the profiles fetch is not what this file is about, and a
  // resolution would land outside act().
  mocked.getProfiles.mockReturnValue(new Promise(() => {}) as never);
  offsetTops.clear();
  for (const a of APPS) offsetTops.set(a.id, 0);
});

afterEach(() => {
  vi.useRealTimers();
  document.body.innerHTML = "";
});

describe("useDetailBand re-anchors on resize", () => {
  it("moves the band to the last tile of its new row, 150ms after the resize", () => {
    const { result } = mount();
    act(() => result.current.open(APPS[0], APPS));
    expect(result.current.insertAfterId).toBe("a3");

    // Narrower grid: a3 wraps, so a1's row now ends at a2.
    offsetTops.set("a3", 240);
    act(() => void window.dispatchEvent(new Event("resize")));
    act(() => void vi.advanceTimersByTime(149));
    expect(result.current.insertAfterId).toBe("a3");

    act(() => void vi.advanceTimersByTime(1));
    expect(result.current.insertAfterId).toBe("a2");
  });

  it("re-measures once for a burst of resizes, at the last one", () => {
    const { result } = mount();
    act(() => result.current.open(APPS[0], APPS));

    offsetTops.set("a3", 240);
    for (let i = 0; i < 5; i++) {
      act(() => void window.dispatchEvent(new Event("resize")));
      act(() => void vi.advanceTimersByTime(100));
    }
    // Every resize restarted the timer, so nothing has fired after 500ms.
    expect(result.current.insertAfterId).toBe("a3");
    act(() => void vi.advanceTimersByTime(150));
    expect(result.current.insertAfterId).toBe("a2");
  });

  it("stops measuring once the band is closed", () => {
    const { result } = mount();
    act(() => result.current.open(APPS[0], APPS));
    act(() => result.current.close());

    offsetTops.set("a3", 240);
    act(() => void window.dispatchEvent(new Event("resize")));
    act(() => void vi.advanceTimersByTime(200));
    expect(result.current.insertAfterId).toBeNull();
  });
});
