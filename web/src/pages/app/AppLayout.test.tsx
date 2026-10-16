// The /app topbar information architecture (2026-08-05).
//
// These mount the REAL route table (App.tsx), not a local copy of it, so a
// route that gets deleted or renamed fails here rather than passing against a
// fixture that no longer resembles the app.
//
// What would otherwise rot silently:
//   · the topbar nav is Home | Library — screens/home.html's <nav class="nav">
//     — and NOT Library | Storage,
//   · both entries are real links: clicking Library moves the route and the
//     route swaps the surface,
//   · the active pill is exact-match, so /app does not stay lit on
//     /app/library,
//   · the search box appears on BOTH grid routes,
//   · /app/storage still resolves — it redirects to /app/account/storage,
//     which is now a real page in the account rail.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { App } from "../../App";
import { buildUserNav, SEARCHABLE_ROUTES } from "./AppLayout";
import { buildUserTabs } from "./userTabs";
import { ConsoleShell } from "../../components/shell/ConsoleShell";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { SetupStatusProvider } from "../../setup/useSetupStatus";
import { ThemeProvider } from "../../settings/ThemeContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import * as storageApi from "../../api/storage";
import * as setupApi from "../../api/setup";

vi.mock("../../api/library");
vi.mock("../../api/storage");
// App.tsx wraps every route in RequireClaimedInstance, which reads this —
// an already-claimed, fully-set-up instance is the case these /app tests
// care about, so the bootstrap gate must never redirect them to /setup.
vi.mock("../../api/setup", () => ({
  getSetupStatus: vi.fn(),
  completeSetup: vi.fn(),
}));

const mockedLibrary = vi.mocked(libraryApi);
const mockedStorage = vi.mocked(storageApi);
const mockedSetup = vi.mocked(setupApi);

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "u@example.com", username: "tester", role: "user" },
  token: "t",
  isAdmin: false,
  sessionExpired: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function Path() {
  return <span data-testid="path">{useLocation().pathname}</span>;
}

function renderApp(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ThemeProvider>
        <AuthContext.Provider value={auth}>
          <SetupStatusProvider>
            <ToastProvider>
              <App />
              <Path />
            </ToastProvider>
          </SetupStatusProvider>
        </AuthContext.Provider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.resetAllMocks();
  mockedSetup.getSetupStatus.mockResolvedValue({ admin_exists: true, setup_completed: true });
  localStorage.clear();
  // These pin the shipped layout; the redesign has its own tests.
  localStorage.setItem("quasar.ui.classic.u1", "1");
  document.body.removeAttribute("data-rail");
  mockedLibrary.listApps.mockResolvedValue({
    items: [
      {
        id: "a1",
        name: "Portal 2",
        description: "",
        kind: "game",
        cover_url: null,
        hero_url: null,
        parent_app_id: null,
        external_source: "steam",
        external_id: "",
        favourite: false,
        enabled: true,
      },
    ],
  } as never);
  mockedLibrary.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mockedLibrary.getHighlights.mockResolvedValue({ items: [] } as never);
  mockedLibrary.getProfilePreferences.mockResolvedValue({ user_overrides_allowed: true } as never);
  mockedStorage.getMyStorage.mockResolvedValue({ items: [] } as never);
});

// ── The topbar nav ────────────────────────────────────────────────────────────

describe("buildUserNav", () => {
  it("gives the two primary entries from screens/home.html", () => {
    expect(buildUserNav()).toEqual([
      { to: "/app", label: "Home" },
      { to: "/app/library", label: "Library" },
    ]);
  });

  it("never offers Storage from the topbar — it lives in the account rail now", () => {
    expect(buildUserNav().some((i) => i.to.includes("storage"))).toBe(false);
  });

  it("only routes that render a grid are searchable", () => {
    expect([...SEARCHABLE_ROUTES]).toEqual(["/app", "/app/library"]);
  });
});

// ── The rendered topbar ──────────────────────────────────────────────────────
//
// Every link query below is SCOPED to a nav, because the same two destinations
// now exist twice in the DOM: once in the topbar pill row and once in the
// small-screen tab bar. Both are always mounted — which of the two is visible is
// a pure media query (`.tabbar` / `.app--tabbar .topbar .nav`, components.css),
// and jsdom applies no stylesheet, so an unscoped getByRole("link") would match
// two elements and throw.

/** The wide-viewport pill row. */
function topbarNav() {
  return screen.getByRole("navigation", { name: "Primary navigation" });
}
/** The <=820px bottom tab bar, once the route has settled. */
function tabBarWhenReady() {
  return screen.findByRole("navigation", { name: "Primary navigation (small screens)" });
}

