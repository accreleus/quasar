// AppHomeNext — the landing page built from
// design_handoff_v3/screens/home.html.
//
// The assertions here are the ones that would otherwise rot silently:
//   · the rail is SERVER-RANKED — it renders `GET /v1/me/highlights` in the
//     order given, joins each app_id against the catalogue it already holds,
//     and contains no ranker of its own,
//   · the Home ↔ Library view is derived from the ROUTE, and the in-page
//     toggle that used to drive it is gone (AppLayout.test.tsx covers the
//     topbar nav that replaced it),
//   · the filter bar's heading is HIDDEN, NOT DELETED — the visible
//     "Library — 17 apps" line went away, the section's accessible name did
//     not,
//   · the view swap slides, with a direction that follows the nav order, and
//     announces "no animation" on first load,
//   · source grouping buckets by what the API actually returns, including an
//     unknown provider id,
//   · the kind filter, the favourite round-trip,
//   · and the #386 artwork contract (a cold load must not pull the catalogue).

import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Link, MemoryRouter, useLocation } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import type { RailHighlight } from "./homeData";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import { ApiError } from "../../api/client";
import type { App, Session } from "../../api/types";

vi.mock("../../api/library");

const mocked = vi.mocked(libraryApi);

/** `over` is keyed by App fields but deliberately value-loose: one test models
 * an `external_source` OUTSIDE today's `"" | "steam"` enum, which is the whole
 * point of the unknown-provider grouping case. */
