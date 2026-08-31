// Pure data logic for the landing page's featured rail + by-source grouping.
// React/DOM-free, same discipline as libraryGrid.ts.
//
// THE SERVER OWNS THE RAIL'S RANKING (protocol/control-api.md `GET
// /v1/me/highlights`, openapi.yaml HighlightList/Highlight/HighlightReason) —
// this module only presents it: `buildRailCards` renders `items` IN ORDER
// GIVEN (never sorts/pads/truncates) and never re-derives `reason`. The old
// client-side ranker (`buildFeaturedCards`) is gone — two rankers drifting
// apart is exactly what the endpoint forecloses.
//
// `Highlight` carries no name/artwork/kind/favourite; the client joins on
// `app_id` against `GET /v1/apps` for one source of truth. A highlight for an
// app the client doesn't hold (entitlement revoked between fetches) is
// skipped silently.
//
// Two mockup details are still not invented: the host name in a live kicker
// ("Session live · 24:18 ON HOST-01", "Ready ON HOST-02" — `Session` only
// carries `host_id` and `GET /v1/hosts` is admin-only, so every kicker and
// action line here ships without the host), and the elapsed clock when
// `session_started_at` is genuinely null for a pending/assigned session (falls
// back to the session row's `started_at ?? created_at`, else drops the clock
// rather than "NaN").
//
// Grouping the Library view by source moved to home/librarySources.ts.

import type { AppKind } from "../../api/types";
import { formatElapsedMinutes, formatRelativeTime } from "./libraryGrid";

// ── Featured rail ────────────────────────────────────────────────────────────

/** One item of `GET /v1/me/highlights`. `reason` is a plain `string`, not the
 * generated `HighlightReason` union — the contract documents it as wideable,
 * so a pinned client must degrade to a neutral kicker rather than fail to
 * compile (same reasoning as `GroupableApp.external_source` below). */
export interface RailHighlight {
  app_id: string;
  reason: string;
  session_id?: string | null;
  session_started_at?: string | null;
  play_seconds?: number;
  last_played_at?: string | null;
}

/** Minimal session shape — only a fallback clock source for a live highlight
 * whose `session_started_at` is null. Ordering/membership never read from it. */
export interface RailSession {
  id: string;
  created_at: string;
  started_at?: string | null;
}

/** Kicker colour, and with it the pulsing dot: `live` is the only variant
 *  that carries one. `neutral` is not in the mock — it is the fall-through for
 *  a `HighlightReason` a later amendment adds (see the default arm below). */
export type RailKickerVariant = "live" | "accent" | "info" | "neutral";

/** The small coloured line above the action ("Session live · 24 minutes"). */
export interface RailKicker {
  variant: RailKickerVariant;
  text: string;
}

export interface RailCard {
  appId: string;
  /** The server's `reason`, VERBATIM — including a value this client version
   *  does not know. Surfaced as `data-variant` for styling/telemetry. */
  reason: string;
  kicker: RailKicker;
  /** The card's display line, under the kicker. */
  action: string;
  /** Present only for a `live` card: the session to resume. */
  sessionId?: string;
}

export interface RailInput {
  /** `HighlightList.items`, in the server's order. */
  highlights: readonly RailHighlight[];
  /** Ids of the apps the client currently holds from `GET /v1/apps`. A
   *  highlight outside this set is dropped — see the header. */
  knownAppIds: ReadonlySet<string>;
  /** The caller's own sessions (`GET /v1/sessions`), used only for the
   *  live-clock fallback. Omit and a clock-less live card simply has no clock. */
  sessions?: readonly RailSession[];
  /** `kind` per app id, for the one card the mock words differently: a live
   *  desktop reads "Running / Jump to your desktop", not "Session live /
   *  Resume". The server's reason is `live` for both. Omit and every live card
   *  takes the game wording. */
  kinds?: ReadonlyMap<string, AppKind>;
  now?: number;
}

