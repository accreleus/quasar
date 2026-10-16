// Home under the new design vs the Classic layout switch: the same tile opens
// the game's page or the band, and the shelves, hearts and `F` key exist only
// in the new design.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import type { App } from "../../api/types";

vi.mock("../../api/library");
const design = vi.hoisted(() => ({ on: true }));
vi.mock("../../settings/OverlayPreferencesContext", async (orig) => ({
  ...(await orig<typeof import("../../settings/OverlayPreferencesContext")>()),
  useNewDesign: () => design.on,
}));
vi.mock("../../webrtc/capability", () => ({
  probeCodecs: () => ({ h264: true, hevc: true, av1: true }),
}));
vi.mock("./game/gameData", async (orig) => ({
  ...(await orig<typeof import("./game/gameData")>()),
  useGameExtras: () => ({
    a1: { play: { last_played_at: "2026-01-01T00:00:00Z", total_seconds: 7200, sessions: 3, last_session: null } },
    a3: { play: { last_played_at: "2026-03-01T00:00:00Z", total_seconds: 1800, sessions: 1, last_session: null } },
  }),
}));

const mocked = vi.mocked(libraryApi);

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
    default_profile_id: null,
    profile_policy: "inherit",
    favourite: false,
    enabled: true,
    ...over,
  } as unknown as App;
}

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function Probe() {
  const loc = useLocation();
  return <span data-testid="path">{loc.pathname}</span>;
}