describe("topbar nav", () => {
  it("renders Home | Library and lights the active one by EXACT match", async () => {
    renderApp("/app");
    const nav = await screen.findByRole("navigation", { name: "Primary navigation" });

    const home = within(nav).getByRole("link", { name: "Home" });
    const library = within(nav).getByRole("link", { name: "Library" });
    expect(within(nav).queryByRole("link", { name: "Storage" })).toBeNull();

    expect(home).toHaveClass("active");
    expect(library).not.toHaveClass("active");
  });

  it("moving to /app/library swaps the surface and the active pill", async () => {
    renderApp("/app");
    await waitFor(() => {
      expect(document.querySelectorAll(".lib-tile").length).toBe(1);
    });
    // Home: hero present, one flat grid.
    expect(screen.getByRole("heading", { level: 1 })).toBeInTheDocument();
    expect(document.querySelectorAll(".src-head")).toHaveLength(0);

    fireEvent.click(within(topbarNav()).getByRole("link", { name: "Library" }));

    await waitFor(() => {
      expect(document.querySelectorAll(".src-head").length).toBe(1);
    });
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    expect(within(topbarNav()).getByRole("link", { name: "Library" })).toHaveClass("active");
    expect(within(topbarNav()).getByRole("link", { name: "Home" })).not.toHaveClass("active");
  });

  it("renders the grouped Library view on a cold load of /app/library", async () => {
    renderApp("/app/library");
    await waitFor(() => {
      expect(document.querySelectorAll(".src-head").length).toBe(1);
    });
    expect(within(topbarNav()).getByRole("link", { name: "Library" })).toHaveClass("active");
  });
});

// ── Small-screen navigation (UX assessment §2.3/§2.8/§3.4) ───────────────────
//
// The defect: below 820px the pill row was `display: none` with NO replacement,
// so Home and Library were unreachable and unlabelled and /app/library needed a
// typed URL. These lock in the replacement's presence, its derivation from the
// one nav definition, and — the compounding half — that the ACCOUNT shell
// carries it too.

describe("buildUserTabs", () => {
  it("derives from buildUserNav and appends Account as the third area", () => {
    expect(buildUserTabs().map((t) => [t.to, t.label, t.end])).toEqual([
      ["/app", "Home", true],
      ["/app/library", "Library", true],
      ["/app/account", "Account", false],
    ]);
  });

  it("matches the primary entries EXACTLY — /app is a prefix of every route", () => {
    for (const tab of buildUserTabs()) {
      expect(tab.end).toBe(tab.to !== "/app/account");
    }
  });

  it("gives every tab an icon — a bar of bare words is not a tab bar", () => {
    for (const tab of buildUserTabs()) expect(tab.icon).toBeTruthy();
  });

  it("picks the icon from the LABEL, not the route", () => {
    const library = buildUserTabs()[1];
    const home = buildUserTabs()[0];
    expect(library.icon).not.toEqual(home.icon);
  });
});