function app(id: string, name: string, over: Partial<Record<keyof App, unknown>> = {}): App {
  return {
    id,
    name,
    description: "",
    kind: "game",
    cover_url: `/v1/artwork/assets/${id}.png`,
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

function session(id: string, appId: string, over: Partial<Session> = {}): Session {
  return {
    id,
    app_id: appId,
    user_id: "u1",
    state: "running",
    created_at: new Date(Date.now() - 60_000).toISOString(),
    started_at: new Date(Date.now() - 60_000).toISOString(),
    ended_at: null,
    ...over,
  } as unknown as Session;
}

/** One `GET /v1/me/highlights` item. Every nullable field is present-and-null,
 *  exactly as the contract serializes it (all six are `required`). */
function highlight(
  appId: string,
  reason: string,
  over: Partial<RailHighlight> = {},
): RailHighlight {
  return {
    app_id: appId,
    reason,
    session_id: null,
    session_started_at: null,
    play_seconds: 0,
    last_played_at: null,
    ...over,
  };
}

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

/** Prints the router's current path so a test can assert where a launch or a
 *  resume actually went. */
function LocationProbe() {
  const loc = useLocation();
  return <span data-testid="path">{loc.pathname}</span>;
}

/** Render the page at `path`. The Home ↔ Library view is derived from the URL
 *  (AppHomeNext header), so the route IS the fixture. `over` patches the auth
 *  context — the empty state is role-varied, so `isAdmin` is a fixture input. */
function renderHome(path = "/app", over: Partial<AuthContextValue> = {}, query = "") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={{ ...auth, ...over }}>
        <LibrarySearchContext.Provider value={{ query, setQuery: vi.fn() }}>
          <LibraryCatalogProvider>
            <ToastProvider>
              <AppHomeNext />
              <LocationProbe />
            </ToastProvider>
          </LibraryCatalogProvider>
        </LibrarySearchContext.Provider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const pathNow = () => screen.getByTestId("path").textContent;

/** Focus an element the way a user arrives at it. Wrapped in `act` because the
 *  rail's roving tabindex is React state driven by the card's `onFocus`. */
function focusEl<T extends HTMLElement>(el: T): T {
  act(() => {
    el.focus();
  });
  return el;
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  // An empty rail is a normal 200 — the default for every test that is not
  // about the rail.
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue({
    profiles: [],
    recommended_id: "",
    confidence: "low",
  } as never);
  mocked.favouriteApp.mockResolvedValue(undefined as never);
  mocked.unfavouriteApp.mockResolvedValue(undefined as never);
});

/** Wait until the catalogue has painted. */
async function waitForGrid(n: number) {
  await waitFor(() => {
    expect(document.querySelectorAll(".lib-tile").length).toBe(n);
  });
}

// ── Featured rail ────────────────────────────────────────────────────────────

describe("featured rail", () => {
  it("renders the hero headline and the server's rail, in the server's order", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Hades"), app("a3", "Celeste")],
    } as never);
    mocked.getMySessions.mockResolvedValue({
      items: [session("s1", "a1")],
      next_cursor: null,
    } as never);
    // A DELIBERATELY "wrong-looking" order — the live card last. A client that
    // re-sorted (as the retired buildFeaturedCards did, live-first) would
    // reorder this; rendering it as given is the whole point.
    mocked.getHighlights.mockResolvedValue({
      items: [
        highlight("a3", "recently_added"),
        highlight("a2", "most_played", { play_seconds: 39_600 }),
        highlight("a1", "live", {
          session_id: "s1",
          session_started_at: new Date(Date.now() - 600_000).toISOString(),
        }),
      ],
    } as never);

    renderHome();
    await waitForGrid(3);

    // "Game / on" gradient headline.
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(/Game\s*on/);

    await waitFor(() => {
      expect(document.querySelectorAll(".home-feat")).toHaveLength(3);
    });
    const cards = Array.from(document.querySelectorAll<HTMLElement>(".home-feat"));
    expect(cards.map((c) => c.getAttribute("data-variant"))).toEqual([
      "recently_added",
      "most_played",
      "live",
    ]);
    expect(cards.map((c) => c.querySelector(".home-feat-name")?.textContent)).toEqual([
      "Celeste",
      "Hades",
      "Portal 2",
    ]);
    expect(within(cards[1]).getByText("11 h on your server")).toBeInTheDocument();
    expect(within(cards[2]).getByText("Session live · 10 minutes")).toBeInTheDocument();
    expect(within(cards[2]).getByText("Resume")).toBeInTheDocument();
  });

  // The v3 mock's card hierarchy: why this card is here (kicker), what pressing
  // it does (action, the display line), which game (name, muted, at the foot).
  // FeaturedRail.test.tsx owns the component's own assertions; this one is here
  // because the page is where the wiring could silently swap two lines.
  it("renders the mock's three-line card body", async () => {
    // The filler app is load-bearing: the rail only renders when it is a
    // SELECTION from the catalogue rather than a second copy of it (see the
    // "earns its space" block below). One app plus one highlight is the
    // one-app case, not the rail case.
    mocked.listApps.mockResolvedValue({
      items: [app("a2", "Hades"), app("a9", "Filler")],
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a2", "most_played", { play_seconds: 39_600 })],
    } as never);

    renderHome();
    await waitForGrid(2);

    await waitFor(() => {
      expect(document.querySelector(".home-feat")).not.toBeNull();
    });
    const body = document.querySelector(".home-feat-body") as HTMLElement;
    expect(Array.from(body.children).map((el) => el.className.split(" ")[0])).toEqual([
      "home-feat-kicker",
      "home-feat-action",
      "home-feat-name",
    ]);
    expect(body.querySelector(".home-feat-kicker")?.textContent).toBe("Most played this week");
    expect(body.querySelector(".home-feat-action")?.textContent).toBe("11 h on your server");
    expect(body.querySelector(".home-feat-name")?.textContent).toBe("Hades");
  });

  it("keeps a live card's clock sane when session_started_at is null (pending session)", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    // A `pending`/`assigned` session genuinely has no started_at yet, so the
    // client falls back to its own session row rather than printing NaN.
    mocked.getMySessions.mockResolvedValue({
      items: [
        session("s1", "a1", {
          state: "pending",
          started_at: null,
          created_at: new Date(Date.now() - 120_000).toISOString(),
        }),
      ],
      next_cursor: null,
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "live", { session_id: "s1", session_started_at: null })],
    } as never);

    renderHome();
    await waitForGrid(1);

    await waitFor(() => {
      expect(document.querySelector(".home-feat")).not.toBeNull();
    });
    const kicker = document.querySelector(".home-feat-kicker") as HTMLElement;
    expect(kicker.textContent).toBe("Session live · 2 minutes");
    expect(kicker.textContent).not.toMatch(/NaN/);
  });

  it("is omitted entirely when the server returns an empty rail", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    mocked.getHighlights.mockResolvedValue({ items: [] } as never);

    renderHome();
    await waitForGrid(1);

    expect(document.querySelector(".home-rail")).toBeNull();
    // The hero copy still renders full-width — only the rail is absent.
    expect(screen.getByRole("heading", { level: 1 })).toBeInTheDocument();
    expect(document.querySelector(".home-hero")?.getAttribute("data-rail")).toBe("false");
  });

  it("keeps the library grid rendering when the highlights endpoint fails", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Hades")],
    } as never);
    mocked.getHighlights.mockRejectedValue(new Error("boom") as never);

    renderHome();
    // The grid below is a separate fetch and must be untouched by the failure.
    await waitForGrid(2);
    expect(document.querySelector(".home-rail")).toBeNull();
  });

  it("skips a highlight for an app the client does not hold", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a5", "Filler")],
    } as never);
    // a9's entitlement was revoked between the two fetches.
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a9", "recently_added"), highlight("a1", "recently_added")],
    } as never);

    renderHome();
    await waitForGrid(2);

    await waitFor(() => {
      expect(document.querySelectorAll(".home-feat")).toHaveLength(1);
    });
    expect(
      document.querySelector(".home-feat .home-feat-name")?.textContent,
    ).toBe("Portal 2");
  });

  it("hides both scroll buttons when there is nothing to scroll to", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a9", "Filler")],
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "recently_added")],
    } as never);

    renderHome();
    await waitForGrid(2);
    await waitFor(() => {
      expect(document.querySelector(".home-rail")).not.toBeNull();
    });

    expect(screen.queryByRole("button", { name: /scroll featured left/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /scroll featured right/i })).toBeNull();
  });

  it("gives every rail card a keyboard-operable surface and play control", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a9", "Filler")],
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "recently_added")],
    } as never);

    renderHome();
    await waitForGrid(2);

    await waitFor(() => {
      expect(document.querySelector(".home-feat")).not.toBeNull();
    });
    const card = document.querySelector(".home-feat") as HTMLElement;
    // The whole card is a real <button>, not a div with onclick.
    expect(within(card).getByRole("button", { name: /Portal 2, Newly added\. Show details/ })).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Play Portal 2" })).toBeInTheDocument();
  });

  it("opens the detail band from a rail card", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a9", "Filler")],
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "recently_added")],
    } as never);

    renderHome();
    await waitForGrid(2);

    await waitFor(() => {
      expect(document.querySelector(".home-feat")).not.toBeNull();
    });
    fireEvent.click(
      screen.getByRole("button", { name: /Portal 2, Newly added\. Show details/ }),
    );
    await waitFor(() => {
      expect(document.querySelector(".detail")).not.toBeNull();
    });
  });
});

