// The game page route: the new design renders the page; the Classic layout
// hands the URL's game to Home's band. gameData is the real module, as a real
// install has it: no extras, no per-game settings, and no request for either.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { GamePageRoute } from "./GamePage";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { LibraryCatalogProvider } from "../libraryCatalog";
import { ToastProvider } from "../../../components/Toast";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App } from "../../../api/types";

vi.mock("../../../api/library");
const design = vi.hoisted(() => ({ on: true }));
vi.mock("../../../settings/OverlayPreferencesContext", async (orig) => ({
  ...(await orig<typeof import("../../../settings/OverlayPreferencesContext")>()),
  useNewDesign: () => design.on,
}));
const caps = vi.hoisted(() => ({ h264: true, hevc: true, av1: true }));
vi.mock("../../../webrtc/capability", () => ({ probeCodecs: () => ({ ...caps }) }));

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

type Reason = { code: string; message: string };
const rung = (id: string, codec: string, height: number, fps: number, kbps: number, position = 1, eligibility = "eligible", reasons: Reason[] = []) => ({
  id,
  display_name: id,
  codec,
  width: Math.round((height * 16) / 9),
  height,
  fps,
  nominal_bitrate_kbps: kbps,
  position,
  eligibility,
  reasons,
});
const launchProfile = (id: string, rungs: ReturnType<typeof rung>[], eligibility = "eligible", reasons: Reason[] = []) => ({
  id,
  display_name: id,
  description: "",
  nominal: { width: rungs[0].width, height: rungs[0].height, fps: rungs[0].fps, bitrate_kbps: rungs[0].nominal_bitrate_kbps },
  eligibility,
  reasons,
  rungs,
});
const response = (recommended: string, ...profiles: ReturnType<typeof launchProfile>[]) => ({
  recommended_id: recommended,
  confidence: "high",
  notes: [],
  profiles,
});

/** The recommendation (1440p60, AV1 then H.264) and one lower profile. */
const profiles = response(
  "p-1440",
  launchProfile("p-1440", [rung("1440-av1", "av1", 1440, 60, 12000, 1), rung("1440-h264", "h264", 1440, 60, 20000, 2)]),
  launchProfile("p-720", [rung("720-h264", "h264", 720, 60, 5000)]),
);

const launched = {
  session: { id: "s9", stream: { width: 2560, height: 1440, fps: 60, codec: "h264", playout0_ms: 50, mic: true } },
  signaling: { url: "wss://signal.test", token: "t" },
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
  const loc = useLocation();
  return <span data-testid="where">{loc.pathname}</span>;
}

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
              <Route path="/app/session/:id" element={<span>session</span>} />
            </Routes>
            <Where />
          </ToastProvider>
        </LibraryCatalogProvider>
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

const playButton = () => screen.getByRole("button", { name: /^(Play|Resume session)$/ });
const line = () => document.querySelector(".gp-stream-line")?.textContent ?? "";
const sent = (i = 0) => mocked.launchSession.mock.calls[i][1];

/** Render the page and wait until Play is live. */
async function ready(path = "/app/game/a1") {
  renderAt(path);
  await waitFor(() => expect(playButton()).toBeEnabled());
  // The selection seeds one flush after the profiles land.
  await act(async () => {});
}

async function openChange() {
  fireEvent.click(screen.getByRole("button", { name: /Change/ }));
  await waitFor(() => expect(screen.getByRole("dialog", { name: "Launch options for Portal 2" })).toBeTruthy());
  return screen.getByRole("dialog", { name: "Launch options for Portal 2" });
}

const fetchSpy = vi.fn();