describe("small-screen tab bar", () => {
  it("renders in the LIBRARY shell with both primary destinations", async () => {
    renderApp("/app");
    const bar = await screen.findByRole("navigation", {
      name: "Primary navigation (small screens)",
    });
    expect(within(bar).getByRole("link", { name: "Home" })).toHaveClass("active");
    expect(within(bar).getByRole("link", { name: "Library" })).toBeInTheDocument();
    expect(within(bar).getByRole("link", { name: "Account" })).toBeInTheDocument();
  });

  it("renders in the ACCOUNT shell too — the two shells now agree", async () => {
    // §2.8: /app/account uses AppShell's `sidebar` mode, which drops the pill
    // nav at every width. Before this the ONLY route back to the library from
    // Account was the brand mark.
    renderApp("/app/account");
    const bar = await screen.findByRole("navigation", {
      name: "Primary navigation (small screens)",
    });
    expect(within(bar).getByRole("link", { name: "Library" })).toBeInTheDocument();
    // Account stays lit on a SUB-route (prefix match), or the bar goes blank in
    // a third of the app.
    expect(within(bar).getByRole("link", { name: "Account" })).toHaveClass("active");
    // The account shell genuinely has no pill row to fall back on.
    expect(
      screen.queryByRole("navigation", { name: "Primary navigation" }),
    ).toBeNull();
  });

  it("stays lit on Account for a rail sub-route", async () => {
    renderApp("/app/account/storage");
    const bar = await tabBarWhenReady();
    expect(within(bar).getByRole("link", { name: "Account" })).toHaveClass("active");
    expect(within(bar).getByRole("link", { name: "Library" })).not.toHaveClass("active");
  });

  it("gives the ACCOUNT shell a wide-screen route back to the library too", async () => {
    // The tab bar is CSS-hidden above 820px, so the desktop half of §2.8 is the
    // user menu — which had no Library entry at all.
    renderApp("/app/account");
    fireEvent.click(await screen.findByRole("button", { name: /tester/ }));
    expect(screen.getByRole("menuitem", { name: "Game library" })).toHaveAttribute(
      "href",
      "/app",
    );
  });

  it("is NOT rendered on the library shell's user menu (it is already there)", async () => {
    renderApp("/app");
    fireEvent.click(await screen.findByRole("button", { name: /tester/ }));
    expect(screen.queryByRole("menuitem", { name: "Game library" })).toBeNull();
  });

  it("is OPT-IN — a console shell that did not ask for it renders none", () => {
    // This is what keeps the bar off /admin (eight subject areas: a rail
    // problem, not a three-slot one) and off the full-screen session view.
    // `/app/session/:id` is registered outside both user-area layouts
    // (App.tsx), so it mounts no shell at all — but even one that did would
    // have to ask for the bar explicitly.
    render(
      <MemoryRouter initialEntries={["/admin"]}>
        <ThemeProvider>
          <AuthContext.Provider value={auth}>
            <ConsoleShell sections={[]} railLabel="Admin sections">
              <p>content</p>
            </ConsoleShell>
          </AuthContext.Provider>
        </ThemeProvider>
      </MemoryRouter>,
    );
    expect(document.querySelector(".tabbar")).toBeNull();
    expect(document.querySelector(".app")).not.toBeNull();
  });

  it("mounts both expressions of the nav, so the CSS swap has two halves", async () => {
    renderApp("/app");
    await waitFor(() => expect(document.querySelector(".tabbar")).not.toBeNull());
    // `.topbar.home .nav { display: none }` below 820px is the other half of
    // the swap (shell.css); above it the tab bar is the hidden one. Exactly
    // one paints, both are always mounted, and neither can drift from the
    // other because both derive from buildUserNav.
    expect(document.querySelector(".topbar.home .nav")).not.toBeNull();
    expect(document.querySelector(".app-home > .tabbar")).not.toBeNull();
  });
});

// ── Topbar search ────────────────────────────────────────────────────────────

describe("topbar search box", () => {
  it("appears on /app", async () => {
    renderApp("/app");
    expect(await screen.findByRole("textbox", { name: "Search library" })).toBeInTheDocument();
  });

  it("appears on /app/library — that view has a grid to filter", async () => {
    renderApp("/app/library");
    expect(await screen.findByRole("textbox", { name: "Search library" })).toBeInTheDocument();
  });

  it("is seeded from ?q= so a palette link (or a bookmark) arrives filtered", async () => {
    renderApp("/app/library?q=Portal");
    const box = await screen.findByRole("textbox", { name: "Search library" });
    expect(box).toHaveValue("Portal");
    await waitFor(() => {
      expect(document.querySelectorAll(".lib-tile").length).toBe(1);
    });

    // Typing wins from there — the URL is a seed, not a controller.
    fireEvent.change(box, { target: { value: "zzz" } });
    expect(box).toHaveValue("zzz");
    await waitFor(() => {
      expect(document.querySelectorAll(".lib-tile").length).toBe(0);
    });
  });

  it("a palette hit routes into the library with the name in the box", async () => {
    renderApp("/app");
    await screen.findByRole("textbox", { name: "Search library" });
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    const palette = within(await screen.findByRole("dialog")).getByRole("combobox");
    fireEvent.change(palette, { target: { value: "Portal" } });
    fireEvent.click(await screen.findByRole("option", { name: /Portal 2/ }));
    await waitFor(() => expect(screen.getByRole("textbox", { name: "Search library" })).toHaveValue("Portal 2"));
    expect(screen.getByTestId("path")).toHaveTextContent("/app/library");
  });
});

// ── The same four, in the new design (the default) ───────────────────────────
//
// The box is a combobox there (a results popover under it), so these query that
// role; what they pin is the same shell behaviour.