function renderHome(state?: unknown) {
  return render(
    <MemoryRouter initialEntries={[{ pathname: "/app", state }]}>
      <AuthContext.Provider value={auth}>
        <LibrarySearchContext.Provider value={{ query: "", setQuery: vi.fn() }}>
          <LibraryCatalogProvider>
            <ToastProvider>
              <AppHomeNext />
              <Probe />
            </ToastProvider>
          </LibraryCatalogProvider>
        </LibrarySearchContext.Provider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const grid = () => document.querySelector(".lib-grid:not([class*='shelf'])") as HTMLElement;

beforeEach(() => {
  cleanup();
  design.on = true;
  vi.resetAllMocks();
  mocked.listApps.mockResolvedValue({
    items: [app("a1", "Portal 2"), app("a2", "Hades", { favourite: true }), app("a3", "Celeste")],
  } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue({ profiles: [], recommended_id: "", confidence: "low" } as never);
  mocked.favouriteApp.mockResolvedValue(undefined as never);
  mocked.unfavouriteApp.mockResolvedValue(undefined as never);
});

async function waitForGrid() {
  await waitFor(() => expect(grid().querySelectorAll(".lib-tile").length).toBe(3));
}

describe("Home, new design", () => {
  it("opens the game's page from a grid tile and never opens the band", async () => {
    renderHome();
    await waitForGrid();
    const surface = within(grid()).getByRole("button", { name: "Portal 2" });
    expect(surface.hasAttribute("aria-expanded")).toBe(false);
    fireEvent.click(surface);
    expect(screen.getByTestId("path").textContent).toBe("/app/game/a1");
    expect(document.querySelector(".detail")).toBeNull();
  });

  it("gives every tile a resting heart and a title, and toggles on the heart and on F", async () => {
    renderHome();
    await waitForGrid();
    expect(grid().querySelectorAll(".lib-tile.tile-titled")).toHaveLength(3);
    expect(grid().querySelector(".fav-marker")).toBeNull();
    fireEvent.click(within(grid()).getByRole("button", { name: "Add Portal 2 to favourites" }));
    await waitFor(() => expect(mocked.favouriteApp).toHaveBeenCalledWith("tok", "a1"));

    const celeste = within(grid()).getByRole("button", { name: "Celeste" });
    act(() => celeste.focus());
    fireEvent.keyDown(celeste, { key: "f" });
    await waitFor(() => expect(mocked.favouriteApp).toHaveBeenCalledWith("tok", "a3"));
  });

  it("shows Favourites and Jump back in shelves, most recent first", async () => {
    renderHome();
    await waitForGrid();
    const fav = screen.getByRole("region", { name: "Favourites" });
    expect(within(fav).getAllByRole("button", { name: /Hades/ })).not.toHaveLength(0);
    const jump = screen.getByRole("region", { name: "Jump back in" });
    const names = Array.from(jump.querySelectorAll(".lib-tile-surface")).map((b) => b.getAttribute("aria-label"));
    expect(names).toEqual(["Celeste", "Portal 2"]);
    expect(jump.textContent).toContain("30 min played");
    fireEvent.click(within(jump).getByRole("button", { name: "Celeste" }));
    expect(screen.getByTestId("path").textContent).toBe("/app/game/a3");
  });

  it("leaves Ctrl/Cmd/Alt+F and a held F to the browser", async () => {
    renderHome();
    await waitForGrid();
    const celeste = within(grid()).getByRole("button", { name: "Celeste" });
    expect(celeste.getAttribute("aria-keyshortcuts")).toBe("F");
    act(() => celeste.focus());
    for (const mod of [{ ctrlKey: true }, { metaKey: true }, { altKey: true }, { repeat: true }]) {
      expect(fireEvent.keyDown(celeste, { key: "f", ...mod })).toBe(true); // not default-prevented
    }
    expect(fireEvent.keyDown(celeste, { key: "p", ctrlKey: true })).toBe(true);
    expect(mocked.favouriteApp).not.toHaveBeenCalled();
    expect(mocked.getProfiles).not.toHaveBeenCalled();
  });

  it("ignores F on a shelf tile, which shows no heart", async () => {
    renderHome();
    await waitForGrid();
    const shelfTile = within(screen.getByRole("region", { name: "Favourites" })).getByRole("button", { name: "Hades" });
    act(() => shelfTile.focus());
    fireEvent.keyDown(shelfTile, { key: "f" });
    expect(mocked.unfavouriteApp).not.toHaveBeenCalled();
  });

  it("keeps focus in the grid when F drops a tile from the Favourites filter", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2", { favourite: true }), app("a2", "Hades", { favourite: true }), app("a3", "Celeste")],
    } as never);
    renderHome();
    await waitForGrid();
    fireEvent.click(screen.getByRole("tab", { name: "Favourites" }));
    await waitFor(() => expect(grid().querySelectorAll(".lib-tile")).toHaveLength(2));
    const hades = within(grid()).getByRole("button", { name: "Hades" });
    act(() => hades.focus());
    fireEvent.keyDown(hades, { key: "f" });
    await waitFor(() => expect(mocked.unfavouriteApp).toHaveBeenCalledWith("tok", "a2"));
    await waitFor(() => expect(document.activeElement?.getAttribute("aria-label")).toBe("Portal 2"));
  });

  it("opens the game's page, not the band, where one-click Play cannot launch", async () => {
    renderHome();
    await waitForGrid();
    fireEvent.click(within(grid()).getByRole("button", { name: "Play Portal 2" }));
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/app/game/a1"));
    expect(document.querySelector(".detail")).toBeNull();
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });

  it("gives a Classic redirect back to the game page when the switch has flipped", async () => {
    renderHome({ openApp: "a3" });
    await waitFor(() => expect(screen.getByTestId("path").textContent).toBe("/app/game/a3"));
    expect(document.querySelector(".detail")).toBeNull();
  });
});

describe("Home, Classic layout", () => {
  beforeEach(() => {
    design.on = false;
  });

  it("keeps the band, the favourite marker and no shelves", async () => {
    renderHome();
    await waitForGrid();
    expect(document.querySelector(".tile-titled, .tile-heart")).toBeNull();
    expect(screen.queryByRole("region", { name: "Favourites" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Jump back in" })).toBeNull();
    expect(grid().querySelectorAll(".fav-marker")).toHaveLength(1);
    const surface = within(grid()).getByRole("button", { name: "Portal 2" });
    expect(surface.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(surface);
    await waitFor(() => expect(document.querySelector(".detail")).not.toBeNull());
    expect(screen.getByTestId("path").textContent).toBe("/app");
  });

  it("ignores F on a tile", async () => {
    renderHome();
    await waitForGrid();
    const celeste = within(grid()).getByRole("button", { name: "Celeste" });
    act(() => celeste.focus());
    fireEvent.keyDown(celeste, { key: "f" });
    expect(mocked.favouriteApp).not.toHaveBeenCalled();
  });

  it("opens the band for a game-page URL's redirect, once", async () => {
    renderHome({ openApp: "a3" });
    await waitFor(() => expect(document.querySelector(".detail")).not.toBeNull());
    expect(document.querySelector(".detail")?.textContent).toContain("Celeste");
  });
});