/** "1 h" / "11 h" / "45 min" — the `most_played` card's play total. */
function formatPlaySeconds(seconds: number): string {
  const minutes = seconds / 60;
  if (minutes < 60) return `${Math.max(0, Math.round(minutes))} min`;
  const hours = minutes / 60;
  const rounded = Math.round(hours * 10) / 10;
  return `${Number.isInteger(rounded) ? rounded.toFixed(0) : rounded.toFixed(1)} h`;
}

/** When the live card's clock starts. `session_started_at` is null for a
 * pending/assigned session (truthfully not started) — falls back to the
 * session's own `started_at ?? created_at`; null (no clock) if neither exists. */
function liveClockFrom(
  h: RailHighlight,
  sessionById: ReadonlyMap<string, RailSession>,
): string | null {
  if (h.session_started_at) return h.session_started_at;
  const s = h.session_id ? sessionById.get(h.session_id) : undefined;
  return s?.started_at ?? s?.created_at ?? null;
}

/** Maps the server's rail to renderable cards. ORDER IS PRESERVED VERBATIM —
 * no sort here, ever (would reintroduce the second ranker the endpoint
 * prevents). Empty result means the caller drops the rail and the hero goes
 * full width. */
export function buildRailCards(input: RailInput): RailCard[] {
  const { highlights, knownAppIds, sessions = [], kinds, now = Date.now() } = input;
  const sessionById = new Map(sessions.map((s) => [s.id, s]));
  const cards: RailCard[] = [];

  for (const h of highlights) {
    // Entitlement drift: rail ranked against a catalogue the client no longer
    // holds. Skip silently — no name or artwork to render with.
    if (!knownAppIds.has(h.app_id)) continue;

    const base = { appId: h.app_id, reason: h.reason };
    switch (h.reason) {
      case "live": {
        // A desktop is never "resumed", it is returned to — the mock gives it
        // its own two lines.
        if (kinds?.get(h.app_id) === "desktop") {
          cards.push({
            ...base,
            kicker: { variant: "live", text: "Running" },
            action: "Jump to your desktop",
            sessionId: h.session_id ?? undefined,
          });
          break;
        }
        const startedAt = liveClockFrom(h, sessionById);
        cards.push({
          ...base,
          kicker: {
            variant: "live",
            text: startedAt
              ? `Session live · ${formatElapsedMinutes(startedAt, now)}`
              : "Session live",
          },
          // The mock says "Hover to resume". Hovering is not an interaction a
          // touch or keyboard user has — the action line names the action.
          action: "Resume",
          sessionId: h.session_id ?? undefined,
        });
        break;
      }
      case "most_played":
        cards.push({
          ...base,
          kicker: { variant: "accent", text: "Most played this week" },
          // The server only labels an app most_played when it has play time,
          // but a 0 would read as a bug rather than a fact — so degrade.
          action: h.play_seconds
            ? `${formatPlaySeconds(h.play_seconds)} on your server`
            : "Ready to play",
        });
        break;
      case "resume":
        cards.push({
          ...base,
          kicker: { variant: "accent", text: "Pick up where you left off" },
          // `last_played_at` is MAX(ended_at) and is null when no session ever
          // reached a clean end — real, and not something to print around.
          action: h.last_played_at
            ? `Last played ${formatRelativeTime(h.last_played_at, now)}`
            : "Ready to play",
        });
        break;
      case "recently_added":
        cards.push({
          ...base,
          kicker: { variant: "info", text: "Newly added" },
          // The mock says "Ready on host-02"; no user-visible shape names a
          // host (see the header), so the card says only what it knows.
          action: "Ready to play",
        });
        break;
      default:
        // A reason from a later contract amendment (openapi.yaml
        // HighlightReason) — kept with a neutral kicker rather than dropped,
        // which would silently shrink a rail the server considers complete.
        // Action line differs from recently_added's so two adjacent neutral
        // cards don't read as a duplicate render.
        cards.push({
          ...base,
          kicker: { variant: "neutral", text: "In your library" },
          action: "Ready when you are",
        });
        break;
    }
  }

  return cards;
}
