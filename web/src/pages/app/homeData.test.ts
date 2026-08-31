// buildRailCards: the server's rail → renderable cards.
//
// The mapper is a join and a copy layer. It holds no ranking, and the first
// test here is the one that keeps it that way. The page's own test covers the
// page; this covers the mapping.

import { describe, expect, it } from "vitest";
import { buildRailCards, type RailHighlight } from "./homeData";

function highlight(
  appId: string,
  reason: string,
  over: Partial<RailHighlight> = {},
): RailHighlight {
  return {
    app_id: appId,
    reason,
    session_id: null,
    session_started_at: null,
    play_seconds: 0,
    last_played_at: null,
    ...over,
  };
}

describe("buildRailCards", () => {
  const now = Date.UTC(2026, 0, 2, 0, 0, 0);
  const known = new Set(["a1", "a2", "a3", "a4"]);

  it("preserves the server's order verbatim, however wrong it looks", () => {
    // recently_added first and live last — the exact inverse of the priority
    // the retired client ranker applied. Re-sorting here would be the second
    // ranker /v1/me/highlights exists to prevent.
    const cards = buildRailCards({
      highlights: [
        highlight("a3", "recently_added"),
        highlight("a2", "resume", { last_played_at: new Date(now - 7200_000).toISOString() }),
        highlight("a1", "live", {
          session_id: "s1",
          session_started_at: new Date(now - 600_000).toISOString(),
        }),
      ],
      knownAppIds: known,
      now,
    });
    expect(cards.map((c) => c.appId)).toEqual(["a3", "a2", "a1"]);
    expect(cards.map((c) => c.reason)).toEqual(["recently_added", "resume", "live"]);
  });

  it("maps each contract reason to its kicker variant and action line", () => {
    const cards = buildRailCards({
      highlights: [
        highlight("a1", "live", {
          session_id: "s1",
          session_started_at: new Date(now - 600_000).toISOString(),
        }),
        highlight("a2", "most_played", { play_seconds: 39_600 }),
        highlight("a3", "resume", { last_played_at: new Date(now - 7200_000).toISOString() }),
        highlight("a4", "recently_added"),
      ],
      knownAppIds: known,
      now,
    });
    expect(cards).toHaveLength(4);
    expect(cards[0]).toMatchObject({
      kicker: { variant: "live", text: "Session live · 10 minutes" },
      action: "Resume",
      sessionId: "s1",
    });
    expect(cards[1]).toMatchObject({
      kicker: { variant: "accent", text: "Most played this week" },
      action: "11 h on your server",
    });
    expect(cards[2]).toMatchObject({
      kicker: { variant: "accent", text: "Pick up where you left off" },
      action: "Last played 2 hours ago",
    });
    expect(cards[3]).toMatchObject({
      kicker: { variant: "info", text: "Newly added" },
      action: "Ready to play",
    });
    // Only the live card carries a session to resume.
    expect(cards.slice(1).every((c) => c.sessionId === undefined)).toBe(true);
  });

  it("gives a live DESKTOP the mock's own words, not a game's", () => {
    // The mock's fifth card. `kind` is the only thing that distinguishes it —
    // the server's reason is `live` for both.
    const cards = buildRailCards({
      highlights: [
        highlight("a1", "live", {
          session_id: "s1",
          session_started_at: new Date(now - 600_000).toISOString(),
        }),
      ],
      knownAppIds: known,
      kinds: new Map([["a1", "desktop"]]),
      now,
    });
    expect(cards[0]).toMatchObject({
      kicker: { variant: "live", text: "Running" },
      action: "Jump to your desktop",
      sessionId: "s1",
    });
  });

  it("keeps an unknown reason with a neutral kicker rather than dropping it", () => {
    // HighlightReason is documented as wideable by a later amendment.
    const cards = buildRailCards({
      highlights: [
        highlight("a1", "trending_on_your_host"),
        highlight("a2", "recently_added"),
      ],
      knownAppIds: known,
      now,
    });
    expect(cards).toHaveLength(2);
    expect(cards[0]).toMatchObject({
      appId: "a1",
      reason: "trending_on_your_host",
      kicker: { variant: "neutral", text: "In your library" },
      action: "Ready when you are",
    });
    // ...and not the same action line as the recently_added card it renders
    // next to. The two sort adjacently, so identical big display lines read as
    // a duplicate render — a designer-pass finding, pinned here so the copy
    // cannot quietly converge again.
    expect(cards[1].action).toBe("Ready to play");
    expect(cards[0].action).not.toBe(cards[1].action);
  });

  it("skips an app_id the client does not hold", () => {
    const cards = buildRailCards({
      highlights: [highlight("gone", "resume"), highlight("a1", "recently_added")],
      knownAppIds: known,
      now,
    });
    expect(cards.map((c) => c.appId)).toEqual(["a1"]);
  });

  it("returns nothing for an empty rail", () => {
    expect(buildRailCards({ highlights: [], knownAppIds: known, now })).toEqual([]);
  });

  it("falls back to the client's session row when session_started_at is null", () => {
    const cards = buildRailCards({
      highlights: [highlight("a1", "live", { session_id: "s1", session_started_at: null })],
      knownAppIds: known,
      sessions: [
        {
          id: "s1",
          started_at: null,
          created_at: new Date(now - 120_000).toISOString(),
        },
      ],
      now,
    });
    expect(cards[0].kicker.text).toBe("Session live · 2 minutes");
  });

  it("drops the clock rather than printing NaN when no timestamp exists at all", () => {
    const cards = buildRailCards({
      highlights: [highlight("a1", "live", { session_id: "s1", session_started_at: null })],
      knownAppIds: known,
      sessions: [],
      now,
    });
    expect(cards[0].kicker).toEqual({ variant: "live", text: "Session live" });
    expect(cards[0].sessionId).toBe("s1");
  });

  it("degrades rather than printing a zero play total or a missing last-played", () => {
    const cards = buildRailCards({
      highlights: [
        highlight("a1", "most_played", { play_seconds: 0 }),
        highlight("a2", "resume", { last_played_at: null }),
      ],
      knownAppIds: known,
      now,
    });
    expect(cards[0].action).toBe("Ready to play");
    expect(cards[1].action).toBe("Ready to play");
  });
});