// ── Home ↔ Library, driven by the ROUTE ──────────────────────────────────────
//
// The in-page segmented toggle is gone: the topbar nav owns the switch and the
// view is a function of the URL. These assert the two surfaces the two routes
// produce; AppLayout.test.tsx asserts that clicking the topbar nav actually
// moves between them.

describe("route-driven view", () => {
  const twoApps = [
    app("a1", "Portal 2", { external_source: "steam" }),
    app("a2", "Weston Terminal", { kind: "desktop" }),
  ];

  it("/app renders the flat Home grid with the hero", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderHome("/app");
    await waitForGrid(2);

    expect(document.querySelectorAll(".lib-grid")).toHaveLength(1);
    expect(document.querySelectorAll(".src-head")).toHaveLength(0);
    expect(screen.getByRole("heading", { level: 1 })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Your library");
    // First load is a cut, not a slide — arriving at /app should not animate.
    expect(document.querySelector(".home-view")?.getAttribute("data-enter")).toBe("none");
  });

  it("/app/library collapses the hero and groups the tiles by source", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderHome("/app/library");
    await waitForGrid(2);

    expect(document.querySelectorAll(".src-head")).toHaveLength(2);
    expect(document.querySelectorAll(".lib-grid")).toHaveLength(2);
    // The hero is UNMOUNTED, not CSS-collapsed — a hidden hero still holds
    // focusable rail cards, which is an invisible keyboard trap.
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Library");
  });

  it("no longer renders an in-page view toggle — the topbar owns it", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderHome("/app");
    await waitForGrid(2);

    expect(screen.queryByRole("tablist", { name: "View" })).toBeNull();
    // The kind filter stays exactly where it was.
    expect(screen.getByRole("tablist", { name: "Filter by kind" })).toBeInTheDocument();
  });
});

// ── The filter bar's heading: hidden, not deleted ────────────────────────────
//
// The v3 head is visible again: title, count, filter, and (Home only) the way
// on to the full library. The heading is also the section's accessible name —
// delete it and a screen-reader user lands in an unlabelled slab of buttons.

describe("library section head", () => {
  const twoApps = [app("a1", "Portal 2"), app("a2", "Hades")];

  it("names the view, counts the apps, and links on to the library from Home only", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    const { unmount } = renderHome("/app");
    await waitForGrid(2);
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Your library");
    expect(screen.getByText("2 apps")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open full library" })).toHaveAttribute(
      "href",
      "/app/library",
    );
    unmount();

    mocked.listApps.mockResolvedValue({ items: twoApps } as never);
    renderHome("/app/library");
    await waitForGrid(2);
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Library");
    // The link would point at the page you are already on.
    expect(screen.queryByRole("link", { name: "Open full library" })).toBeNull();
  });

  it("counts what the filter matched, not the whole catalogue", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Weston", { kind: "desktop" })],
    } as never);

    renderHome("/app");
    await waitForGrid(2);
    expect(screen.getByText("2 apps")).toBeInTheDocument();

    const tabs = screen.getByRole("tablist", { name: "Filter by kind" });
    fireEvent.click(within(tabs).getByRole("tab", { name: "Desktops" }));
    await waitForGrid(1);
    expect(screen.getByText("1 app")).toBeInTheDocument();
  });

  it("keeps the section's accessible name and points the region at it", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderHome("/app");
    await waitForGrid(2);

    const heading = screen.getByRole("heading", { level: 2 });
    expect(heading).toHaveTextContent("Your library");
    // The <section> is a named region, not an anonymous div — the heading is
    // what names it.
    const region = screen.getByRole("region", { name: "Your library" });
    expect(region).toHaveClass("home-lib");
    expect(region.getAttribute("aria-labelledby")).toBe(heading.id);
    // ...and the grid lives inside that named region.
    expect(region.querySelector(".lib-grid")).not.toBeNull();
  });
});

