// The launch band SURFACES the eligibility reasons it used to drop
// (UX assessment §2.6).
//
// `GET /v1/me/profiles` sends `reasons[]` on every verdict. The band rendered a
// bare "RISKY" tag with no tooltip, no title and no text; the "no launchable
// stream profile" case was a dead end with no next action; and the codec
// segment offered Auto/H.264/HEVC/AV1 with nothing to choose on. These tests
// assert the visible half of that fix — the code→sentence mapping itself is
// pinned in launchReasons.test.ts.
//
// `LibraryDetail` is shared with the classic page, so this is one behaviour for
// both; the resume test file covers the classic render path.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppHomeNext } from "../AppHomeNext";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import { LibraryCatalogProvider } from "../libraryCatalog";
import { LibrarySearchContext } from "../librarySearchContext";
import { ToastProvider } from "../../../components/Toast";
import * as libraryApi from "../../../api/library";
import type { App } from "../../../api/types";

vi.mock("../../../api/library");

// The band's codec segment is intersected with the client's decode probe, and
// jsdom answers nothing — pin the probe so the panel's codec buttons exist.
vi.mock("../../../webrtc/capability", () => ({
  probeCodecs: () => ({ h264: true, hevc: true, av1: false }),
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
    favourite: false,
    enabled: true,
  } as unknown as App;
}

const rung = (
  id: string,
  codec: string,
  width: number,
  height: number,
  fps: number,
  position: number,
  kbps: number,
  eligibility = "eligible",
  reasons: Array<{ code: string; message: string }> = [],
) => ({
  id,
  display_name: id,
  codec,
  width,
  height,
  fps,
  nominal_bitrate_kbps: kbps,
  position,
  eligibility,
  reasons,
});

const launchProfile = (
  id: string,
  rungs: ReturnType<typeof rung>[],
  eligibility = "eligible",
  reasons: Array<{ code: string; message: string }> = [],
) => ({
  id,
  display_name: id,
  description: "",
  nominal: {
    width: rungs[0].width,
    height: rungs[0].height,
    fps: rungs[0].fps,
    bitrate_kbps: rungs[0].nominal_bitrate_kbps,
  },
  eligibility,
  reasons,
  rungs,
});

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

/** Open the one app's detail band and wait for its profiles to land. */
async function openBand() {
  renderHome();
  await waitFor(() => expect(document.querySelectorAll(".lib-tile").length).toBe(1));
  fireEvent.click(screen.getByRole("button", { name: "Portal 2" }));
  await waitFor(() => expect(document.querySelector(".d-specs")).toBeTruthy());
}

const bandText = () => document.querySelector(".detail")?.textContent ?? "";

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listApps.mockResolvedValue({ items: [app("a1", "Portal 2")] } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({ items: [] } as never);
});

describe("a risky selection says what makes it risky", () => {
  beforeEach(() => {
    // A risky profile CAN be `recommended_id`: when nothing is fully eligible
    // the server recommends the lowest-demand entry anyway, at low confidence
    // (profile/launch.go recommendLaunch). That combination is exactly where a
    // suppressed warning does the most damage.
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "1080p60",
      confidence: "low",
      notes: [],
      profiles: [
        launchProfile(
          "1080p60",
          [rung("1080p60-h264", "h264", 1920, 1080, 60, 1, 8000, "risky", [
            // The wire's own phrasing, verbatim from eligibility.go.
            { code: "bandwidth_too_low", message: "bandwidth is below the recommended headroom; quality may be unstable" },
          ])],
          "risky",
          [{ code: "bandwidth_too_low", message: "bandwidth is below the recommended headroom; quality may be unstable" }],
        ),
      ],
    } as never);
  });

  it("explains the risk in the user's words, not the wire's", async () => {
    await openBand();
    const why = document.querySelector(".lib-why");
    expect(why).toBeTruthy();
    expect(why?.textContent).toMatch(/room to spare|wobble/i);
    // The operator-facing phrasing never reaches the screen.
    expect(bandText()).not.toContain("recommended headroom");
  });

  it("tells the user what to do about it", async () => {
    await openBand();
    expect(document.querySelector(".lib-why")?.textContent).toMatch(/Adjust/);
  });

  it("labels the risky row in the cascade panel with the same reason", async () => {
    await openBand();
    fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeTruthy());

    const tag = document.querySelector<HTMLElement>(".qp-tag.risky");
    expect(tag).toBeTruthy();
    // Recommended AND risky: both are true, so both are shown.
    expect(document.querySelectorAll(".qp-tag")).toHaveLength(2);
    // The badge is no longer bare: it carries the reason, and the row prints it.
    expect(tag?.getAttribute("title")).toMatch(/room to spare|wobble/i);
    expect(document.querySelector(".qr-why")?.textContent).toMatch(/room to spare|wobble/i);
  });
});

