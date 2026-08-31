// The band as the page mounts it: the focus round trip through the overlay, and
// the band's identity when two apps share one row's slot.
//
// Both are page-level by nature — focus returns to the Adjust button the page
// owns the ref for, and the carried-state defect only exists because the band
// renders in the same slot for every app in a row.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

// jsdom answers nothing for the decode probe; pin it so the rung grid exists.
vi.mock("../../../webrtc/capability", () => ({
  probeCodecs: () => ({ h264: true, hevc: true, av1: true }),
}));

const mocked = vi.mocked(libraryApi);

function app(id: string, name: string): App {
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
  } as unknown as App;
}

const rung = (codec: string, width: number, height: number, fps: number, kbps: number) => ({
  id: `${codec}-${height}`,
  display_name: `${height}p${fps}`,
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

/** 1440p is the recommendation, 1080p the option a user can move to. */
const profiles = {
  recommended_id: "p-1440",
  confidence: "high",
  notes: [],
  profiles: [
    launchProfile("p-1440", rung("av1", 2560, 1440, 60, 12000)),
    launchProfile("p-1080", rung("av1", 1920, 1080, 60, 8000)),
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

function renderHome() {
  return render(
    <MemoryRouter initialEntries={["/app"]}>
      <AuthContext.Provider value={auth}>
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

async function openBand(name: string, tiles: number) {
  await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(tiles));
  fireEvent.click(screen.getByRole("button", { name }));
  await waitFor(() => expect(document.querySelector(".d-specs")).toBeTruthy());
}

async function openOptions() {
  fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
  await waitFor(() => expect(document.querySelector(".lo.show")).toBeTruthy());
}

const specText = () => document.querySelector(".d-specs")?.textContent ?? "";

beforeEach(() => {
  vi.resetAllMocks();
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
  mocked.getProfiles.mockResolvedValue(profiles as never);
});

describe("the overlay's focus round trip", () => {
  beforeEach(() => {
    mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
  });

  it("moves focus into the overlay and hands it back to Adjust on Cancel", async () => {
    renderHome();
    await openBand("Portal 2", 1);
    const adjust = screen.getByRole("button", { name: /Adjust/ });

    await openOptions();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close options" }));
    // The band behind the overlay holds no tab stops while it is covered.
    expect(document.querySelector(".d-inner")).toHaveAttribute("inert");

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeNull());
    expect(document.activeElement).toBe(adjust);
    expect(document.querySelector(".d-inner")).not.toHaveAttribute("inert");
  });

  it("hands focus back from the overlay's own close control", async () => {
    renderHome();
    await openBand("Portal 2", 1);
    const adjust = screen.getByRole("button", { name: /Adjust/ });

    await openOptions();
    fireEvent.click(screen.getByRole("button", { name: "Close options" }));
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeNull());
    expect(document.activeElement).toBe(adjust);
  });

  it("hands focus back on Escape, and only then closes the band", async () => {
    renderHome();
    await openBand("Portal 2", 1);
    const adjust = screen.getByRole("button", { name: /Adjust/ });

    await openOptions();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeNull());
    expect(document.activeElement).toBe(adjust);
    expect(document.querySelector(".detail")).not.toBeNull();
  });
});

describe("two apps in one row do not share a selection", () => {
  it("seeds the second app from its own catalogue, not the first's committed draft", async () => {
    mocked.listApps.mockResolvedValue({
      items: [app("a1", "Portal 2"), app("a2", "Half-Life")],
    } as never);
    // The launch is refused, so the band stays open with the committed draft on
    // it — the state that must not survive into the next app.
    mocked.launchSession.mockRejectedValue(new ApiError(409, "conflict", "nope"));

    renderHome();
    await openBand("Portal 2", 2);
    expect(specText()).toContain("2560×1440");

    await openOptions();
    fireEvent.click(screen.getByRole("radio", { name: /1080p/ }));
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    await waitFor(() => expect(specText()).toContain("1920×1080"));

    // Both tiles share a row, so the band renders in the same slot for both.
    fireEvent.click(screen.getByRole("button", { name: "Half-Life" }));
    await waitFor(() =>
      expect(document.querySelector(".detail h2")?.textContent).toBe("Half-Life"),
    );
    await waitFor(() => expect(specText()).toContain("2560×1440"));
  });
});