// ── The view SLIDES, and the direction matches the nav order ─────────────────

describe("view transition", () => {
  const twoApps = [
    app("a1", "Portal 2", { external_source: "steam" }),
    app("a2", "Weston Terminal", { kind: "desktop" }),
  ];

  /** AppHomeNext plus two real router links, so a nav click is a real
   *  navigation rather than a re-render with a different prop. */
  function renderWithNav(path = "/app") {
    return render(
      <MemoryRouter initialEntries={[path]}>
        <AuthContext.Provider value={auth}>
          <LibrarySearchContext.Provider value={{ query: "", setQuery: vi.fn() }}>
            <LibraryCatalogProvider>
              <ToastProvider>
                <Link to="/app">to home</Link>
                <Link to="/app/library">to library</Link>
                <AppHomeNext />
              </ToastProvider>
            </LibraryCatalogProvider>
          </LibrarySearchContext.Provider>
        </AuthContext.Provider>
      </MemoryRouter>,
    );
  }

  const dir = () => document.querySelector(".home-view")?.getAttribute("data-enter");

  it("enters from the right going forward and from the left going back", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderWithNav("/app");
    await waitForGrid(2);
    expect(dir()).toBe("none");

    // Home → Library is FORWARD along the topbar nav order.
    fireEvent.click(screen.getByRole("link", { name: "to library" }));
    await waitFor(() => expect(document.querySelectorAll(".src-head").length).toBe(2));
    expect(dir()).toBe("right");

    fireEvent.click(screen.getByRole("link", { name: "to home" }));
    await waitFor(() => expect(document.querySelectorAll(".src-head").length).toBe(0));
    expect(dir()).toBe("left");
  });

  it("remounts the wrapper per navigation so the one-shot animation replays", async () => {
    mocked.listApps.mockResolvedValue({ items: twoApps } as never);

    renderWithNav("/app");
    await waitForGrid(2);
    const first = document.querySelector(".home-view");

    fireEvent.click(screen.getByRole("link", { name: "to library" }));
    await waitFor(() => expect(document.querySelectorAll(".src-head").length).toBe(2));
    // A class swap on a persistent node would not replay a CSS animation; the
    // `key` is what makes this a fresh element.
    expect(document.querySelector(".home-view")).not.toBe(first);
  });
});

// ── Source grouping ──────────────────────────────────────────────────────────

describe("source grouping", () => {
  it("buckets by provider, kind and the unknown/no-source cases, with counts", async () => {
    mocked.listApps.mockResolvedValue({
      items: [
        app("a1", "Portal 2", { external_source: "steam" }),
        app("a2", "Hades", { external_source: "steam" }),
        // A provider id nothing in the codebase knows about: it must degrade
        // to a readable heading, not vanish and not throw.
        app("a3", "Tyrian", { external_source: "gog" }),
        app("a4", "Weston Terminal", { kind: "desktop" }),
        app("a5", "Verification App"),
      ],
    } as never);

    renderHome("/app/library");
    await waitForGrid(5);

    expect(document.querySelectorAll(".src-head")).toHaveLength(4);
    const heads = Array.from(document.querySelectorAll<HTMLElement>(".src-head"));
    // Named providers alphabetically, then Manual, then Desktops.
    expect(heads.map((h) => h.querySelector("h3")?.textContent)).toEqual([
      "Gog",
      "Steam",
      "Manual",
      "Desktops",
    ]);
    expect(heads.map((h) => h.querySelector(".s-sub")?.textContent)).toEqual([
      "via Gog",
      "via Steam",
      "Added to the catalogue directly",
      "Full desktop environments",
    ]);
    expect(heads.map((h) => h.querySelector(".count")?.textContent)).toEqual([
      "1",
      "2",
      "1",
      "1",
    ]);
  });

});

// ── Kind filter ──────────────────────────────────────────────────────────────

describe("kind filter", () => {
  it("narrows the grid", async () => {
    mocked.listApps.mockResolvedValue({
      items: [
        app("a1", "Portal 2"),
        app("a2", "Weston Terminal", { kind: "desktop" }),
        app("a3", "Hades", { favourite: true }),
      ],
    } as never);

    renderHome();
    await waitForGrid(3);

    const kindTabs = screen.getByRole("tablist", { name: "Filter by kind" });
    fireEvent.click(within(kindTabs).getByRole("tab", { name: "Desktops" }));
    await waitForGrid(1);
    expect(screen.getByRole("button", { name: "Weston Terminal" })).toBeInTheDocument();

    fireEvent.click(within(kindTabs).getByRole("tab", { name: "Favourites" }));
    await waitForGrid(1);
    expect(screen.getByRole("button", { name: "Hades" })).toBeInTheDocument();

    fireEvent.click(within(kindTabs).getByRole("tab", { name: "All" }));
    await waitForGrid(3);
  });
});

