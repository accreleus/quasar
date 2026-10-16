// Shelf section 1 — Switch game (handoff §E.1). The quick-switch rail lives
// here.
//
// A swap keeps the WebRTC session and its stream alive — no reconnect or
// renegotiation, so the bar's identity and metrics survive unchanged. Pinned to
// this session's host with no placement step, so a tile whose library lives
// elsewhere is refused rather than relocated.
//
// This component's job stops at the request: it fires the POST and reports only
// accept/reject. Whether the swap succeeds is known only via agent-api's async
// session_state callbacks, handled entirely by the owner's useSwapTransition
// poll — reporting "done" from this request/response cycle is exactly the bug
// this shape prevents.

import { useEffect, useMemo, useState } from "react";
import { listApps, swapSession } from "../../../../api/library";
import { ApiError } from "../../../../api/client";
import type { App } from "../../../../api/types";
import { useAuth } from "../../../../auth/context";
import { artFor } from "../../../../lib/appArtwork";
import { appGlyph } from "../../../../lib/appGlyph";
import { coverClassAt } from "../../libraryGrid";

export interface GamesPaneProps {
  sessionId: string;
  currentAppId: string;
  /** True while a swap is in flight — disables every tile, since only one swap
   *  can run per session. The owner (useSwapTransition) is the single source of
   *  truth for this, same as `currentAppId`. */
  swapPending: boolean;
  /** Fired the instant a tile is clicked, before the request is even sent —
   *  this is what lets the owner hold the transition from the moment the user
   *  picks an app, not from the response. */
  onSwapStart: (target: { id: string; name: string }) => void;
  /** Fired only if the swap REQUEST was rejected (ownership/entitlement/
   *  host-mismatch, network failure) — terminal immediately, nothing to poll. */
  onSwapRejected: (message: string) => void;
}

/** Turn a swap failure into something a player can act on. The raw codes are
 *  precise and useless mid-session. */
function explain(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === "home_not_provisioned") {
      return "That app's library lives on another host. Exit and launch it from the library instead.";
    }
    if (e.code === "parent_app_disabled") return "That app has been disabled by an admin.";
    if (e.status === 403) return "You don't have access to that app.";
    if (e.status === 409) return "That app can't be swapped into this session right now.";
    return e.message;
  }
  return "Could not switch apps.";
}

export function GamesPane({
  sessionId,
  currentAppId,
  swapPending,
  onSwapStart,
  onSwapRejected,
}: GamesPaneProps) {
  const { token } = useAuth();
  const [apps, setApps] = useState<Array<Pick<App, "id" | "name" | "cover_url" | "hero_url">>>([]);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    void (async () => {
      try {
        const res = await listApps(token);
        if (!cancelled) {
          setApps(
            res.items.map((a) => ({
              id: a.id,
              name: a.name,
              cover_url: a.cover_url,
              hero_url: a.hero_url,
            })),
          );
        }
      } catch {
        // An empty rail is a fine degradation; the rest of the shelf works.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [token]);

  // Keyed off NAME-sorted order (libraryGrid.ts's `coverClassAt`, shared with
  // the library grid) — not the current-app-first display order below, so a
  // tile's colour never shifts as the running app changes.
  const coverClassById = useMemo(() => {
    const byName = [...apps].sort((a, b) => a.name.localeCompare(b.name));
    const m = new Map<string, string>();
    byName.forEach((a, i) => m.set(a.id, coverClassAt(i)));
    return m;
  }, [apps]);

  // Running app sorts to front (the mock's `.qs.current` is first in markup) so
  // it is visible without scrolling the rail. Stable sort.
  const displayApps = useMemo(
    () =>
      [...apps].sort((a, b) => {
        const ca = a.id === currentAppId ? 0 : 1;
        const cb = b.id === currentAppId ? 0 : 1;
        return ca - cb;
      }),
    [apps, currentAppId],
  );

  const swap = async (app: { id: string; name: string }) => {
    if (!token || swapPending) return;
    // Held from here, not the response — see the file header and useSwapTransition.
    onSwapStart({ id: app.id, name: app.name });
    try {
      await swapSession(token, sessionId, app.id);
      // Accepted: state_detail is now "swapping"; the owner's poll watches for
      // the real outcome.
    } catch (e: unknown) {
      onSwapRejected(explain(e));
    }
  };

  return (
    <>
      <div className="pane-head">
        <h3>Switch game</h3>
        {/* "As it is now", not "as launched": with the adaptive resolution
            ladder the stream may already sit below its launch size, and a swap
            keeps whatever it is on. */}
        <p>
          Switching keeps this session and its stream alive. No reconnect, and the stream
          resolution stays as it is now.
        </p>
      </div>
      <div className="qs-rail">
        {displayApps.map((a) => {
          const current = a.id === currentAppId;
          const coverClass = coverClassById.get(a.id) ?? coverClassAt(0);
          // Mirrors the library grid: real artwork or the derived gradient
          // (artFor — the same helper, not a second lookup).
          const tileArt = artFor(a, "tile");
          return (
            <button
              key={a.id}
              type="button"
              className={`qs${current ? " current" : ""}`}
              onClick={() => void swap(a)}
              disabled={current || swapPending}
              aria-label={a.name}
            >
              <span className={`cv ${coverClass}`} aria-hidden="true">
                {tileArt ? (
                  <img src={tileArt} alt="" className="cover-img" loading="lazy" decoding="async" />
                ) : (
                  <span className="glyph">{appGlyph(a.name)}</span>
                )}
              </span>
              {current && <span className="qs-badge">PLAYING</span>}
              <span className="nm">{a.name}</span>
            </button>
          );
        })}
      </div>
    </>
  );
}