describe("new design (no Classic layout pin)", () => {
  beforeEach(() => {
    localStorage.removeItem("quasar.ui.classic.u1");
  });
  const box = () => screen.findByRole("combobox", { name: "Search library" });

  it("the search box appears on /app and on /app/library", async () => {
    const { unmount } = renderApp("/app");
    expect(await box()).toBeInTheDocument();
    unmount();
    renderApp("/app/library");
    expect(await box()).toBeInTheDocument();
  });

  it("is seeded from ?q= and typing then filters", async () => {
    renderApp("/app/library?q=Portal");
    const input = await box();
    expect(input).toHaveValue("Portal");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(1));
    fireEvent.change(input, { target: { value: "zzz" } });
    expect(input).toHaveValue("zzz");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(0));
  });

  it("a palette hit opens the game's page", async () => {
    renderApp("/app");
    await box();
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    const palette = within(await screen.findByRole("dialog")).getByRole("combobox");
    fireEvent.change(palette, { target: { value: "Portal" } });
    fireEvent.click(await screen.findByRole("option", { name: /Portal 2/ }));
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/app/game/"));
    expect(await screen.findByRole("heading", { level: 1, name: "Portal 2" })).toBeInTheDocument();
  });

  it("a top-bar result opens the game's page, and Back returns to the library", async () => {
    renderApp("/app/library");
    fireEvent.change(await box(), { target: { value: "Portal" } });
    fireEvent.click(await screen.findByRole("option", { name: "Portal 2" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Portal 2" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Back" }));
    await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent("/app/library"));
  });

  describe("a pick hands focus to Play, so P launches rather than typing into the search", () => {
    beforeEach(() => {
      const rung = { id: "r", display_name: "r", codec: "h264", width: 1920, height: 1080, fps: 60, nominal_bitrate_kbps: 8000, position: 1, eligibility: "eligible", reasons: [] };
      mockedLibrary.getProfiles.mockResolvedValue({
        recommended_id: "p",
        confidence: "high",
        notes: [],
        profiles: [{ id: "p", display_name: "p", description: "", nominal: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 8000 }, eligibility: "eligible", reasons: [], rungs: [rung] }],
      } as never);
      mockedLibrary.launchSession.mockRejectedValue(new Error("not here"));
    });
    async function playHasFocusAndPLaunches() {
      const play = await screen.findByRole("button", { name: "Play" });
      await waitFor(() => expect(document.activeElement).toBe(play));
      const input = await box();
      expect(input).toHaveAttribute("aria-expanded", "false");
      fireEvent.keyDown(play, { key: "p" });
      await waitFor(() => expect(mockedLibrary.launchSession).toHaveBeenCalledTimes(1));
      expect(mockedLibrary.launchSession.mock.calls[0][1]).toEqual({ app_id: "a1", mic: true });
      expect(input).toHaveValue("Portal");
    }

    it("from the top-bar search", async () => {
      renderApp("/app/library");
      const input = await box();
      act(() => input.focus());
      fireEvent.change(input, { target: { value: "Portal" } });
      fireEvent.keyDown(input, { key: "Enter" });
      await playHasFocusAndPLaunches();
    });

    it("from the palette, which hands focus back to the search field it was opened from", async () => {
      renderApp("/app/library?q=Portal");
      const input = await box();
      act(() => input.focus());
      fireEvent.keyDown(input, { key: "k", ctrlKey: true });
      const dialog = await screen.findByRole("dialog");
      fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "Portal" } });
      fireEvent.click(await within(dialog).findByRole("option", { name: /Portal 2/ }));
      await playHasFocusAndPLaunches();
    });

    it("again for the game already open, from the top-bar search and from the palette", async () => {
      renderApp("/app/library");
      const input = await box();
      act(() => input.focus());
      fireEvent.change(input, { target: { value: "Portal" } });
      fireEvent.keyDown(input, { key: "Enter" });
      const play = await screen.findByRole("button", { name: "Play" });
      await waitFor(() => expect(document.activeElement).toBe(play));

      act(() => input.focus());
      fireEvent.keyDown(input, { key: "Enter" });
      await waitFor(() => expect(document.activeElement).toBe(play));
      expect(input).toHaveAttribute("aria-expanded", "false");

      act(() => input.focus());
      fireEvent.keyDown(input, { key: "k", ctrlKey: true });
      const dialog = await screen.findByRole("dialog");
      fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: "Portal" } });
      fireEvent.click(await within(dialog).findByRole("option", { name: /Portal 2/ }));
      await playHasFocusAndPLaunches();
    });
  });

  describe("a pick always opens the chosen game's page", () => {
    const game = (id: string, name: string) => ({ ...mockedAppBase, id, name });
    const mockedAppBase = {
      description: "",
      kind: "game",
      cover_url: null,
      hero_url: null,
      parent_app_id: null,
      external_source: "steam",
      external_id: "",
      favourite: false,
      enabled: true,
    };
    async function pick(name: string, option = 0) {
      fireEvent.keyDown(document, { key: "k", ctrlKey: true });
      const dialog = await screen.findByRole("dialog");
      fireEvent.change(within(dialog).getByRole("combobox"), { target: { value: name } });
      fireEvent.click((await within(dialog).findAllByRole("option", { name: new RegExp(name) }))[option]);
    }
    const page = async (id: string, name: string) => {
      await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent(`/app/game/${id}`));
      expect(await screen.findByRole("heading", { level: 1, name })).toBeInTheDocument();
    };
    const nav = (name: string) => within(screen.getAllByRole("navigation")[0]).getByRole("link", { name });
    const twoGames = () =>
      mockedLibrary.listApps.mockResolvedValue({ items: [game("a1", "Portal 2"), game("a2", "Celeste")] } as never);

    it("again after going Home, from its own page, and for a different game", async () => {
      twoGames();
      renderApp("/app");
      await box();
      await pick("Portal 2");
      await page("a1", "Portal 2");
      fireEvent.click(nav("Home"));
      await waitFor(() => expect(screen.getByTestId("path")).toHaveTextContent(/^\/app$/));
      await pick("Portal 2");
      await page("a1", "Portal 2");
      await pick("Celeste");
      await page("a2", "Celeste");
      await pick("Celeste");
      await page("a2", "Celeste");
    });

    it("when the kind filter hides it", async () => {
      mockedLibrary.listApps.mockResolvedValue({
        items: [game("a1", "Portal 2"), { ...game("a2", "Weston Terminal"), kind: "desktop" }],
      } as never);
      renderApp("/app/library");
      await box();
      const tabs = await screen.findByRole("tablist", { name: "Filter by kind" });
      fireEvent.click(within(tabs).getByRole("tab", { name: "Desktops" }));
      await pick("Portal 2");
      await page("a1", "Portal 2");
    });

    it("two games with the same name: the chosen id opens", async () => {
      mockedLibrary.listApps.mockResolvedValue({ items: [game("a1", "Twin"), game("a2", "Twin")] } as never);
      renderApp("/app/library");
      await box();
      await pick("Twin", 1);
      await page("a2", "Twin");
    });
  });
});

