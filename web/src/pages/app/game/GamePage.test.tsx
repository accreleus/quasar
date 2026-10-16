// The game page route: the new design renders the page; the Classic layout
// hands the URL's game to Home's band.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { GamePageRoute } from "./GamePage";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { LibraryCatalogProvider } from "../libraryCatalog";
import { ToastProvider } from "../../../components/Toast";
import * as libraryApi from "../../../api/library";
import type { App } from "../../../api/types";

vi.mock("../../../api/library");
const design = vi.hoisted(() => ({ on: true }));
vi.mock("../../../settings/OverlayPreferencesContext", async (orig) => ({
  ...(await orig<typeof import("../../../settings/OverlayPreferencesContext")>()),
  useNewDesign: () => design.on,
}));
vi.mock("../../../webrtc/capability", () => ({
  probeCodecs: () => ({ h264: true, hevc: true, av1: true }),
}));
vi.mock("./gameData", async (orig) => ({
  ...(await orig<typeof import("./gameData")>()),
  useGameExtras: () => ({}),
  getGameSettings: () => Promise.resolve(null),
  putGameSettings: () => Promise.resolve({}),
  clearGameSettings: () => Promise.resolve(),
}));

const mocked = vi.mocked(libraryApi);

const game = {
  id: "a1",
  name: "Portal 2",
  description: "A puzzle game.",
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
} as unknown as App;

const rung = {
  id: "av1-1440",
  display_name: "1440p60",
  codec: "av1",
  width: 2560,
  height: 1440,
  fps: 60,
  nominal_bitrate_kbps: 12000,
  position: 1,
  eligibility: "eligible",
  reasons: [],
};
const profiles = {
  recommended_id: "p-1440",
  confidence: "high",
  notes: [],
  profiles: [
    {
      id: "p-1440",
      display_name: "p-1440",
      description: "",
      nominal: { width: 2560, height: 1440, fps: 60, bitrate_kbps: 12000 },
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

function Home() {
  const loc = useLocation();
  return <span data-testid="home">{JSON.stringify(loc.state)}</span>;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthContext.Provider value={auth}>
        <LibraryCatalogProvider>
          <ToastProvider>
            <Routes>
              <Route path="/app" element={<Home />} />
              <Route path="/app/game/:id" element={<GamePageRoute />} />
              <Route path="/app/library" element={<span>library</span>} />
            </Routes>
          </ToastProvider>
        </LibraryCatalogProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  cleanup();
  design.on = true;
  vi.resetAllMocks();
  mocked.listApps.mockResolvedValue({ items: [game] } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getProfiles.mockResolvedValue(profiles as never);
  mocked.favouriteApp.mockResolvedValue(undefined as never);
});

describe("game page", () => {
  it("shows the game, what Play will use, and Play", async () => {
    renderAt("/app/game/a1");
    expect(await screen.findByRole("heading", { level: 1, name: "Portal 2" })).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("button", { name: "Play" }).hasAttribute("disabled")).toBe(false));
    expect(document.querySelector(".gp-stream-line")?.textContent).toContain("1440p");
    expect(document.querySelector(".gp-stream-line")?.textContent).toContain("chosen automatically");
    expect(screen.getByText("A puzzle game.")).toBeTruthy();
  });

  it("favourites from the hero", async () => {
    renderAt("/app/game/a1");
    fireEvent.click(await screen.findByRole("button", { name: "Add Portal 2 to favourites" }));
    await waitFor(() => expect(mocked.favouriteApp).toHaveBeenCalledWith("tok", "a1"));
    expect(screen.getByRole("button", { name: "Remove Portal 2 from favourites" })).toBeTruthy();
  });

  it("says so for a game that is not in the library", async () => {
    renderAt("/app/game/nope");
    expect(await screen.findByText("This game is not in your library")).toBeTruthy();
  });

  it("under Classic layout hands the game to Home's band", async () => {
    design.on = false;
    renderAt("/app/game/a1");
    await waitFor(() => expect(screen.getByTestId("home").textContent).toBe('{"openApp":"a1"}'));
  });
});
