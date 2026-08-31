// The v3 library tile (handoff §B "Tile anatomy").
//
// Two of these assertions are older than the v3 pass and are carried forward
// rather than rewritten, because both guard defects that reached users:
//
//  · The name is not hover-gated (UX assessment §2.1/§3.1). It used to live
//    inside an `opacity: 0` overlay, which made the only text identifying a
//    game a mouse-only affordance — invisible on touch, at TV distance, and
//    (worst) in the product's own default state, where `cover_url` is null and
//    every tile is a two-letter glyph on a gradient. In v3 it is `.fnm` inside
//    the always-rendered `.cover` fallback, with the artwork layered over it:
//    no artwork, and the name is the tile; artwork, and the box art carries the
//    title itself. The structural fact worth pinning is that it is not inside a
//    hover-gated element.
//  · Artwork stays lazy (#386). A 17-app catalogue fetching every cover
//    eagerly was 14.5 MB on one page load, which made the web UI unusable over
//    a VPN while the stream itself was fine. One attribute, so exactly the kind
//    of thing a restyle drops.

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { LibraryTile, tileBoxOf } from "./LibraryTile";
import type { App } from "../../../api/types";

function app(id: string, name: string, over: Partial<App> = {}): App {
  return {
    id,
    name,
    description: "",
    kind: "game",
    cover_url: null,
    hero_url: null,
    parent_app_id: null,
    external_source: "",
    external_id: "",
    default_width: 1920,
    default_height: 1080,
    default_fps: 60,
    default_bitrate_kbps: 8000,
    favourite: false,
    enabled: true,
    ...over,
  } as unknown as App;
}

function renderTiles(apps: App[], over: Partial<Parameters<typeof LibraryTile>[0]> = {}) {
  return render(
    <MemoryRouter>
      <div className="lib-grid">
        {apps.map((a) => (
          <LibraryTile
            key={a.id}
            app={a}
            isRunning={false}
            isBlocked={false}
            blockedByName={null}
            isOpen={false}
            isBusy={false}
            coverClass="cv-violet"
            tileRef={() => {}}
            onOpen={() => {}}
            onPlay={() => {}}
            {...over}
          />
        ))}
      </div>
    </MemoryRouter>,
  );
}

describe("library tile", () => {
  it("renders every tile's name in the always-rendered fallback, not a hover-gated box", () => {
    renderTiles([app("a1", "Portal 2"), app("a2", "Hades"), app("a3", "Celeste")]);

    const names = Array.from(document.querySelectorAll<HTMLElement>(".lib-tile .cover .fnm")).map(
      (el) => el.textContent,
    );
    expect(names).toEqual(["Portal 2", "Hades", "Celeste"]);
    // The glyph is the artwork-less tile's other half.
    expect(
      Array.from(document.querySelectorAll<HTMLElement>(".lib-tile .glyph")).map((e) => e.textContent),
    ).toEqual(["P2", "H", "C"]);
  });

  it("layers the cover over the fallback and keeps it lazy (#386)", () => {
    renderTiles([
      app("a1", "Portal 2", { cover_url: "/v1/artwork/assets/a1.png" }),
      app("a2", "Hades", { cover_url: "/v1/artwork/assets/a2.png" }),
    ]);

    const imgs = Array.from(document.querySelectorAll<HTMLImageElement>("img.cover-img"));
    expect(imgs).toHaveLength(2);
    for (const img of imgs) {
      expect(img.getAttribute("loading")).toBe("lazy");
      expect(img.getAttribute("decoding")).toBe("async");
      // Inside the fallback, so the gradient and name are underneath it.
      expect(img.closest(".cover")).not.toBeNull();
    }
    expect(document.querySelectorAll(".fnm")).toHaveLength(2);
  });

  it("drops a cover that fails to load back to the gradient tile", () => {
    renderTiles([app("a1", "Portal 2", { cover_url: "/v1/artwork/assets/gone.png" })]);
    const img = document.querySelector<HTMLImageElement>("img.cover-img")!;
    fireEvent.error(img);
    expect(document.querySelector("img.cover-img")).toBeNull();
    expect(document.querySelector(".fnm")?.textContent).toBe("Portal 2");
  });

  it("is ONE tab stop that opens the band, with the play control out of the tab order", () => {
    const onOpen = vi.fn();
    const onPlay = vi.fn();
    renderTiles([app("a1", "Portal 2")], { onOpen, onPlay });

    const surface = screen.getByRole("button", { name: "Portal 2" });
    expect(surface).toHaveAttribute("aria-expanded", "false");
    expect(surface.getAttribute("data-app-id")).toBe("a1");
    fireEvent.click(surface);
    expect(onOpen).toHaveBeenCalled();

    const play = screen.getByRole("button", { name: "Play Portal 2" });
    expect(play.tabIndex).toBe(-1);
    fireEvent.click(play);
    expect(onPlay).toHaveBeenCalled();
  });

  it("marks the favourite, the running session and the open state", () => {
    renderTiles([app("a1", "Portal 2", { favourite: true })], { isRunning: true, isOpen: true });

    expect(document.querySelector(".lib-tile .fav-marker")).not.toBeNull();
    expect(document.querySelector(".lib-tile .running")?.textContent).toContain("Running");
    expect(document.querySelector(".lib-tile")?.getAttribute("data-open")).toBe("true");
    expect(screen.getByRole("button", { name: "Portal 2" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    // The live app's own tile resumes rather than launching a second session.
    expect(screen.getByRole("button", { name: "Resume Portal 2" })).toBeEnabled();
  });

  it("disables play on a tile blocked by the live session's home", () => {
    renderTiles([app("a1", "Portal 2")], { isBlocked: true, blockedByName: "Steam" });

    expect(screen.getByRole("button", { name: "Play Portal 2" })).toBeDisabled();
    expect(document.querySelector(".lib-tile .blocked")?.textContent).toContain("In use");
  });

  it("renders the detail band as the tile's next sibling", () => {
    renderTiles([app("a1", "Portal 2")], { detail: <div className="lib-detail" /> });
    const tile = document.querySelector(".lib-tile")!;
    expect(tile.nextElementSibling?.className).toBe("lib-detail");
  });

  it("resolves a surface button back to its tile box", () => {
    renderTiles([app("a1", "Portal 2")]);
    const surface = screen.getByRole("button", { name: "Portal 2" });
    expect(tileBoxOf(surface)).toBe(document.querySelector(".lib-tile"));
    expect(tileBoxOf(null)).toBeNull();
  });
});
