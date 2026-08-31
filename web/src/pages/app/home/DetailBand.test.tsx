// The detail band's own structure and wiring (v3 handoff §B "Detail view").
// DetailBand.policy.test.tsx and DetailBand.reasons.test.tsx drive it through
// the page for the two defects it exists to answer; this renders it directly.

import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DetailBand } from "./DetailBand";
import { AuthContext, type AuthContextValue } from "../../../auth/context";
import type { App, ProfilesResponse } from "../../../api/types";

const CAPS = { h264: true, hevc: true, av1: true, vp9: false };

function app(over: Partial<Record<keyof App, unknown>> = {}): App {
  return {
    id: "a1",
    name: "Portal 2",
    description: "Now you're thinking with portals.",
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
  } as unknown as App;
}

const PROFILES = {
  recommended_id: "p-1440",
  confidence: "high",
  notes: [],
  profiles: [
    {
      id: "p-1440",
      display_name: "1440p60",
      description: "",
      nominal: { width: 2560, height: 1440, fps: 60, bitrate_kbps: 12000 },
      eligibility: "eligible",
      reasons: [],
      rungs: [
        {
          id: "r1",
          display_name: "1440p60",
          codec: "av1",
          width: 2560,
          height: 1440,
          fps: 60,
          nominal_bitrate_kbps: 12000,
          position: 1,
          eligibility: "eligible",
          reasons: [],
        },
      ],
    },
    {
      id: "p-1080",
      display_name: "1080p60",
      description: "",
      nominal: { width: 1920, height: 1080, fps: 60, bitrate_kbps: 8000 },
      eligibility: "eligible",
      reasons: [],
      rungs: [
        {
          id: "r2",
          display_name: "1080p60",
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
} as unknown as ProfilesResponse;

const auth: AuthContextValue = {
  status: "authenticated",
  user: { id: "u1", email: "a@b.c", username: "u", role: "user" } as never,
  token: "tok",
  isAdmin: false,
  login: vi.fn(),
  claim: vi.fn(),
  logout: vi.fn(),
};

function renderBand(over: Partial<Parameters<typeof DetailBand>[0]> = {}) {
  const props = {
    app: app(),
    codecCaps: CAPS,
    launching: false,
    waitingForSlot: false,
    profiles: PROFILES,
    optionsOpen: false,
    optionsToggleRef: createRef<HTMLButtonElement>(),
    onToggleOptions: vi.fn(),
    onCloseOptions: vi.fn(),
    onRetryProfiles: vi.fn(),
    onClose: vi.fn(),
    onConfirmProfile: vi.fn(),
    onToggleFavourite: vi.fn(),
    isBlocked: false,
    blockedByName: null,
    liveSessionId: null,
    isLive: false,
    onResume: vi.fn(),
    ...over,
  };
  return {
    ...render(
      <MemoryRouter>
        <AuthContext.Provider value={auth}>
          <DetailBand {...props} />
        </AuthContext.Provider>
      </MemoryRouter>,
    ),
    props,
  };
}

describe("DetailBand", () => {
  it("names the app, its kind and its description", () => {
    renderBand();
    expect(document.querySelector(".d-kind")?.textContent).toBe("Game");
    expect(screen.getByRole("heading", { level: 2 })).toHaveTextContent("Portal 2");
    expect(document.querySelector(".d-desc")?.textContent).toMatch(/thinking with portals/);
  });

  it("labels a desktop and a launcher as themselves", () => {
    const { unmount } = renderBand({ app: app({ kind: "desktop" }) });
    expect(document.querySelector(".d-kind")?.textContent).toBe("Desktop");
    unmount();
    renderBand({ app: app({ kind: "launcher" }) });
    expect(document.querySelector(".d-kind")?.textContent).toBe("Launcher");
  });

  it("prints the recommended rung across the four spec segments", () => {
    renderBand();
    const specs = Array.from(document.querySelectorAll(".d-specs .sp")).map((sp) => [
      sp.querySelector(".l")?.textContent,
      sp.querySelector(".v")?.textContent,
    ]);
    expect(specs).toEqual([
      ["Resolution", "2560×1440"],
      ["Frame rate", "60 fps"],
      ["Bitrate", "12 Mbps"],
      ["Codec", "Auto · AV1"],
    ]);
  });

  it("says the selection is the recommendation, and offers Adjust", () => {
    const { props } = renderBand();
    expect(document.querySelector(".d-rec")?.textContent).toMatch(/Recommended for this device\./);
    expect(document.querySelector(".d-rec")?.getAttribute("data-tone")).toBe("ok");
    fireEvent.click(screen.getByRole("button", { name: /Adjust/ }));
    expect(props.onToggleOptions).toHaveBeenCalled();
  });

  it("holds the overlay closed until it is opened", () => {
    const { unmount } = renderBand();
    expect(document.querySelector(".lo")).not.toHaveClass("show");
    unmount();
    renderBand({ optionsOpen: true });
    expect(document.querySelector(".lo.show")).not.toBeNull();
    expect(screen.getByRole("button", { name: "Close options" })).toBeInTheDocument();
  });

  it("moves focus into the overlay when it opens", () => {
    const { rerender, props } = renderBand();
    expect(document.activeElement).toBe(document.body);
    rerender(
      <MemoryRouter>
        <AuthContext.Provider value={auth}>
          <DetailBand {...props} optionsOpen />
        </AuthContext.Provider>
      </MemoryRouter>,
    );
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close options" }));
  });

  it("routes Cancel and the overlay's close through the path that restores focus", () => {
    const { props } = renderBand({ optionsOpen: true });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Close options" }));
    fireEvent.click(screen.getByRole("button", { name: "Close details" }));
    expect(props.onCloseOptions).toHaveBeenCalledTimes(3);
    // The bare toggle would leave focus on a control the overlay covers.
    expect(props.onToggleOptions).not.toHaveBeenCalled();
  });

  it("makes the covered band inert, so Tab cannot walk back out of the overlay", () => {
    const { unmount } = renderBand({ optionsOpen: true });
    const inner = document.querySelector(".d-inner")!;
    expect(inner).toHaveAttribute("inert");
    expect(inner.contains(screen.getByRole("button", { name: /Adjust/ }))).toBe(true);
    unmount();

    renderBand();
    expect(document.querySelector(".d-inner")).not.toHaveAttribute("inert");
  });

  it("falls back to the glyph when the app has no hero art", () => {
    const { unmount } = renderBand();
    expect(document.querySelector(".hero-art img")).toBeNull();
    expect(document.querySelector(".hero-art .glyph")?.textContent).toBe("P2");
    unmount();

    renderBand({ app: app({ hero_url: "/v1/artwork/assets/a1-hero.png" }) });
    const img = document.querySelector<HTMLImageElement>(".hero-art img");
    expect(img?.getAttribute("src")).toBe("/v1/artwork/assets/a1-hero.png");
    expect(img?.getAttribute("decoding")).toBe("async");
  });

  it("closes the overlay first and the band only once it is closed", () => {
    const { props, unmount } = renderBand({ optionsOpen: true });
    fireEvent.click(screen.getByRole("button", { name: "Close details" }));
    expect(props.onCloseOptions).toHaveBeenCalled();
    expect(props.onClose).not.toHaveBeenCalled();
    unmount();

    const second = renderBand();
    fireEvent.click(screen.getByRole("button", { name: "Close details" }));
    expect(second.props.onClose).toHaveBeenCalled();
  });

  it("launches the committed selection", () => {
    const { props } = renderBand();
    fireEvent.click(screen.getByRole("button", { name: /^Play$/ }));
    // Auto: no codec on the wire, the server still resolves it.
    expect(props.onConfirmProfile).toHaveBeenCalledWith("p-1440", undefined);
  });

  it("commits and launches in one press from the overlay", () => {
    const { props } = renderBand({ optionsOpen: true });
    fireEvent.click(screen.getByRole("radio", { name: /H\.264/ }));
    fireEvent.click(screen.getByRole("button", { name: /Play now/ }));
    // The h264 rung is 1080p, so the draft healed to it on the way.
    expect(props.onConfirmProfile).toHaveBeenCalledWith("p-1080", "h264");
    // …and the band's own strip now reads back the committed selection.
    expect(document.querySelector(".d-specs")?.textContent).toContain("1920×1080");
  });

  it("resumes instead of launching when this app owns the live session", () => {
    const { props } = renderBand({ isLive: true, liveSessionId: "s1" });
    const resume = screen.getByRole("button", { name: /Resume session/ });
    fireEvent.click(resume);
    expect(props.onResume).toHaveBeenCalled();
    expect(props.onConfirmProfile).not.toHaveBeenCalled();
    expect(document.querySelector(".note")?.textContent).toMatch(/Resume returns you to that session/);
  });

  it("names the app holding the home and links to its session when blocked", () => {
    renderBand({ isBlocked: true, blockedByName: "Steam", liveSessionId: "s9" });
    expect(document.querySelector(".note.warn")?.textContent).toMatch(/Steam.*is already running/);
    expect(screen.getByRole("link", { name: "Go to your session" })).toHaveAttribute(
      "href",
      "/app/session/s9",
    );
    expect(screen.getByRole("button", { name: /^Play$/ })).toBeDisabled();
  });

  it("wires the heart to the favourite round trip", () => {
    const { props, unmount } = renderBand();
    const heart = screen.getByRole("button", { name: "Add Portal 2 to favourites" });
    expect(heart).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(heart);
    expect(props.onToggleFavourite).toHaveBeenCalled();
    unmount();

    renderBand({ app: app({ favourite: true }) });
    expect(screen.getByRole("button", { name: "Remove Portal 2 from favourites" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
  });

  it("offers a retry, not a dead panel, when the evaluation failed", () => {
    const { props } = renderBand({ profiles: "could not load stream profiles" });
    expect(document.querySelector(".d-rec")?.textContent).toMatch(/Could not load stream profiles/);
    expect(document.querySelector(".d-rec")?.getAttribute("data-tone")).toBe("off");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(props.onRetryProfiles).toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /Adjust/ })).toBeNull();
  });

  it("says it is waiting for a slot rather than looking stuck (#494)", () => {
    renderBand({ launching: true, waitingForSlot: true });
    const actions = document.querySelector(".d-actions") as HTMLElement;
    expect(within(actions).getByRole("button", { name: /Waiting for a slot…/ })).toBeInTheDocument();
    expect(document.querySelector(".note")?.textContent).toMatch(/waiting for a slot/i);
  });

  it("refuses to pretend a browser with no H.264 can play anything", () => {
    renderBand({ codecCaps: { ...CAPS, h264: false } });
    expect(screen.getByRole("alert").textContent).toMatch(/Without H\.264/);
    expect(screen.getByRole("button", { name: /^Play$/ })).toBeDisabled();
  });

  it("takes the ref the page scrolls the band into view with", () => {
    const ref = createRef<HTMLDivElement>();
    render(
      <MemoryRouter>
        <AuthContext.Provider value={auth}>
          <DetailBand
            ref={ref}
            app={app()}
            codecCaps={CAPS}
            launching={false}
            profiles={PROFILES}
            optionsOpen={false}
            optionsToggleRef={createRef<HTMLButtonElement>()}
            onToggleOptions={vi.fn()}
            onCloseOptions={vi.fn()}
            onRetryProfiles={vi.fn()}
            onClose={vi.fn()}
            onConfirmProfile={vi.fn()}
            onToggleFavourite={vi.fn()}
            isBlocked={false}
            blockedByName={null}
            liveSessionId={null}
            isLive={false}
            onResume={vi.fn()}
          />
        </AuthContext.Provider>
      </MemoryRouter>,
    );
    expect(ref.current).toHaveClass("detail");
  });
});