describe("an unknown reason code degrades rather than vanishing", () => {
  it("shows the server's message when the code is one this build never heard of", async () => {
    // A risky profile CAN be `recommended_id`: when nothing is fully eligible
    // the server recommends the lowest-demand entry anyway, at low confidence
    // (profile/launch.go recommendLaunch). That combination is exactly where a
    // suppressed warning does the most damage.
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "1080p60",
      confidence: "low",
      notes: [],
      profiles: [
        launchProfile(
          "1080p60",
          [rung("1080p60-h264", "h264", 1920, 1080, 60, 1, 8000, "risky", [
            { code: "thermal_budget_exceeded", message: "the host GPU is thermally throttled" },
          ])],
          "risky",
          [{ code: "thermal_budget_exceeded", message: "the host GPU is thermally throttled" }],
        ),
      ],
    } as never);

    await openBand();
    expect(document.querySelector(".lib-why")?.textContent).toContain(
      "The host GPU is thermally throttled.",
    );
    // Never the raw code.
    expect(bandText()).not.toContain("thermal_budget_exceeded");
  });
});

describe("the dead end has a next action", () => {
  it("says why nothing can stream, offers a re-check, and disables Play", async () => {
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "4k60",
      confidence: "low",
      notes: [{ code: "probe_stale", message: "device probe is stale; network not freshly measured" }],
      profiles: [
        launchProfile(
          "4k60",
          [rung("4k60-h264", "h264", 3840, 2160, 60, 1, 25000, "ineligible", [
            { code: "decode_height_too_low", message: "client decode height is below the profile resolution" },
          ])],
          "ineligible",
          [{ code: "decode_height_too_low", message: "client decode height is below the profile resolution" }],
        ),
      ],
    } as never);

    await openBand();

    const why = document.querySelector(".lib-why");
    expect(why?.textContent).toContain("Nothing here can stream to this device");
    expect(why?.textContent).toMatch(/can't decode a picture this large/i);
    // The next action: someone can change this, and so can a re-measure.
    expect(why?.textContent).toMatch(/An admin can add a lower-quality option for Portal 2/);
    expect(screen.getByRole("button", { name: "Check again" })).toBeEnabled();

    // …and Play no longer offers to launch something that cannot run. (It used
    // to be enabled and labelled "Custom — recommended is 4K · 60 fps.")
    expect(screen.getByRole("button", { name: /^Play$/ })).toBeDisabled();
    expect(bandText()).not.toContain("Custom — recommended");

    // "Check again" re-runs the evaluation rather than being decoration.
    mocked.getProfiles.mockClear();
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await waitFor(() => expect(mocked.getProfiles).toHaveBeenCalled());
  });
});

describe("the codec segment has a basis", () => {
  beforeEach(() => {
    mocked.getProfiles.mockResolvedValue({
      recommended_id: "1080p60",
      confidence: "high",
      notes: [],
      profiles: [
        launchProfile("1080p60", [
          rung("1080p60-hevc", "hevc", 1920, 1080, 60, 1, 6000),
          rung("1080p60-h264", "h264", 1920, 1080, 60, 2, 8000),
        ]),
      ],
    } as never);
  });

  it("names what Auto actually resolves to, on the card and in the panel", async () => {
    await openBand();
    // The card no longer says only "Auto" — it says what Auto lands on.
    expect(document.querySelector(".d-specs")?.textContent).toContain("Auto · HEVC");

    fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeTruthy());
    const hints = Array.from(document.querySelectorAll(".seg-hint")).map((n) => n.textContent ?? "");
    expect(hints.join(" ")).toMatch(/lands on HEVC/);
    // v3 moves this one onto the column heading itself (mock: `.qp-section`).
    const section = document.querySelector('[aria-label="Codec"] .qp-section');
    expect(section?.getAttribute("title")).toBe("Only codecs this device can decode are listed.");
  });

  it("gives each explicit codec a reason to pick it", async () => {
    await openBand();
    fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
    await waitFor(() => expect(document.querySelector(".lo.show")).toBeTruthy());

    fireEvent.click(screen.getByRole("radio", { name: "HEVC" }));
    await waitFor(() =>
      expect(document.querySelector(".seg-hint")?.textContent).toMatch(/less bandwidth/i),
    );

    fireEvent.click(screen.getByRole("radio", { name: "H.264" }));
    await waitFor(() =>
      expect(document.querySelector(".seg-hint")?.textContent).toMatch(/every device/i),
    );
  });
});
