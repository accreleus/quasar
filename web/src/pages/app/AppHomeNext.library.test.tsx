// The redesigned library section (letter sections, the A–Z index, the Genre and
// Sort menus, ranked search) and the Classic layout switch that turns it off.
// The play history and genres come from a source that is null in the shipped
// build, so the tests that need them mock it.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import type { App } from "../../api/types";
import type { ExtrasMap } from "./library/libraryExtras";

vi.mock("../../api/library");
const design = vi.hoisted(() => ({ isNew: true }));
vi.mock("../../settings/OverlayPreferencesContext", () => ({ useNewDesign: () => design.isNew }));
const extrasSource = vi.hoisted(() => ({ value: null as unknown }));
vi.mock("./library/libraryExtras", async (orig) => ({
  ...(await orig<typeof import("./library/libraryExtras")>()),
  useLibraryExtras: () => extrasSource.value,
}));

const mocked = vi.mocked(libraryApi);

const NAMES = [..."ABCDEFGHIJ"].flatMap((l) => Array.from({ length: 7 }, (_, i) => `${l}game ${i}`));
const apps = NAMES.map(
  (name, i) =>
    ({
      id: `a${i}`,
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
    }) as unknown as App,
);

// 20 "Action" titles, a few with play history.
const extras: ExtrasMap = Object.fromEntries(
  apps.map((a, i) => [
    a.id,
    {
      genres: i < 20 ? ["Action"] : ["Indie"],
      added_at: "2026-01-01T00:00:00Z",
      play: i === 3 ? { last_played_at: "2026-10-01T00:00:00Z", total_seconds: 7200 } : null,
    },
  ]),
);

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

function renderLibrary(query = "") {
  return render(
    <MemoryRouter initialEntries={["/app/library"]}>
      <AuthContext.Provider value={auth}>
        <LibrarySearchContext.Provider value={{ query, setQuery: vi.fn() }}>
          <LibraryCatalogProvider>
            <ToastProvider>
              <AppHomeNext />
            </ToastProvider>
          </LibraryCatalogProvider>
        </LibrarySearchContext.Provider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const sections = () => document.querySelectorAll("[data-letter-section]");
async function ready() {
  await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBeGreaterThan(0));
}

beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
  design.isNew = true;
  extrasSource.value = null;
  mocked.listApps.mockResolvedValue({ items: apps } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue({ profiles: [], recommended_id: "", confidence: "low" } as never);
});

describe("library, new design", () => {
  it("splits a long A–Z list into letter sections with an index that lists every letter", async () => {
    renderLibrary();
    await ready();
    expect(sections()).toHaveLength(10);
    const index = screen.getByRole("navigation", { name: "Jump to letter" });
    expect(within(index).getAllByRole("button")).toHaveLength(27);
    expect(within(index).getByRole("button", { name: "K" })).toBeDisabled();
    expect(within(index).getByRole("button", { name: "C" })).toBeEnabled();
  });

  it("offers no Genre or Sort menu without the data behind them", async () => {
    renderLibrary();
    await ready();
    expect(screen.queryByRole("button", { name: /^Genre/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Sort/ })).toBeNull();
  });

  it("keeps a short result a plain grid with no index", async () => {
    renderLibrary("game 3");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(10));
    expect(sections()).toHaveLength(0);
    expect(screen.queryByRole("navigation", { name: "Jump to letter" })).toBeNull();
  });

  it("finds a misspelled title", async () => {
    renderLibrary("bgame 3");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(1));
  });
});

describe("library, new design with play history and genres", () => {
  beforeEach(() => {
    extrasSource.value = extras;
  });

  it("filters by genre through the menu and clears it from the chip", async () => {
    renderLibrary();
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Genre" }));
    fireEvent.click(screen.getByRole("option", { name: /Action/ }));
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(20));
    fireEvent.click(screen.getByRole("button", { name: "Clear genre: Action" }));
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(70));
  });

  it("re-sorting drops the letter sections, adds captions, and closes an open band", async () => {
    renderLibrary();
    await ready();
    fireEvent.click(screen.getAllByRole("button", { name: "Agame 0" })[0]);
    await waitFor(() => expect(document.querySelector(".detail")).not.toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Sort: A–Z" }));
    fireEvent.click(screen.getByRole("option", { name: "Most played" }));
    await waitFor(() => expect(document.querySelector(".detail")).toBeNull());
    expect(sections()).toHaveLength(0);
    // The one played title leads, with its hours.
    expect(document.querySelector(".lib-tile-surface")).toHaveAttribute("aria-label", "Agame 3");
    expect(document.querySelector(".lib-tile-meta")?.textContent).toMatch(/^2 h · /);
  });
});

describe("library, Classic layout", () => {
  beforeEach(() => {
    design.isNew = false;
    extrasSource.value = extras;
  });

  it("renders the one flat grid, with none of the new controls", async () => {
    renderLibrary();
    await ready();
    expect(sections()).toHaveLength(0);
    expect(document.querySelector(".lib-index, .lib-menus, .lib-tile-meta")).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Jump to letter" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Sort/ })).toBeNull();
  });

  it("filters by substring, not by typo", async () => {
    renderLibrary("bgame 3");
    await waitFor(() => expect(screen.queryAllByRole("button", { name: /game/ })).toHaveLength(0));
  });
});
