// A derived tile's Play for a user with no Steam home gets `409 home_not_provisioned`.
// Every Play surface must say why and what to do, and none may retry it as a wait.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import { GamePageRoute } from "./game/GamePage";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import { ApiError } from "../../api/client";
import type { App } from "../../api/types";

vi.mock("../../api/library");
const design = vi.hoisted(() => ({ on: false }));
vi.mock("../../settings/OverlayPreferencesContext", async (orig) => ({
  ...(await orig<typeof import("../../settings/OverlayPreferencesContext")>()),
  useNewDesign: () => design.on,
}));
vi.mock("../../webrtc/capability", () => ({ probeCodecs: () => ({ h264: true, hevc: true, av1: true }) }));

const mocked = vi.mocked(libraryApi);

const app = (id: string, name: string, over: Record<string, unknown> = {}) =>
  ({
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
  }) as unknown as App;

const catalogue = [app("steam", "Steam", { kind: "launcher" }), app("hades", "Hades", { parent_app_id: "steam" })];

const rung = {
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
};
const profiles = {
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
      rungs: [rung],
    },
  ],
};

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

function Where() {
  return <span data-testid="where">{useLocation().pathname}</span>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={auth}>
        <LibrarySearchContext.Provider value={{ query: "", setQuery: vi.fn() }}>
          <LibraryCatalogProvider>
            <ToastProvider>
              <Routes>
                <Route path="/app" element={<AppHomeNext />} />
                <Route path="/app/game/:id" element={<GamePageRoute />} />
              </Routes>
              <Where />
            </ToastProvider>
          </LibraryCatalogProvider>
        </LibrarySearchContext.Provider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const where = () => screen.getByTestId("where").textContent;

/** The toast says what to do and offers the Steam app; the launch was asked once. */
async function expectGuidance(classic = false) {
  expect(await screen.findByText("Start Steam once first")).toBeTruthy();
  expect(screen.getByText(/Start Steam once, then press Play again/)).toBeTruthy();
  expect(screen.queryByText(/Waiting for/)).toBeNull();
  expect(mocked.launchSession).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole("button", { name: "Open Steam" }));
  // Classic has no game page: /app/game/:id bounces to /app and opens that app's detail band.
  if (classic) await waitFor(() => expect(document.querySelector(".detail")?.textContent).toContain("Steam"));
  else await waitFor(() => expect(where()).toBe("/app/game/steam"));
}

beforeEach(() => {
  cleanup();
  vi.resetAllMocks();
  vi.stubGlobal("fetch", vi.fn());
  mocked.listApps.mockResolvedValue({ items: catalogue } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue(profiles as never);
  mocked.getProfilePreferences.mockResolvedValue({
    default_profile_id: null,
    global_default_profile_id: null,
    user_overrides_allowed: true,
  } as never);
  mocked.launchSession.mockRejectedValue(new ApiError(409, "home_not_provisioned", "launch Steam once on a host"));
});

afterEach(() => vi.unstubAllGlobals());

describe("home_not_provisioned on Play", () => {
  it("Classic tile Play", async () => {
    design.on = false;
    renderAt("/app");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(2));
    const tile = screen.getByRole("button", { name: "Hades" }).closest(".lib-tile") as HTMLElement;
    fireEvent.click(tile.querySelector(".lib-tile-play") as HTMLButtonElement);
    await expectGuidance(true);
  });

  it("Classic detail band Play", async () => {
    design.on = false;
    renderAt("/app");
    await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(2));
    fireEvent.click(screen.getByRole("button", { name: "Hades" }));
    await waitFor(() => expect(document.querySelector(".detail")).toBeTruthy());
    const play = await screen.findByRole("button", { name: /^Play$/ });
    await waitFor(() => expect(play).toBeEnabled());
    fireEvent.click(play);
    await expectGuidance(true);
  });

  it("game page Play", async () => {
    design.on = true;
    renderAt("/app/game/hades");
    const play = await screen.findByRole("button", { name: /^Play$/ });
    await waitFor(() => expect(play).toBeEnabled());
    fireEvent.click(play);
    await expectGuidance();
  });
});