beforeEach(() => {
  cleanup();
  design.on = true;
  Object.assign(caps, { h264: true, hevc: true, av1: true });
  vi.resetAllMocks();
  vi.stubGlobal("fetch", fetchSpy);
  mocked.listApps.mockResolvedValue({ items: [game] } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getProfiles.mockResolvedValue(profiles as never);
  mocked.favouriteApp.mockResolvedValue(undefined as never);
  mocked.launchSession.mockResolvedValue(launched as never);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("game page", () => {
  it("shows the game and Play, with a stream line that promises nothing Play does not send", async () => {
    await ready();
    expect(screen.getByRole("heading", { level: 1, name: "Portal 2" })).toBeVisible();
    expect(line()).toBe("Default settings (chosen by the server at launch)");
    expect(screen.getByText("A puzzle game.")).toBeTruthy();
  });

  it("asks nothing of endpoints the control plane does not have", async () => {
    await ready();
    expect(fetchSpy).not.toHaveBeenCalled();
    // Nothing to show under About, history or related titles without them.
    expect(screen.queryByRole("heading", { name: "Your history" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "More like this" })).toBeNull();
  });

  it("hides About when the catalogue has no description", async () => {
    mocked.listApps.mockResolvedValue({ items: [{ ...game, description: "" }] } as never);
    await ready();
    expect(screen.queryByRole("heading", { name: "About" })).toBeNull();
  });

  it("Play sends what Home's one-click Play sends: no profile, no codec", async () => {
    await ready();
    fireEvent.click(playButton());
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({ app_id: "a1", mic: true });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/app/session/s9"));
  });

  it("sends one launch for a double click", async () => {
    await ready();
    const play = playButton();
    // One batch, so the second click still sees `launching` false.
    act(() => {
      play.click();
      play.click();
    });
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    await act(async () => {});
    expect(mocked.launchSession).toHaveBeenCalledTimes(1);
  });

  it("with overrides off, Play still launches and a Change the server refuses says so", async () => {
    mocked.launchSession.mockImplementation((async (_t: string, req: libraryApi.LaunchRequest) => {
      if (req.profile_id) throw new ApiError(409, "conflict", "profile overrides are disabled for this launch");
      return launched;
    }) as never);
    await ready();
    const sheet = await openChange();
    fireEvent.click(within(sheet).getByRole("radio", { name: /720p/ }));
    fireEvent.click(within(sheet).getByRole("button", { name: "Play now" }));
    expect(await screen.findByText("profile overrides are disabled for this launch")).toBeTruthy();
    expect(sent()).toEqual({ app_id: "a1", mic: true, profile_id: "p-720" });

    // The page now shows, and sends, the choice; nothing on it claims a codec.
    await waitFor(() => expect(playButton()).toBeEnabled());
    expect(line()).toBe("720p · 60 fps · 5 Mbps (codec chosen by the server)");
  });

  it("a Change with a codec sends the profile and the codec, as the band does", async () => {
    mocked.launchSession.mockRejectedValue(new ApiError(503, "unavailable", "try later"));
    await ready();
    const sheet = await openChange();
    fireEvent.click(within(sheet).getByRole("radio", { name: /HEVC|H\.264/ }));
    const codec = within(within(sheet).getByRole("radiogroup", { name: "Codec" }))
      .getAllByRole("radio")
      .find((r) => r.getAttribute("aria-checked") === "true")!;
    fireEvent.click(within(sheet).getByRole("button", { name: "Play now" }));
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    const wire = codec.textContent?.includes("HEVC") ? "h265" : "h264";
    expect(sent()).toMatchObject({ app_id: "a1", profile_id: expect.any(String), stream: { codec: wire } });
    await waitFor(() => expect(playButton()).toBeEnabled());
    expect(line()).toContain(wire === "h265" ? "HEVC" : "H.264");
    expect(line()).not.toContain("chosen by the server");
  });

  it("a risky recommendation is launched as the band launches it, and the line names it", async () => {
    mocked.getProfiles.mockResolvedValue(
      response(
        "p-1440",
        launchProfile("p-1440", [rung("1440-h264", "h264", 1440, 60, 20000)], "risky", [
          { code: "bandwidth_too_low", message: "" },
        ]),
      ) as never,
    );
    await ready();
    expect(line()).toBe("1440p · 60 fps · 20 Mbps (codec chosen by the server)");
    fireEvent.click(playButton());
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    expect(sent()).toEqual({ app_id: "a1", mic: true, profile_id: "p-1440" });
  });

  it("a forced app launches on its pin: no profile by default, the pin from Change", async () => {
    mocked.listApps.mockResolvedValue({ items: [{ ...game, profile_policy: "force", default_profile_id: "p-720" }] } as never);
    mocked.launchSession.mockRejectedValue(new ApiError(503, "unavailable", "try later"));
    await ready();
    fireEvent.click(playButton());
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalledTimes(1));
    expect(sent(0)).toEqual({ app_id: "a1", mic: true });

    await waitFor(() => expect(playButton()).toBeEnabled());
    const sheet = await openChange();
    expect(sheet.textContent).toContain("Fixed by this app, it always launches at its own setting.");
    fireEvent.click(within(sheet).getByRole("button", { name: "Play now" }));
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalledTimes(2));
    expect(sent(1)).toEqual({ app_id: "a1", mic: true, profile_id: "p-720" });
  });

  it("resumes the live session instead of launching", async () => {
    mocked.getMySessions.mockResolvedValue({ items: [{ id: "s1", app_id: "a1", state: "running" }], next_cursor: null } as never);
    renderAt("/app/game/a1");
    fireEvent.click(await screen.findByRole("button", { name: "Resume session" }));
    expect(screen.getByTestId("where").textContent).toBe("/app/session/s1");
    expect(mocked.launchSession).not.toHaveBeenCalled();
  });

  it("explains a browser without H.264 and offers no Play (#605)", async () => {
    caps.h264 = false;
    mocked.getProfiles.mockResolvedValue(response("p", launchProfile("p", [rung("p-h264", "h264", 1080, 60, 8000)])) as never);
    renderAt("/app/game/a1");
    expect(await screen.findByText(/Without H\.264 this device cannot play any stream/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Play" })).toBeDisabled();
  });

  it("says why nothing can stream, and re-checks", async () => {
    const tooTall = { code: "decode_height_too_low", message: "client decode height is below the profile resolution" };
    mocked.getProfiles.mockResolvedValue(
      response("p", launchProfile("p", [rung("p-h264", "h264", 2160, 60, 25000, 1, "ineligible", [tooTall])], "ineligible", [tooTall])) as never,
    );
    renderAt("/app/game/a1");
    expect(await screen.findByText(/Nothing here can stream to this device/)).toBeTruthy();
    expect(document.querySelector(".lib-why")?.textContent).toMatch(/can't decode a picture this large/i);
    expect(screen.getByRole("button", { name: "Play" })).toBeDisabled();
    mocked.getProfiles.mockClear();
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(mocked.getProfiles).toHaveBeenCalled());
  });

  it("offers Retry when the profiles fail, and Play once they load", async () => {
    mocked.getProfiles.mockRejectedValueOnce(new ApiError(500, "internal", "boom"));
    renderAt("/app/game/a1");
    expect(await screen.findByText("Could not load stream profiles.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Play" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(playButton()).toBeEnabled());
  });

  it("P plays (not with a modifier or a held key), Escape closes Change", async () => {
    mocked.launchSession.mockRejectedValue(new ApiError(503, "unavailable", "try later"));
    await ready();
    fireEvent.keyDown(window, { key: "p", ctrlKey: true });
    fireEvent.keyDown(window, { key: "p", repeat: true });
    expect(mocked.launchSession).not.toHaveBeenCalled();
    fireEvent.keyDown(window, { key: "p" });
    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalledTimes(1));
    expect(sent()).toEqual({ app_id: "a1", mic: true });

    await waitFor(() => expect(playButton()).toBeEnabled());
    await openChange();
    fireEvent.keyDown(window, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("favourites from the hero", async () => {
    renderAt("/app/game/a1");
    fireEvent.click(await screen.findByRole("button", { name: "Add Portal 2 to favourites" }));
    await waitFor(() => expect(mocked.favouriteApp).toHaveBeenCalledWith("tok", "a1"));
    expect(screen.getByRole("button", { name: "Remove Portal 2 from favourites" })).toBeTruthy();
  });
});

describe("game page deep links", () => {
  it("shows loading, not a missing game, until the catalogue lands", async () => {
    let resolve!: (v: unknown) => void;
    mocked.listApps.mockReturnValue(new Promise((r) => (resolve = r)) as never);
    renderAt("/app/game/a1");
    expect(screen.getByText("Loading…")).toBeTruthy();
    expect(screen.queryByText("This game is not in your library")).toBeNull();
    await act(async () => resolve({ items: [game] }));
    expect(await screen.findByRole("heading", { level: 1, name: "Portal 2" })).toBeTruthy();
  });

  it("says so for a game that is not in the library, once it has loaded", async () => {
    renderAt("/app/game/nope");
    expect(await screen.findByText("This game is not in your library")).toBeTruthy();
    expect(mocked.listApps).toHaveBeenCalled();
  });

  it("shows a catalogue failure with Try again", async () => {
    mocked.listApps.mockRejectedValueOnce(new ApiError(500, "internal", "db down"));
    renderAt("/app/game/a1");
    expect(await screen.findByText("Your library didn't load")).toBeTruthy();
    expect(screen.queryByText("This game is not in your library")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Portal 2" })).toBeTruthy();
  });

  it("under Classic layout hands the game to Home's band", async () => {
    design.on = false;
    renderAt("/app/game/a1");
    await waitFor(() => expect(screen.getByTestId("home").textContent).toBe('{"openApp":"a1"}'));
  });
});