// ── Reachability (UX assessment §2.5) ────────────────────────────────────────
//
// The rail's play control is `tabIndex={-1}`, exactly like the tile's, so `P`
// is its keyboard route. It used to be gated on the TILE's class name, which
// meant a keyboard user on a rail card watched the play button appear on focus
// and had no way to press it.

describe("the rail's P shortcut", () => {
  const cat = () => [app("a1", "Portal 2"), app("a2", "Hades"), app("a3", "Celeste")];

  /** Focus a rail card's surface button by the app's name. */
  function focusRailCard(name: string) {
    return focusEl(
      screen.getByRole("button", {
        name: new RegExp(`^${name}, .*Show details$`),
      }),
    );
  }

  it("RESUMES a live card — it goes to the session, it does not launch", async () => {
    mocked.listApps.mockResolvedValue({ items: cat() } as never);
    mocked.getMySessions.mockResolvedValue({
      items: [session("s1", "a1")],
      next_cursor: null,
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [
        highlight("a1", "live", {
          session_id: "s1",
          session_started_at: new Date(Date.now() - 600_000).toISOString(),
        }),
      ],
    } as never);

    renderHome();
    await waitForGrid(3);
    await waitFor(() => expect(document.querySelector(".home-feat")).not.toBeNull());

    focusRailCard("Portal 2");
    fireEvent.keyDown(document, { key: "p" });

    await waitFor(() => expect(pathNow()).toBe("/app/session/s1"));
    // THE POINT: a live card must not go down the launch path at all. Doing so
    // is the 409 the single-writer lock exists to produce.
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });

  it("LAUNCHES a card with no session behind it", async () => {
    mocked.listApps.mockResolvedValue({ items: cat() } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a2", "most_played", { play_seconds: 3600 })],
    } as never);
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "p1",
      confidence: "high",
      profiles: [
        {
          id: "p1",
          display_name: "1080p60",
          eligibility: "eligible",
          reasons: [],
          rungs: [{ id: "r1", codec: "h264", eligibility: "eligible", reasons: [] }],
        },
      ],
    } as never);
    mocked.launchSession.mockResolvedValue({
      session: {
        id: "s9",
        stream: { width: 1920, height: 1080, fps: 60, codec: "h264", playout0_ms: 50, mic: true },
      },
      signaling: { url: "ws://x", token: "t" },
    } as never);

    renderHome();
    await waitForGrid(3);
    await waitFor(() => expect(document.querySelector(".home-feat")).not.toBeNull());

    focusRailCard("Hades");
    fireEvent.keyDown(document, { key: "p" });

    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    await waitFor(() => expect(pathNow()).toBe("/app/session/s9"));
  });

  it("does nothing on a card blocked by the live session's home", async () => {
    // a2 and a3 share a1's family, so the single-writer lock blocks them while
    // a1 runs. The shortcut must not be a way around a disabled play button.
    mocked.listApps.mockResolvedValue({
      items: [
        app("a1", "Portal 2"),
        app("a2", "Hades", { parent_app_id: "a1" }),
        app("a3", "Celeste"),
      ],
    } as never);
    mocked.getMySessions.mockResolvedValue({
      items: [session("s1", "a1")],
      next_cursor: null,
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [
        highlight("a1", "live", { session_id: "s1" }),
        highlight("a2", "most_played", { play_seconds: 3600 }),
      ],
    } as never);

    renderHome();
    await waitForGrid(3);
    await waitFor(() => expect(document.querySelectorAll(".home-feat")).toHaveLength(2));

    focusRailCard("Hades");
    fireEvent.keyDown(document, { key: "p" });

    await waitFor(() => expect(pathNow()).toBe("/app"));
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });
});

// ── One roving-focus model over the rail AND the grid (assessment §3.5) ──────
//
// roving.test.ts owns the model's arithmetic. These are the wiring assertions:
// that the page measures the two regions and hands them to it, that a crossing
// actually moves DOM focus, that the grid's own behaviour is untouched, and
// that the rail scrolls its focused card into view — the failure mode the
// assessment names, since a rail card can be focused while off-screen.

