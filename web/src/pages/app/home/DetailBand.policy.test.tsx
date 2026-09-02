// #525 — a policy-pinned app must not offer overrides it will be refused for,
// and a launch refusal must never be silent.
//
// Live on devbox 2026-08-24: an app with `profile_policy: "force"` (Quasar
// Benchapp, pinned to 1080p60) still rendered the full 4K/1440p/1080p ×
// 60–120 grid. Picking 1440p120 made every Play POST a `profile_id` the
// control plane refuses with `409 profile overrides are disabled for this
// launch` — observed seven consecutive times with no visible feedback.
//
// Two assertions, matching the two prongs of the fix:
//   1. a launch 409 that is NOT the capacity bounce reaches the toast, always,
//   2. a forced app's resolution + frame-rate rows are disabled and say why,
//      while the codec row — a `stream.codec`-only override IS accepted on a
//      forced app — stays live.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppHomeNext } from "../AppHomeNext";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { LibraryCatalogProvider } from "../libraryCatalog";
import { LibrarySearchContext } from "../librarySearchContext";
import { ToastProvider } from "../../../components/Toast";
import * as libraryApi from "../../../api/library";
import { ApiError } from "../../../api/client";
import type { App } from "../../../api/types";

vi.mock("../../../api/library");

// jsdom answers nothing for the decode probe; pin it so the codec segment and
// the rung grid exist at all.
vi.mock("../../../webrtc/capability", () => ({
  probeCodecs: () => ({ h264: true, hevc: true, av1: false }),
}));

const mocked = vi.mocked(libraryApi);

function app(over: Partial<Record<keyof App, unknown>> = {}): App {
  return {
    id: "a1",
    name: "Benchapp",
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
    default_profile_id: "1080p60",
    profile_policy: "inherit",
    favourite: false,
    enabled: true,
    ...over,
  } as unknown as App;
}

const rung = (id: string, codec: string, width: number, height: number, fps: number, kbps: number) => ({
  id,
  display_name: id,
  codec,
  width,
  height,
  fps,
  nominal_bitrate_kbps: kbps,
  position: 1,
  eligibility: "eligible",
  reasons: [],
});

const launchProfile = (id: string, r: ReturnType<typeof rung>) => ({
  id,
  display_name: id,
  description: "",
  nominal: { width: r.width, height: r.height, fps: r.fps, bitrate_kbps: r.nominal_bitrate_kbps },
  eligibility: "eligible",
  reasons: [],
  rungs: [r],
});

/** Two launch profiles, so "the recommendation is not the app's pin" is
 *  representable — which is the exact shape that produced the 409. */
const profiles = {
  recommended_id: "1440p120",
  confidence: "high",
  notes: [],
  profiles: [
    launchProfile("1440p120", rung("1440p120-h264", "h264", 2560, 1440, 120, 30000)),
    launchProfile("1080p60", rung("1080p60-h264", "h264", 1920, 1080, 60, 8000)),
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

function renderHome(over: Partial<AuthContextValue> = {}) {
  return render(
    <MemoryRouter initialEntries={["/app"]}>
      <AuthContext.Provider value={{ ...auth, ...over }}>
        <LibrarySearchContext.Provider value={{ query: "", setQuery: vi.fn() }}>
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

/** Open the app's detail band and wait for its profiles to land. */
async function openBand(over: Partial<AuthContextValue> = {}) {
  renderHome(over);
  await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(1), { timeout: 8000 });
  fireEvent.click(screen.getByRole("button", { name: "Benchapp" }));
  await waitFor(() => expect(document.querySelector(".d-specs")).toBeTruthy(), { timeout: 8000 });
  // `.d-specs` renders on the profiles response, but the selection is seeded by
  // an effect one flush later — under React 19 that lands after `waitFor`
  // returns, so the band would still be showing the app's defaults here.
  await act(async () => {});
}

async function openOptions() {
  fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
  await waitFor(() => expect(document.querySelector(".lo.show")).toBeTruthy(), { timeout: 8000 });
}

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue(profiles as never);
});

describe("a launch 409 is never silent", () => {
  // Explicit per-assertion timeouts plus a longer test budget: under heavy
  // parallel test load this render (full AppHomeNext + detail band + launch
  // round trip) can take longer than RTL's/vitest's 1s/5s defaults even
  // though nothing is actually hung — it passes alone in well under 1s.
  it(
    "surfaces the server's own message through the launch toast",
    async () => {
      mocked.listApps.mockResolvedValue({ items: [app()] } as never);
      mocked.launchSession.mockRejectedValue(
        new ApiError(409, "conflict", "profile overrides are disabled for this launch"),
      );

      await openBand();
      fireEvent.click(screen.getByRole("button", { name: /^Play$/ }));

      await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled(), { timeout: 8000 });
      // The server's words, verbatim — this is the launch failure the user spent
      // seven clicks not being told about.
      expect(
        await screen.findByText(/profile overrides are disabled for this launch/, {}, { timeout: 8000 }),
      ).toBeInTheDocument();
    },
    15_000,
  );

  it("leaves Play usable afterwards, so a second attempt is not a dead button", async () => {
    mocked.listApps.mockResolvedValue({ items: [app()] } as never);
    mocked.launchSession.mockRejectedValue(new ApiError(409, "conflict", "nope"));

    await openBand();
    const play = screen.getByRole("button", { name: /^Play$/ });
    fireEvent.click(play);
    await screen.findByText("nope");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^Play$/ })).not.toBeDisabled(),
    );
  });
});