// ── The moved Storage route ──────────────────────────────────────────────────

describe("/app/storage", () => {
  it("redirects into the account area rather than 404ing", async () => {
    renderApp("/app/storage");
    // The account shell's Usage section, reached through the account rail.
    expect(await screen.findByRole("tab", { name: "Storage" })).toBeInTheDocument();
    expect(screen.queryByText("Page not found")).toBeNull();
    // The account rail replaced the topbar pill nav, and it carries one row per
    // section — Storage is the Usage row's first tab, not a rail row.
    const rail = screen.getByRole("navigation", { name: "Account sections" });
    expect(rail).toContainElement(screen.getByRole("link", { name: "Usage" }));
    expect(screen.getByRole("link", { name: "Usage" })).toHaveClass("active");
    expect(screen.getByRole("tab", { name: "Storage" })).toHaveClass("active");
  });
});

// ── The command palette's sources ────────────────────────────────────────────
//
// The palette lives in the shell, above the routed page, so its app list has to
// come from the layout. These mount the real route table, so a layout that
// stops supplying one would leave the palette answering "No matches" here.

describe("command palette sources", () => {
  async function openPalette() {
    fireEvent.keyDown(document, { key: "k", ctrlKey: true });
    return await screen.findByRole("combobox");
  }

  it("searches the catalogue on /app and routes a hit into the library", async () => {
    renderApp("/app");
    await screen.findByLabelText("Search library");

    const box = await openPalette();
    expect(box).toHaveAttribute("placeholder", "Search games or jump to a page");
    fireEvent.change(box, { target: { value: "Portal" } });

    fireEvent.click(await screen.findByRole("option", { name: /Portal 2/ }));
    await waitFor(() =>
      expect(screen.getByLabelText("Search library")).toHaveValue("Portal 2"),
    );
  });

  it("carries the same catalogue into the account shell, and promises nothing else", async () => {
    renderApp("/app/account/profile");
    await screen.findByRole("navigation", { name: "Account sections" });

    const box = await openPalette();
    expect(box).toHaveAttribute("placeholder", "Search games or jump to a page");
    fireEvent.change(box, { target: { value: "Portal" } });
    expect(await screen.findByRole("option", { name: /Portal 2/ })).toBeInTheDocument();
  });
});