describe("roving focus over the rail and the grid", () => {
  const cat = () => [
    app("a1", "Portal 2"),
    app("a2", "Hades"),
    app("a3", "Celeste"),
    app("a4", "Factorio"),
    app("a5", "Stray"),
  ];

  const rails = () =>
    Array.from(document.querySelectorAll<HTMLButtonElement>(".home-feat-surface"));
  const tiles = () =>
    Array.from(document.querySelectorAll<HTMLButtonElement>(".lib-tile-surface"));
  const arrow = (key: string) => fireEvent.keyDown(document, { key });

  /** jsdom has no layout: every rect is 0 and every offsetTop is 0. Give the
   *  elements the geometry the model reads, since the model reads MEASUREMENTS
   *  by design. `centerX` is what a rail↔grid crossing aims with. */
  function place(el: HTMLElement, centerX: number) {
    el.getBoundingClientRect = () =>
      ({ left: centerX - 100, width: 200, top: 0, height: 200, right: centerX + 100, bottom: 200, x: centerX - 100, y: 0, toJSON: () => ({}) }) as DOMRect;
  }
  /** Put a tile in a row — `offsetTop` on the tile BOX, which is what
   *  `tileBoxOf` resolves and what the grid has always measured. */
  function row(surface: HTMLElement, offsetTop: number) {
    const box = surface.closest(".lib-tile") as HTMLElement;
    Object.defineProperty(box, "offsetTop", { value: offsetTop, configurable: true });
  }

  async function renderWithRail(highlights: RailHighlight[], items = cat()) {
    mocked.listApps.mockResolvedValue({ items } as never);
    mocked.getHighlights.mockResolvedValue({ items: highlights } as never);
    renderHome();
    await waitForGrid(items.length);
    await waitFor(() => expect(document.querySelector(".home-feat")).not.toBeNull());
  }

  it("walks the rail with left/right, and stops at its ends", async () => {
    await renderWithRail([
      highlight("a1", "recently_added"),
      highlight("a2", "most_played", { play_seconds: 3600 }),
      highlight("a3", "recently_added"),
    ]);
    const cards = rails();
    expect(cards).toHaveLength(3);

    focusEl(cards[0]);
    arrow("ArrowRight");
    expect(document.activeElement).toBe(cards[1]);
    arrow("ArrowRight");
    expect(document.activeElement).toBe(cards[2]);

    // The end of the rail is an end: it does not spill into the grid sideways.
    arrow("ArrowRight");
    expect(document.activeElement).toBe(cards[2]);

    arrow("ArrowLeft");
    expect(document.activeElement).toBe(cards[1]);
  });

  it("crosses DOWN from a rail card into the grid tile beneath it", async () => {
    await renderWithRail([
      highlight("a1", "recently_added"),
      highlight("a2", "most_played", { play_seconds: 3600 }),
      highlight("a3", "recently_added"),
    ]);
    const cards = rails();
    const grid = tiles();
    // Rail cards at 100/400/700; a 5-wide grid row at 60/220/380/540/700.
    cards.forEach((c, i) => place(c, 100 + i * 300));
    grid.forEach((t, i) => {
      place(t, 60 + i * 160);
      row(t, 0);
    });

    focusEl(cards[2]);
    arrow("ArrowDown");
    // 700 is column 4's centre exactly — the crossing lands under the card,
    // not at column 0.
    expect(document.activeElement).toBe(grid[4]);
  });

  it("crosses UP from the grid's first row back into the rail", async () => {
    await renderWithRail([
      highlight("a1", "recently_added"),
      highlight("a2", "most_played", { play_seconds: 3600 }),
      highlight("a3", "recently_added"),
    ]);
    const cards = rails();
    const grid = tiles();
    cards.forEach((c, i) => place(c, 100 + i * 300));
    // Two rows of 3, so up from the SECOND row must stay in the grid.
    grid.forEach((t, i) => {
      place(t, 60 + (i % 3) * 320);
      row(t, Math.floor(i / 3) * 400);
    });

    focusEl(grid[3]);
    arrow("ArrowUp");
    expect(document.activeElement).toBe(grid[0]);

    arrow("ArrowUp");
    expect(document.activeElement).toBe(cards[0]);
  });

  it("scrolls a focused rail card into view, clear of the chevrons", async () => {
    await renderWithRail([
      highlight("a1", "recently_added"),
      highlight("a2", "most_played", { play_seconds: 3600 }),
      highlight("a3", "recently_added"),
    ]);
    const track = document.querySelector(".home-rail-track") as HTMLElement;
    const scrollTo = vi.fn();
    // jsdom implements neither `scrollTo` nor layout; the page falls back to
    // assigning `scrollLeft` when `scrollTo` is missing, so giving it one here
    // is what makes the target value assertable.
    (track as unknown as { scrollTo: unknown }).scrollTo = scrollTo;
    Object.defineProperty(track, "clientWidth", { value: 600, configurable: true });
    track.scrollLeft = 0;
    track.getBoundingClientRect = () => ({ left: 0, width: 600 }) as DOMRect;

    // Cards 300 apart, 280 wide, measured as RECTS — the surface is absolutely
    // positioned inside its card, so `offsetLeft` is 0 for every one of them
    // and reading it computed "already visible" for the whole rail.
    const cards = rails();
    cards.forEach((c, i) => {
      c.getBoundingClientRect = () => ({ left: i * 300, width: 280 }) as DOMRect;
    });

    focusEl(cards[0]);
    arrow("ArrowRight");
    // Card 1 (300..580) is inside 0..600 but only 20px clear of the edge —
    // under the chevron. Card 2 (600..880) is off-screen entirely.
    arrow("ArrowRight");
    expect(document.activeElement).toBe(cards[2]);
    expect(scrollTo).toHaveBeenCalled();
    const last = scrollTo.mock.calls.at(-1)?.[0] as { left: number; behavior: string };
    // 880 + 44 clearance − 600 of viewport.
    expect(last.left).toBe(324);
    // INSTANT on purpose: Chrome re-snaps a `mandatory` scroller after a SMOOTH
    // programmatic scroll, which measured live as the focused card springing
    // back off-screen. See the comment at the call site.
    expect(last.behavior).toBe("auto");
  });

  it("is ONE tab stop, and the stop follows focus", async () => {
    await renderWithRail([
      highlight("a1", "recently_added"),
      highlight("a2", "most_played", { play_seconds: 3600 }),
      highlight("a3", "recently_added"),
    ]);
    const cards = rails();
    expect(cards.filter((c) => c.tabIndex === 0)).toHaveLength(1);
    expect(cards[0].tabIndex).toBe(0);

    focusEl(cards[0]);
    arrow("ArrowRight");
    await waitFor(() => expect(rails()[1].tabIndex).toBe(0));
    expect(rails().filter((c) => c.tabIndex === 0)).toHaveLength(1);

    // The grid is NOT a composite widget — every tile stays its own tab stop.
    expect(tiles().every((t) => t.tabIndex === 0)).toBe(true);
  });

  it("leaves the grouped Library view's grid roving exactly as it was", async () => {
    // No rail here at all (the hero is unmounted in Library view), and the
    // grid spans two source sections — the case the measured-offsetTop maths
    // exists for. Down from the top row must reach the next section's tile.
    mocked.listApps.mockResolvedValue({
      items: [
        app("a1", "Portal 2", { external_source: "steam" }),
        app("a2", "Hades", { external_source: "steam" }),
        app("a3", "Weston", { kind: "desktop" }),
        app("a4", "XFCE", { kind: "desktop" }),
      ],
    } as never);
    renderHome("/app/library");
    await waitForGrid(4);
    expect(document.querySelector(".home-rail")).toBeNull();

    const grid = tiles();
    grid.forEach((t, i) => {
      place(t, 60 + (i % 2) * 320);
      row(t, Math.floor(i / 2) * 400);
    });

    focusEl(grid[0]);
    arrow("ArrowDown");
    expect(document.activeElement).toBe(grid[2]);
    arrow("ArrowUp");
    expect(document.activeElement).toBe(grid[0]);
    // And with no rail above it, up from the first row is a no-op rather than
    // a crossing into nothing.
    arrow("ArrowUp");
    expect(document.activeElement).toBe(grid[0]);
  });
});

