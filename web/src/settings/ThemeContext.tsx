/**
 * ThemeContext — manages [data-theme] on <html> and [data-density] on <body>.
 *
 * Persistence: localStorage['quasar-theme'] and localStorage['quasar-density'].
 * Default: dark theme, comfortable density.
 *
 * Dark-lock: pages that must always render dark (e.g. the session view, loading
 * screens) call useDarkLock() at mount. The lock is ref-counted so nested locks
 * are safe and auto-release on unmount.
 *
 * Preferred vs effective theme:
 *   preferredTheme — the user's persisted choice; ONLY written to localStorage.
 *   effectiveTheme — what actually renders: "dark" while any lock is held,
 *                    otherwise preferredTheme. Only effectiveTheme touches the DOM.
 *   Locking never clobbers preferredTheme or localStorage.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useState,
} from "react";

export type Theme = "dark" | "light";
export type Density = "comfortable" | "dense";
export type Rail = "expanded" | "collapsed";

interface ThemeContextValue {
  /** The effective (rendered) theme — dark while any lock is held. */
  theme: Theme;
  density: Density;
  /** Console rail width mode. Shared by /admin and /app/account — both
   *  render ConsoleShell, and the preference is one body-level attribute, so
   *  collapsing in one area collapses in the other (both rails carry the
   *  toggle, or the second one would be a dead end). */
  rail: Rail;
  setTheme: (t: Theme) => void;
  setDensity: (d: Density) => void;
  setRail: (r: Rail) => void;
  toggleTheme: () => void;
  toggleDensity: () => void;
  toggleRail: () => void;
  /** Acquire a dark lock. Returns a release function; call it on cleanup. */
  lockDark: () => () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

const THEME_KEY = "quasar-theme";
const DENSITY_KEY = "quasar-density";
const RAIL_KEY = "quasar-rail";

function readTheme(): Theme {
  try {
    const stored = localStorage.getItem(THEME_KEY);
    return stored === "light" ? "light" : "dark";
  } catch {
    return "dark";
  }
}

function readDensity(): Density {
  try {
    const stored = localStorage.getItem(DENSITY_KEY);
    return stored === "dense" ? "dense" : "comfortable";
  } catch {
    return "comfortable";
  }
}

function readRail(): Rail {
  try {
    const stored = localStorage.getItem(RAIL_KEY);
    return stored === "collapsed" ? "collapsed" : "expanded";
  } catch {
    return "expanded";
  }
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  // The user's persisted choice — the ONLY thing written to localStorage.
  const [preferredTheme, setPreferredTheme] = useState<Theme>(readTheme);
  const [density, setDensityState] = useState<Density>(readDensity);
  const [rail, setRailState] = useState<Rail>(readRail);
  // Ref-counted dark lock: >0 means some page requires dark regardless of preference.
  const [darkLockCount, setDarkLockCount] = useState(0);

  // Effective theme: locked pages always render dark.
  const effectiveTheme: Theme = darkLockCount > 0 ? "dark" : preferredTheme;

  // Apply effective theme to DOM. Never persists — persistence is handled separately.
  useEffect(() => {
    document.documentElement.setAttribute("data-theme", effectiveTheme);
  }, [effectiveTheme]);

  // Persist the user's preference whenever it changes. Never called by lockDark.
  useEffect(() => {
    try {
      localStorage.setItem(THEME_KEY, preferredTheme);
    } catch {
      // localStorage unavailable (private-browsing, quota) — ignore.
    }
  }, [preferredTheme]);

  // Apply density to DOM + persist.
  useEffect(() => {
    document.body.setAttribute("data-density", density);
    try {
      localStorage.setItem(DENSITY_KEY, density);
    } catch {
      // ignore
    }
  }, [density]);

  // Apply rail mode to DOM + persist. Set on <body> like density so the
  // --rail-w override in tokens.css reaches the shell's `.app` grid track.
  useEffect(() => {
    document.body.setAttribute("data-rail", rail);
    try {
      localStorage.setItem(RAIL_KEY, rail);
    } catch {
      // ignore
    }
  }, [rail]);

  const setTheme = useCallback((t: Theme) => {
    // Always updates the preference — the effective theme handles the lock overlay.
    setPreferredTheme(t);
  }, []);

  const setDensity = useCallback((d: Density) => {
    setDensityState(d);
  }, []);

  const toggleTheme = useCallback(() => {
    setPreferredTheme((p) => (p === "dark" ? "light" : "dark"));
  }, []);

  const toggleDensity = useCallback(() => {
    setDensityState((d) => (d === "comfortable" ? "dense" : "comfortable"));
  }, []);

  const setRail = useCallback((r: Rail) => {
    setRailState(r);
  }, []);

  const toggleRail = useCallback(() => {
    setRailState((r) => (r === "expanded" ? "collapsed" : "expanded"));
  }, []);

  const lockDark = useCallback((): (() => void) => {
    setDarkLockCount((n) => n + 1);
    return () => {
      setDarkLockCount((n) => Math.max(0, n - 1));
    };
  }, []);

  return (
    <ThemeContext.Provider
      value={{
        theme: effectiveTheme,
        density,
        rail,
        setTheme,
        setDensity,
        setRail,
        toggleTheme,
        toggleDensity,
        toggleRail,
        lockDark,
      }}
    >
      {children}
    </ThemeContext.Provider>
  );
}

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used within ThemeProvider");
  return ctx;
}

export interface DarkLockOptions {
  /**
   * Also set data-streaming="true" on <html> while the lock is held — a cheap
   * CSS signal to lighten backdrop-filter blur on HUD panels (blur(12px)
   * instead of blur(22px)) and cut per-frame GPU compositing cost over live
   * video. Default true, which is what every session/loading caller wants.
   *
   * The pre-auth card is dark for design reasons and has no video behind it,
   * so it passes false: nothing is streaming, and saying otherwise would put
   * the whole app's streaming CSS on a page that never plays a frame.
   */
  streaming?: boolean;
}

/**
 * Hook for pages that must always render dark (session view, loading screens,
 * the pre-auth card). Uses useLayoutEffect so the lock is applied synchronously
 * after DOM mutation and before the browser paints — prevents a single
 * light-theme frame flash. Releases automatically on unmount.
 */
export function useDarkLock({ streaming = true }: DarkLockOptions = {}) {
  const { lockDark } = useTheme();
  useLayoutEffect(() => {
    if (streaming) document.documentElement.setAttribute("data-streaming", "true");
    const release = lockDark();
    return () => {
      if (streaming) document.documentElement.removeAttribute("data-streaming");
      release();
    };
  }, [lockDark, streaming]);
}
