// The featured rail (v3 handoff §B). The page test covers the rail's data —
// that it renders GET /v1/me/highlights in the server's order and joins each
// app_id against the catalogue. These are the component's own contract: the
// card's three-line body, the kicker variants, the edge rule that hides the
// chevrons, and the roving tab stop.

import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { FeaturedRail } from "./FeaturedRail";
import type { RailCard } from "../homeData";
import type { App } from "../../../api/types";

function app(id: string, name: string, over: Partial<App> = {}): App {
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

const CARDS: RailCard[] = [
  {
    appId: "a1",
    reason: "live",
    kicker: { variant: "live", text: "Session live · 10 minutes" },
    action: "Resume",
    sessionId: "s1",
  },
  {
    appId: "a2",
    reason: "most_played",
    kicker: { variant: "accent", text: "Most played this week" },
    action: "11 h on your server",
  },
  {
    appId: "a3",
    reason: "recently_added",
    kicker: { variant: "info", text: "Newly added" },
    action: "Ready to play",
  },
];

const APPS = new Map([
  ["a1", app("a1", "Portal 2", { cover_url: "/v1/artwork/assets/a1.png" })],
  ["a2", app("a2", "Hades", { cover_url: "/v1/artwork/assets/a2.png" })],
  ["a3", app("a3", "Celeste")],
]);

function renderRail(over: Partial<Parameters<typeof FeaturedRail>[0]> = {}) {
  const trackRef = createRef<HTMLDivElement>();
  const result = render(
    <FeaturedRail
      cards={CARDS}
      trackRef={trackRef}
      appById={APPS}
      coverClassById={new Map()}
      busy={false}
      blockedIds={new Set()}
      onOpen={() => {}}
      onPlay={() => {}}
      {...over}
    />,
  );
  return { ...result, trackRef };
}

describe("FeaturedRail", () => {
  it("renders the mock's three-line body: kicker, action, then the muted name", () => {
    renderRail();
    const body = document.querySelector(".home-feat-body") as HTMLElement;
    expect(Array.from(body.children).map((el) => el.className.split(" ")[0])).toEqual([
      "home-feat-kicker",
      "home-feat-action",
      "home-feat-name",
    ]);
    expect(body.querySelector(".home-feat-kicker")?.textContent).toBe("Session live · 10 minutes");
    expect(body.querySelector(".home-feat-action")?.textContent).toBe("Resume");
    expect(body.querySelector(".home-feat-name")?.textContent).toBe("Portal 2");
  });

  it("colours each kicker by variant and gives only the live one a pulsing dot", () => {
    renderRail();
    const kickers = Array.from(document.querySelectorAll<HTMLElement>(".home-feat-kicker"));
    expect(kickers.map((k) => k.className)).toEqual([
      "home-feat-kicker live",
      "home-feat-kicker accent",
      "home-feat-kicker info",
    ]);
    expect(document.querySelectorAll(".home-feat-kicker .live-dot")).toHaveLength(1);
    expect(kickers[0].querySelector(".live-dot")).not.toBeNull();
  });

  it("keeps the server's reason on the card for styling and telemetry", () => {
    renderRail();
    expect(
      Array.from(document.querySelectorAll(".home-feat")).map((c) => c.getAttribute("data-variant")),
    ).toEqual(["live", "most_played", "recently_added"]);
  });

  it("gives every card a surface tab stop and a play control out of the tab order", () => {
    const onOpen = vi.fn();
    const onPlay = vi.fn();
    renderRail({ onOpen, onPlay });

    const card = document.querySelector(".home-feat") as HTMLElement;
    // A live card resumes; the others launch.
    const play = within(card).getByRole("button", { name: "Resume Portal 2" });
    expect(play.tabIndex).toBe(-1);
    fireEvent.click(play);
    expect(onPlay).toHaveBeenCalledWith(CARDS[0], APPS.get("a1"));

    const surface = within(card).getByRole("button", {
      name: "Portal 2, Session live · 10 minutes. Show details",
    });
    fireEvent.click(surface);
    expect(onOpen).toHaveBeenCalledWith(APPS.get("a1"));
    expect(screen.getByRole("button", { name: "Play Hades" })).toBeInTheDocument();
  });

  it("is ONE tab stop, and the stop follows focus", () => {
    renderRail();
    const surfaces = () =>
      Array.from(document.querySelectorAll<HTMLButtonElement>(".home-feat-surface"));
    expect(surfaces().filter((s) => s.tabIndex === 0)).toHaveLength(1);
    expect(surfaces()[0].tabIndex).toBe(0);

    act(() => surfaces()[2].focus());
    expect(surfaces().filter((s) => s.tabIndex === 0)).toHaveLength(1);
    expect(surfaces()[2].tabIndex).toBe(0);
  });

  it("disables play on a blocked card, but never on the live one", () => {
    renderRail({ blockedIds: new Set(["a1", "a2"]) });
    // a1 carries the live session — its play control resumes, which is what
    // the block exists to send the user to.
    expect(screen.getByRole("button", { name: "Resume Portal 2" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Play Hades" })).toBeDisabled();
  });

  it("hides both chevrons when there is nothing to scroll to", () => {
    renderRail();
    expect(screen.queryByRole("button", { name: /scroll featured/i })).toBeNull();
    expect(document.querySelector(".home-rail-fade")).toBeNull();
  });

  it("shows prev past 20px of scroll and hides next within 20px of the end", () => {
    const { trackRef } = renderRail();
    const track = trackRef.current!;
    Object.defineProperty(track, "clientWidth", { value: 600, configurable: true });
    Object.defineProperty(track, "scrollWidth", { value: 900, configurable: true });

    // 20px in is still "at the start" — the rule is strictly greater.
    track.scrollLeft = 20;
    fireEvent.scroll(track);
    expect(screen.queryByRole("button", { name: "Scroll featured left" })).toBeNull();
    expect(screen.getByRole("button", { name: "Scroll featured right" })).toBeInTheDocument();

    track.scrollLeft = 21;
    fireEvent.scroll(track);
    expect(screen.getByRole("button", { name: "Scroll featured left" })).toBeInTheDocument();

    // 300 is the maximum scroll; anything within 20px of it is the end.
    track.scrollLeft = 285;
    fireEvent.scroll(track);
    expect(screen.queryByRole("button", { name: "Scroll featured right" })).toBeNull();
    expect(document.querySelector(".home-rail-fade")).toBeNull();
  });

  it("scrolls by most of a track width, forwards and back", () => {
    const { trackRef } = renderRail();
    const track = trackRef.current!;
    Object.defineProperty(track, "clientWidth", { value: 900, configurable: true });
    Object.defineProperty(track, "scrollWidth", { value: 2000, configurable: true });
    const scrollBy = vi.fn();
    (track as unknown as { scrollBy: unknown }).scrollBy = scrollBy;
    track.scrollLeft = 100;
    fireEvent.scroll(track);

    fireEvent.click(screen.getByRole("button", { name: "Scroll featured right" }));
    expect(scrollBy).toHaveBeenCalledWith({ left: 720, behavior: "smooth" });
    fireEvent.click(screen.getByRole("button", { name: "Scroll featured left" }));
    expect(scrollBy).toHaveBeenLastCalledWith({ left: -720, behavior: "smooth" });
  });

  it("makes only the FIRST poster eager and high-priority — it is the LCP element", () => {
    renderRail();
    const imgs = Array.from(document.querySelectorAll<HTMLImageElement>(".home-feat img.cover-img"));
    // Celeste has no artwork, so only two posters carry an <img>.
    expect(imgs).toHaveLength(2);
    expect(imgs[0].getAttribute("loading")).toBeNull();
    expect(imgs[0].getAttribute("fetchpriority")).toBe("high");
    expect(imgs[1].getAttribute("loading")).toBe("lazy");
    expect(imgs[1].getAttribute("fetchpriority")).toBeNull();
    // The artwork-less card falls back to the gradient + glyph.
    expect(
      document.querySelectorAll(".home-feat")[2].querySelector(".glyph")?.textContent,
    ).toBe("C");
  });

  it("skips a card whose app the client no longer holds", () => {
    renderRail({ appById: new Map([["a2", APPS.get("a2")!]]) });
    expect(document.querySelectorAll(".home-feat")).toHaveLength(1);
    expect(document.querySelector(".home-feat-name")?.textContent).toBe("Hades");
  });
});
