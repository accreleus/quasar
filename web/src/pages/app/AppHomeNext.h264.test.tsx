// #565 — the library must not follow the device probe's H.264 verdict. A Firefox without
// its OpenH264 plugin is told so by a launch it cannot play, but the library keeps reading
// the plain capability list: the control plane ignores `codecs.h264`, and a Firefox that can
// decode AV1 on a multi-codec profile has to stay playable. This runs the real capability
// module, so coupling `probeCodecs()` to the verdict again turns the first test red. #605:
// a list without H.264 refuses only a profile that has nothing else this browser decodes.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppHomeNext } from "./AppHomeNext";
import { probeH264Decodable } from "../../webrtc/capability";
import { AuthContext, type AuthContextValue } from "../../auth/context";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { ToastProvider } from "../../components/Toast";
import * as libraryApi from "../../api/library";
import type { App } from "../../api/types";

vi.mock("../../api/library");
const mocked = vi.mocked(libraryApi);

const FIREFOX_UA = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0";
const NOTE = /Without H\.264 this device cannot play any stream/;

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

const portal = {
  id: "a1", name: "Portal 2", description: "", kind: "game", cover_url: "/v1/artwork/assets/a1.png", hero_url: null,
  parent_app_id: null, external_source: "", external_id: "", default_width: 1920, default_height: 1080,
  default_fps: 60, default_bitrate_kbps: 8000, favourite: false, enabled: true,
} as unknown as App;

/** A browser listing AV1, and H.264 when `listsH264`, whose loopback answer is `answer`. */
function stubFirefox(listsH264: boolean, answer: string) {
  Object.defineProperty(navigator, "userAgent", { value: FIREFOX_UA, configurable: true });
  vi.stubGlobal("RTCRtpReceiver", {
    getCapabilities: () => ({
      codecs: [{ mimeType: "video/VP8" }, { mimeType: "video/AV1" }, ...(listsH264 ? [{ mimeType: "video/H264" }] : [])],
    }),
  });
  vi.stubGlobal(
    "RTCPeerConnection",
    class {
      async setRemoteDescription() {}
      async createAnswer() {
        return { sdp: answer };
      }
      close() {}
    },
  );
}

async function openBand() {
  render(
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
  await waitFor(() => expect(document.querySelector(".home-feat")).not.toBeNull());
  fireEvent.click(screen.getByRole("button", { name: /Portal 2, Newly added\. Show details/ }));
  await waitFor(() => expect(document.querySelector(".detail")).not.toBeNull());
}

const rung = (codec: string, position: number) => ({
  id: `1080p60-${codec}`, display_name: "1080p60", codec, width: 1920, height: 1080, fps: 60,
  nominal_bitrate_kbps: 8000, position, eligibility: "eligible", reasons: [],
});
const H264 = rung("h264", 2);
const AV1 = rung("av1", 1);

/** One eligible 1080p60 launch profile over `rungs`, as the control plane reports it today. */
function profiles(rungs: unknown[]) {
  return {
    recommended_id: "p", confidence: "high", notes: [],
    profiles: [{
      id: "p", display_name: "1080p60", description: "",
      nominal: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 8000 },
      eligibility: "eligible", reasons: [], rungs,
    }],
  } as never;
}

const play = () => screen.getByRole("button", { name: /^Play$/ });

beforeEach(() => {
  vi.resetAllMocks();
  mocked.listApps.mockResolvedValue({ items: [portal, { ...portal, id: "a9", name: "Filler" }] } as never);
  mocked.getMySessions.mockResolvedValue({ items: [], next_cursor: null } as never);
  mocked.getHighlights.mockResolvedValue({
    items: [{ app_id: "a1", reason: "recently_added", session_id: null, session_started_at: null, play_seconds: 0, last_played_at: null }],
  } as never);
  mocked.getProfiles.mockResolvedValue(profiles([H264]));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  delete (navigator as unknown as { userAgent?: unknown }).userAgent;
});

describe("the library and the H.264 probe verdict (#565)", () => {
  it("does not block play when the probe found a listed H.264 undecodable", async () => {
    stubFirefox(true, "m=video 0 UDP/TLS/RTP/SAVPF 120\r\n");
    expect(await probeH264Decodable()).toBe(false); // the verdict is in, as after a login
    await openBand();
    await waitFor(() => expect(play()).toBeEnabled());
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("still blocks an H.264-only profile when the capability list leaves H.264 out", async () => {
    stubFirefox(false, "m=video 0 UDP/TLS/RTP/SAVPF 120\r\n");
    await openBand();
    expect(await screen.findByText(NOTE)).toBeInTheDocument();
    expect(play()).toBeDisabled();
  });

  it("plays a profile with an AV1 rung when the capability list leaves H.264 out (#605)", async () => {
    stubFirefox(false, "m=video 0 UDP/TLS/RTP/SAVPF 120\r\n");
    mocked.getProfiles.mockResolvedValue(profiles([AV1, H264]));
    await openBand();
    await waitFor(() => expect(play()).toBeEnabled());
    expect(screen.queryByText(NOTE)).toBeNull();
  });
});