describe("a policy-pinned app does not offer overrides it will be refused for", () => {
  it("disables the resolution and frame-rate rows and says why", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "1080p60" })],
    } as never);

    await openBand();
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    // The two columns the pin freezes. (Every column is `.qp-row` in v3, so
    // this has to be scoped — the codec column below must stay live.)
    for (const col of ['[aria-label="Resolution"]', '[aria-label="Frame rate"]']) {
      const rows = panel.querySelectorAll<HTMLButtonElement>(`${col} .qp-row`);
      expect(rows.length).toBeGreaterThan(0);
      for (const row of rows) expect(row.disabled).toBe(true);
    }
    // A disabled control with no explanation is its own defect.
    expect(panel.textContent).toMatch(/Fixed by this app/);
  });

  it("keeps the codec segment live — a codec-only override IS accepted", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "1080p60" })],
    } as never);

    await openBand();
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    const codecButtons = panel.querySelectorAll<HTMLButtonElement>('[aria-label="Codec"] .qp-row');
    expect(codecButtons.length).toBeGreaterThan(0);
    for (const btn of codecButtons) expect(btn.disabled).toBe(false);
  });

  it("commits the APP'S pinned profile, not the device recommendation", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "1080p60" })],
    } as never);
    mocked.launchSession.mockResolvedValue({
      session: {
        id: "s1",
        stream: { width: 1920, height: 1080, fps: 60, codec: "h264", playout0_ms: 50, mic: true },
      },
      signaling: { url: "ws://x", token: "t" },
    } as never);

    await openBand();
    fireEvent.click(screen.getByRole("button", { name: /^Play$/ }));

    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    const req = mocked.launchSession.mock.calls[0][1];
    expect(req.profile_id).toBe("1080p60");
  });

  it("leaves an UNPINNED app's rows selectable", async () => {
    mocked.listApps.mockResolvedValue({ items: [app({ profile_policy: "inherit" })] } as never);

    await openBand();
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    const rows = [...panel.querySelectorAll<HTMLButtonElement>('[aria-label="Resolution"] .qp-row')];
    expect(rows.some((r) => !r.disabled)).toBe(true);
    expect(panel.textContent).not.toMatch(/Fixed by this app/);
  });

  it("sends the PIN, not whatever the lossy draft resolves back to", async () => {
    // Two launch profiles at the SAME height and fps. A draft is only
    // {codec, fps, height}, so it cannot tell them apart and resolveSelection
    // returns the first — which here is NOT the pin. Round-tripping the
    // selection would launch the wrong profile and 409.
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "1080p60-alt",
      confidence: "high",
      notes: [],
      profiles: [
        launchProfile("1080p60-alt", rung("alt-h264", "h264", 1920, 1080, 60, 8000)),
        launchProfile("1080p60", rung("pin-h264", "h264", 1920, 1080, 60, 8000)),
      ],
    } as never);
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "1080p60" })],
    } as never);
    mocked.launchSession.mockResolvedValue({
      session: {
        id: "s1",
        stream: { width: 1920, height: 1080, fps: 60, codec: "h264", playout0_ms: 50, mic: true },
      },
      signaling: { url: "ws://x", token: "t" },
    } as never);

    await openBand();
    fireEvent.click(screen.getByRole("button", { name: /^Play$/ }));

    await waitFor(() => expect(mocked.launchSession).toHaveBeenCalled());
    expect(mocked.launchSession.mock.calls[0][1].profile_id).toBe("1080p60");
  });
});

describe("a pin that does not resolve is a fallback, not a dead end", () => {
  it("leaves the rows live when a forced app has NO pinned profile", async () => {
    // profile_policy "force" with a null default_profile_id: freezing the rows
    // would lock the user onto the recommendation the server still refuses,
    // with no way to change it. Falling back to the ordinary panel at least
    // fails loudly through the toast.
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: null })],
    } as never);

    await openBand();
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    const rows = [...panel.querySelectorAll<HTMLButtonElement>('[aria-label="Resolution"] .qp-row')];
    expect(rows.some((r) => !r.disabled)).toBe(true);
    expect(panel.textContent).not.toMatch(/Fixed by this app/);
  });

  it("leaves the rows live when the pin names a profile this menu does not offer", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "a-profile-nobody-has" })],
    } as never);

    await openBand();
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    const rows = [...panel.querySelectorAll<HTMLButtonElement>('[aria-label="Resolution"] .qp-row')];
    expect(rows.some((r) => !r.disabled)).toBe(true);
  });

  it("does not disable an ADMIN's rows — the server exempts them", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app({ profile_policy: "force", default_profile_id: "1080p60" })],
    } as never);

    await openBand({ isAdmin: true });
    await openOptions();

    const panel = document.querySelector(".lo.show") as HTMLElement;
    const rows = [...panel.querySelectorAll<HTMLButtonElement>('[aria-label="Resolution"] .qp-row')];
    expect(rows.some((r) => !r.disabled)).toBe(true);
  });
});
