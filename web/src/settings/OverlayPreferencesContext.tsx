// Provider for the user's session-overlay preferences.
//
// Server-held and synced across devices, with a localStorage MIRROR keyed by
// user id. The mirror exists for exactly one reason: a session's first paint
// happens before any fetch can resolve, and rendering the default strip and
// then snapping to the user's actual choice is a visible flinch at the worst
// possible moment. The server value always wins once it arrives; the mirror is
// a cache, never an authority, and is never merged with the server's answer.

import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Outlet } from "react-router-dom";
import { getUIPreferences, patchUIPreferences } from "../api/me";
import { useAuth } from "../auth/context";
import {
  DEFAULT_OVERLAY_PREFERENCES,
  fromWire,
  toWire,
  type SessionOverlayPreferences,
} from "./overlayPreferences";

interface OverlayPreferencesValue {
  prefs: SessionOverlayPreferences;
  /** False until the server's answer has been applied. The UI uses this to
   *  disable the editor, never to hide the strip — a strip that appears late is
   *  worse than a strip that appears with cached values. */
  loaded: boolean;
  save: (next: SessionOverlayPreferences) => Promise<void>;
  error: string | null;
}

const Ctx = createContext<OverlayPreferencesValue | null>(null);

function cacheKey(userId: string | undefined): string | null {
  return userId ? `quasar.ui.overlay.${userId}` : null;
}

function readCache(userId: string | undefined): SessionOverlayPreferences {
  const key = cacheKey(userId);
  if (!key) return DEFAULT_OVERLAY_PREFERENCES;
  try {
    const raw = localStorage.getItem(key);
    return raw ? fromWire(JSON.parse(raw)) : DEFAULT_OVERLAY_PREFERENCES;
  } catch {
    return DEFAULT_OVERLAY_PREFERENCES;
  }
}

function writeCache(userId: string | undefined, prefs: SessionOverlayPreferences): void {
  const key = cacheKey(userId);
  if (!key) return;
  try {
    localStorage.setItem(key, JSON.stringify(toWire(prefs)));
  } catch {
    // Quota or private mode. The cache is an optimisation; losing it costs a
    // single frame of default strip, so there is nothing to report.
  }
}

export function OverlayPreferencesProvider({ children }: { children: ReactNode }) {
  const { token, user } = useAuth();
  const [prefs, setPrefs] = useState<SessionOverlayPreferences>(() => readCache(user?.id));
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Mirrors `prefs`, but updated SYNCHRONOUSLY at the call site of every
  // `setPrefs`, not just during render. React's own commit for a `setPrefs`
  // call can land after a later microtask runs (e.g. a rejected PATCH's catch
  // block), so a ref that only gets refreshed by the render body can still
  // read stale during that window. Writing it eagerly here makes "what did we
  // most recently apply" answerable without waiting on a commit.
  const prefsRef = useRef(prefs);

  const applyPrefs = useCallback(
    (next: SessionOverlayPreferences) => {
      prefsRef.current = next;
      setPrefs(next);
    },
    [],
  );

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    void (async () => {
      try {
        const wire = await getUIPreferences(token);
        if (cancelled) return;
        const next = fromWire(wire.session_overlay);
        applyPrefs(next);
        writeCache(user?.id, next);
      } catch {
        // Keep whatever the cache gave us. A preferences read failing must not
        // block the library or a live session.
      } finally {
        if (!cancelled) setLoaded(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [token, user?.id, applyPrefs]);

  const save = useCallback(
    async (next: SessionOverlayPreferences) => {
      if (!token) return;
      const previous = prefsRef.current;
      applyPrefs(next);
      writeCache(user?.id, next);
      setError(null);
      try {
        await patchUIPreferences(token, { session_overlay: toWire(next) });
      } catch (e: unknown) {
        // Saves can overlap (save-on-every-toggle). A failed save may roll
        // back only its own change, and only if no later save superseded it
        // (prefsRef.current !== next) — this guard looks redundant but is
        // load-bearing. The error still surfaces either way.
        if (prefsRef.current === next) {
          applyPrefs(previous);
          writeCache(user?.id, previous);
        }
        setError(e instanceof Error ? e.message : "could not save preferences");
      }
    },
    [token, user?.id, applyPrefs],
  );

  return <Ctx.Provider value={{ prefs, loaded, save, error }}>{children}</Ctx.Provider>;
}

/** Outside a provider this returns defaults rather than throwing: the overlay
 *  must render in any harness that mounts SessionPage without the full app
 *  shell, and a thrown error there would be a blank stream. */
export function useOverlayPreferences(): OverlayPreferencesValue {
  const ctx = useContext(Ctx);
  if (ctx) return ctx;
  return {
    prefs: DEFAULT_OVERLAY_PREFERENCES,
    loaded: true,
    save: async () => {},
    error: null,
  };
}

/** Layout-route wrapper: mounts the provider once for every authenticated
 *  route beneath it (Account settings AND the live session view) and renders
 *  the matched child via <Outlet/>. Kept in this file, not App.tsx, so the
 *  route tree only ever names the wrapper, not the provider's internals. */
export function OverlayPreferencesRoute() {
  return (
    <OverlayPreferencesProvider>
      <Outlet />
    </OverlayPreferencesProvider>
  );
}