// ── The rail earns its space (§2.7, the one-app case) ────────────────────────

describe("rail suppression", () => {
  it("is not rendered when it would only repeat the whole catalogue", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "most_played", { play_seconds: 3600 })],
    } as never);

    renderHome();
    await waitForGrid(1);
    await waitFor(() => expect(document.querySelector(".home-lib")).not.toBeNull());

    expect(document.querySelector(".home-rail")).toBeNull();
    // …and the hero drops the resume half of its promise with it.
    expect(document.querySelector(".home-hero-sub")?.textContent).toBe(
      "Launch anything from your hosts.",
    );
  });

  it("survives for a LIVE card even then — resuming is the one-action job", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    mocked.getMySessions.mockResolvedValue({
      items: [session("s1", "a1")],
      next_cursor: null,
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [highlight("a1", "live", { session_id: "s1" })],
    } as never);

    renderHome();
    await waitForGrid(1);
    await waitFor(() => expect(document.querySelector(".home-rail")).not.toBeNull());
  });

  it("offers only the kind segments that can match something, and none at all for one app", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    renderHome();
    await waitForGrid(1);
    expect(screen.queryByRole("tablist", { name: "Filter by kind" })).toBeNull();

    // Two apps, no favourites and no launchers → All / Games / Desktops only.
    cleanup();
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Weston", { kind: "desktop" })],
    } as never);
    renderHome();
    await waitForGrid(2);
    const tabs = screen.getByRole("tablist", { name: "Filter by kind" });
    expect(within(tabs).getAllByRole("tab").map((t) => t.textContent)).toEqual([
      "All",
      "Games",
      "Desktops",
    ]);
  });
});

// ── Bare states (§2.7) ───────────────────────────────────────────────────────

