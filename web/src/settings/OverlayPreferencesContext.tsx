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
  /** The user opted back into the previous layout (`classic_layout`). Read it
   *  through `useNewDesign()`, not directly. */
  classicLayout: boolean;
  /** Optimistic; on a failed PATCH it reverts and REJECTS, so the caller can
   *  show its own toast. */
  setClassicLayout: (next: boolean) => Promise<void>;
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

// Same mirror idea for the layout choice, kept apart from the overlay entry so
// the overlay cache stays exactly the wire shape it was.
const classicKey = (userId: string | undefined) => (userId ? `quasar.ui.classic.${userId}` : null);

function readClassicCache(userId: string | undefined): boolean {
  const key = classicKey(userId);
  try {
    return key !== null && localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}

function writeClassicCache(userId: string | undefined, classic: boolean): void {
  const key = classicKey(userId);
  if (!key) return;
  try {
    localStorage.setItem(key, classic ? "1" : "0");
  } catch {
    // Same as writeCache: an optimisation, nothing to report.
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
  // Load-time behaviour: the new design is the default, so before the server
  // answers we render whatever this device last saw for this user (mirror),
  // else the new design. A user who has never chosen classic therefore never
  // sees the classic UI flash in. A classic user sees no flip on a device that
  // has already loaded once; on their very first load on a new device the new
  // design shows until the answer arrives, then it switches to classic.
  const [classicLayout, setClassicState] = useState(() => readClassicCache(user?.id));
  const classicRef = useRef(classicLayout);
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
        classicRef.current = wire.classic_layout === true;
        setClassicState(classicRef.current);
        writeClassicCache(user?.id, classicRef.current);
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

  const setClassicLayout = useCallback(
    async (next: boolean) => {
      if (!token) return;
      const previous = classicRef.current;
      classicRef.current = next;
      setClassicState(next);
      writeClassicCache(user?.id, next);
      try {
        // Only this key: the server merges at the top level, so session_overlay
        // and any other key are left alone.
        await patchUIPreferences(token, { classic_layout: next });
      } catch (e: unknown) {
        if (classicRef.current === next) {
          classicRef.current = previous;
          setClassicState(previous);
          writeClassicCache(user?.id, previous);
        }
        throw e;
      }
    },
    [token, user?.id],
  );

  return (
    <Ctx.Provider value={{ prefs, loaded, save, error, classicLayout, setClassicLayout }}>
      {children}
    </Ctx.Provider>
  );
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
    classicLayout: false,
    setClassicLayout: async () => {},
  };
}

/** True when the redesigned library, game page and account pages should render
 *  (the default); false when the user chose "Classic layout". Outside the
 *  provider (signed-out pages, admin, bare test harnesses) it is true. */
export function useNewDesign(): boolean {
  return !useOverlayPreferences().classicLayout;
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
