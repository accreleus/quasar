// A tile's play RESUMES when that app owns the live session (UX assessment
// §3.3, Plex's resume-vs-start-over decision).
//
// Before this, a tile's play always called `quickLaunch`, which the server
// answers with `409 home_in_use` when the user's own session is the thing in
// the way — only the rail card offered a one-press resume. The three
// properties worth pinning are all failure-shaped:
//
//   · the LIVE app's tile resumes (navigates) and never calls the launch API,
//   · a SIBLING tile — same home, different app, marked "In use" by the Steam
//     library-discovery rules — must NOT resume into a session that is not its
//     app, and must not launch either,
//   · the control SAYS WHICH IT WILL DO. A play glyph that sometimes launches
//     and sometimes resumes, with no way to tell, is worse than the 409.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import type { App, Session } from "../../api/types";

vi.mock("../../api/library");

const mocked = vi.mocked(libraryApi);

function app(id: string, name: string, over: Partial<Record<keyof App, unknown>> = {}): App {
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

function session(id: string, appId: string): Session {
  return {
    id,
    app_id: appId,
    user_id: "u1",
    state: "running",
    created_at: new Date(Date.now() - 60_000).toISOString(),
    started_at: new Date(Date.now() - 60_000).toISOString(),
    ended_at: null,
  } as unknown as Session;
}

/** A response whose recommendation is outright eligible — otherwise
 *  `quickLaunch` diverts to the warning band instead of launching, and the
 *  "a normal tile still launches" control case proves nothing. */
const ELIGIBLE_PROFILES = {
  recommended_id: "1080p60",
  confidence: "high",
  notes: [],
  profiles: [
    {
      id: "1080p60",
      display_name: "1080p 60",
      description: "",
      nominal: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 8000 },
      eligibility: "eligible",
      reasons: [],
      rungs: [
        {
          id: "1080p60-h264",
          display_name: "1080p60-h264",
          codec: "h264",
          width: 1920,
          height: 1080,
          fps: 60,
          nominal_bitrate_kbps: 8000,
          position: 1,
          eligibility: "eligible",
          reasons: [],
        },
      ],
    },
  ],
};

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function LocationProbe() {
  const loc = useLocation();
  return <span data-testid="path">{loc.pathname}</span>;
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/app"]}>
      <AuthContext.Provider value={auth}>
        <LibrarySearchContext.Provider value={{ query: "", setQuery: vi.fn() }}>
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

/** THE FAMILY. `a2` and `a3` are Steam-derived tiles that BORROW `steam`'s
 *  home, so a live session on `a2` blocks `steam` and `a3` (they would 409
 *  `home_in_use`) while `solo` is unrelated and freely launchable. */
function familyCatalogue(): App[] {
  return [
    app("steam", "Steam", { kind: "launcher" }),
    app("a2", "Hades", { parent_app_id: "steam" }),
    app("a3", "Celeste", { parent_app_id: "steam" }),
    app("solo", "Portal 2"),
  ];
}

function tileFor(name: string): HTMLElement {
  const surface = screen.getByRole("button", { name });
  const tile = surface.closest(".lib-tile");
  if (!tile) throw new Error(`no .lib-tile for ${name}`);
  return tile as HTMLElement;
}

/** The layered play control inside a tile (tabIndex -1, so it is found by
 *  class rather than by tab order). */
function playControlIn(tile: HTMLElement): HTMLButtonElement {
  const btn = tile.querySelector<HTMLButtonElement>(".lib-tile-play");
  if (!btn) throw new Error("tile has no play control");
  return btn;
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listApps.mockResolvedValue({ items: familyCatalogue() } as never);
  mocked.getMySessions.mockResolvedValue({ items: [session("s9", "a2")], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue(ELIGIBLE_PROFILES as never);
  mocked.launchSession.mockResolvedValue({
    session: {
      id: "new-session",
      app_id: "solo",
      stream: { width: 1920, height: 1080, fps: 60, codec: "h264", playout0_ms: 50, mic: true },
    },
    signaling: { url: "ws://x", token: "t" },
  } as never);
});

async function waitForGrid(n: number) {
  await waitFor(() => {
    expect(document.querySelectorAll(".lib-tile").length).toBe(n);
  });
}

describe("tile play: resume vs launch", () => {
  it("resumes the LIVE app's own tile — navigates to the session, never launches", async () => {
    renderPage();
    await waitForGrid(4);

    const play = playControlIn(tileFor("Hades"));
    expect(play).toHaveAttribute("data-action", "resume");
    expect(play).toBeEnabled();

    fireEvent.click(play);

    await waitFor(() => expect(pathNow()).toBe("/app/session/s9"));
    expect(mocked.launchSession).not.toHaveBeenCalled();
    // It must not even ASK for profiles — resuming is not a launch decision.
    expect(mocked.getProfiles).not.toHaveBeenCalled();
  });

  it("says which it will do — the resume control is labelled, the launch control is not", async () => {
    renderPage();
    await waitForGrid(4);

    const live = playControlIn(tileFor("Hades"));
    expect(live).toHaveAccessibleName("Resume Hades");
    expect(live.textContent).toContain("Resume");

    const idle = playControlIn(tileFor("Portal 2"));
    expect(idle).toHaveAccessibleName("Play Portal 2");
    expect(idle).toHaveAttribute("data-action", "launch");
    expect(idle.textContent).not.toContain("Resume");
  });

  it("still launches an unrelated tile", async () => {
    renderPage();
    await waitForGrid(4);

    fireEvent.click(playControlIn(tileFor("Portal 2")));

    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalledTimes(1));
    expect(mocked.launchSession.mock.calls[0][1]).toMatchObject({ app_id: "solo" });
    await waitFor(() => expect(pathNow()).toBe("/app/session/new-session"));
  });

  it("a SIBLING of the live app stays 'In use' and neither resumes nor launches", async () => {
    renderPage();
    await waitForGrid(4);

    for (const name of ["Celeste", "Steam"]) {
      const tile = tileFor(name);
      // The Steam-library-discovery presentation is untouched…
      expect(tile.querySelector(".blocked")?.textContent).toContain("In use");
      const play = playControlIn(tile);
      // …and the control is still a LAUNCH control that happens to be disabled,
      // not a resume into someone else's session.
      expect(play).toHaveAttribute("data-action", "launch");
      expect(play).toHaveAccessibleName(`Play ${name}`);
      expect(play).toBeDisabled();

      fireEvent.click(play);
    }

    expect(pathNow()).toBe("/app");
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });

  it("the live app's detail band offers Resume session, not Play", async () => {
    renderPage();
    await waitForGrid(4);

    fireEvent.click(screen.getByRole("button", { name: "Hades" }));
    await waitFor(() => expect(document.querySelector(".detail")).toBeTruthy());

    const resume = await screen.findByRole("button", { name: /Resume session/ });
    expect(screen.queryByRole("button", { name: /^Play$/ })).toBeNull();
    // It explains why the launch settings beside it are not what this button
    // does, rather than leaving two contradictory affordances side by side.
    expect(document.querySelector(".detail")?.textContent).toMatch(
      /You're playing .*right now/,
    );

    fireEvent.click(resume);
    await waitFor(() => expect(pathNow()).toBe("/app/session/s9"));
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });
});