describe("catalogue failure", () => {
  it("explains itself and re-runs the fetch on Try again", async () => {
    mocked.listApps.mockRejectedValueOnce(
      new ApiError(500, "internal", "catalogue unavailable"),
    );

    renderHome();

    const panel = await screen.findByRole("alert");
    expect(panel).toHaveTextContent("Your library didn't load");
    // The server's own words survive — vague is worse than blunt.
    expect(panel).toHaveTextContent("catalogue unavailable");
    // Nothing on screen pretends the page is healthy.
    expect(document.querySelector(".home-hero")).toBeNull();
    expect(screen.queryByRole("tablist", { name: "Filter by kind" })).toBeNull();

    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));

    await waitForGrid(1);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("empty catalogue", () => {
  it("tells an admin how to fix it and gives them the route", async () => {
    mocked.listApps.mockResolvedValue({ items: [] } as never);

    renderHome("/app", { isAdmin: true });
    await screen.findByText("Your library is empty");

    expect(
      screen.getByText(/Add one in the admin area and it shows up here\./),
    ).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Add apps" }));
    await waitFor(() => expect(pathNow()).toBe("/admin/library/apps"));
  });

  it("tells a plain user what to ask for, and offers no admin route", async () => {
    mocked.listApps.mockResolvedValue({ items: [] } as never);

    renderHome("/app", { isAdmin: false });
    await screen.findByText("Your library is empty");

    expect(
      screen.getByText(/Ask an admin for the ones you want to play\./),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add apps" })).toBeNull();
    // The hero's "launch anything from your hosts" is not printed over nothing.
    expect(document.querySelector(".home-hero")).toBeNull();
  });

  it("offers a way out of a filter that matched nothing", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Weston", { kind: "desktop" })],
    } as never);

    renderHome();
    await waitForGrid(2);

    const tabs = screen.getByRole("tablist", { name: "Filter by kind" });
    fireEvent.click(within(tabs).getByRole("tab", { name: "Desktops" }));
    await waitForGrid(1);

    // Search narrows it to nothing on top of the kind filter.
    cleanup();
    renderHome("/app", {}, "zzz");
    await waitFor(() => expect(document.querySelector(".home-state")).not.toBeNull());
    expect(screen.getByText("Nothing matches")).toBeInTheDocument();
    expect(screen.getByText(/No app matches “zzz” in this filter\./)).toBeInTheDocument();
  });
});

// ── Favourites ───────────────────────────────────────────────────────────────

describe("favourite toggle", () => {
  it("round-trips through the API and shows the tile marker", async () => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);

    renderHome();
    await waitForGrid(1);
    expect(document.querySelector(".fav-marker")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Portal 2" }));
    await waitFor(() => {
      expect(document.querySelector(".detail")).not.toBeNull();
    });

    fireEvent.click(screen.getByRole("button", { name: "Add Portal 2 to favourites" }));
    await waitFor(() => {
      expect(mocked.favouriteApp).toHaveBeenCalledWith("tok", "a1");
    });
    // Optimistic: the tile marker appears without a refetch.
    await waitFor(() => {
      expect(document.querySelector(".fav-marker")).not.toBeNull();
    });

    fireEvent.click(screen.getByRole("button", { name: "Remove Portal 2 from favourites" }));
    await waitFor(() => {
      expect(mocked.unfavouriteApp).toHaveBeenCalledWith("tok", "a1");
    });
  });
});

// ── #386 artwork contract ────────────────────────────────────────────────────

describe("artwork loading (#386)", () => {
  it("keeps every grid tile lazy so a cold load does not pull the catalogue", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Hades"), app("a3", "Celeste")],
    } as never);

    renderHome();
    await waitForGrid(3);

    const imgs = Array.from(
      document.querySelectorAll<HTMLImageElement>(".lib-grid img.cover-img"),
    );
    expect(imgs).toHaveLength(3);
    for (const img of imgs) {
      expect(img.getAttribute("loading")).toBe("lazy");
      expect(img.getAttribute("decoding")).toBe("async");
    }
  });

  it("makes only the FIRST rail poster eager+high-priority — it is the LCP element", async () => {
    mocked.listApps.mockResolvedValue({
      items: [
        app("a1", "Portal 2"),
        app("a2", "Hades"),
        app("a3", "Celeste"),
        app("a9", "Filler"),
      ],
    } as never);
    mocked.getHighlights.mockResolvedValue({
      items: [
        highlight("a1", "recently_added"),
        highlight("a2", "recently_added"),
        highlight("a3", "recently_added"),
      ],
    } as never);

    renderHome();
    await waitForGrid(4);

    await waitFor(() => {
      expect(document.querySelectorAll(".home-feat img.cover-img")).toHaveLength(3);
    });
    const rail = Array.from(
      document.querySelectorAll<HTMLImageElement>(".home-feat img.cover-img"),
    );
    expect(rail).toHaveLength(3);
    expect(rail[0].getAttribute("loading")).toBeNull();
    expect(rail[0].getAttribute("fetchpriority")).toBe("high");
    for (const img of rail.slice(1)) {
      expect(img.getAttribute("loading")).toBe("lazy");
      expect(img.getAttribute("fetchpriority")).toBeNull();
    }
  });
});
