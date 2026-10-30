// After a setting is chosen, bring it on screen clear of the top bar (and the
// account page's sticky section bar), move focus to it, and light it briefly.
// The page may still be loading, so wait until the target exists and has stopped
// moving before scrolling to it.

import { useEffect } from "react";
import { useLocation } from "react-router-dom";
import type { Target } from "./settingsIndex";
import "../../../styles/settings-search.css";

const CONTROL = "input, select, textarea, button, a[href]";
const FLASH_MS = 1400;

function find(t: Target): HTMLElement | null {
  const all = [...document.querySelectorAll<HTMLElement>(t.sel)];
  return (t.text ? all.find((e) => e.textContent?.trim() === t.text) : all[0]) ?? null;
}

function scrollerOf(el: HTMLElement): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const oy = getComputedStyle(p).overflowY;
    if ((oy === "auto" || oy === "scroll") && p.scrollHeight > p.clientHeight) return p;
  }
  return null;
}

/** Height of whatever sits over the top of the scroller: the topbar when the
 *  document scrolls under it, and any sticky bar inside the scroller. */
function coveredTop(el: HTMLElement): number {
  const scroller = scrollerOf(el);
  const top = Math.max(0, scroller?.getBoundingClientRect().top ?? 0);
  const bar = document.querySelector<HTMLElement>(".topbar");
  let covered = bar ? Math.max(0, bar.getBoundingClientRect().bottom - top) : 0;
  for (const s of scroller?.querySelectorAll<HTMLElement>(".os-bar") ?? []) {
    const cs = getComputedStyle(s);
    if (cs.position === "sticky") covered += Math.max(0, (parseFloat(cs.top) || 0) + s.offsetHeight);
  }
  return covered;
}

function reveal(t: Target, match: HTMLElement): void {
  const row = (t.up && match.closest<HTMLElement>(t.up)) || match;
  const reduce = typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  // Runtime value: the covered height is only known now.
  const covered = coveredTop(row);
  row.style.scrollMarginTop = `${covered + 16}px`;
  const room = window.innerHeight - covered;
  row.scrollIntoView({ block: row.offsetHeight < room / 2 ? "center" : "start", behavior: reduce ? "auto" : "smooth" });

  // A control is focused itself; a group of controls hands focus to its chosen
  // option; a section or a device row takes focus as a whole.
  const focusEl = match.matches(CONTROL)
    ? match
    : t.up
      ? (row.querySelector<HTMLElement>('[aria-checked="true"], [aria-pressed="true"], [aria-selected="true"]') ?? row.querySelector<HTMLElement>(CONTROL))
      : null;
  if (focusEl) focusEl.focus({ preventScroll: true });
  else {
    if (!row.hasAttribute("tabindex")) row.setAttribute("tabindex", "-1");
    row.focus({ preventScroll: true });
  }

  row.classList.add("ss-hit", "ss-flash");
  window.setTimeout(() => row.classList.remove("ss-flash"), FLASH_MS);
  window.setTimeout(() => row.classList.remove("ss-hit"), FLASH_MS + 400);
}

interface Pending {
  stop: () => void;
  /** Router location key when the wait began. */
  from: string;
  /** Key of the page the reveal is for; null until the navigation it started lands. */
  at: string | null;
}
let pending: Pending | null = null;

/** Call right after navigating (or not, when already there). `from` is the
 *  location key at the call and `moves` whether a navigation was just started.
 *  Any navigation after that one (see useCancelRevealOnNavigation), or another
 *  reveal, ends the wait; a target that never appears ends it without a trace. */
export function revealWhenReady(t: Target, from: string, moves: boolean): void {
  pending?.stop();
  let timer = 0;
  const me: Pending = {
    stop: () => {
      window.clearTimeout(timer);
      if (pending === me) pending = null;
    },
    from,
    at: moves ? null : from,
  };
  pending = me;
  const start = performance.now();
  let lastTop = NaN;
  let still = 0;
  const done = (el: HTMLElement) => {
    me.stop();
    reveal(t, el);
  };
  const tick = () => {
    const el = find(t);
    // A disabled control cannot take focus: wait for it (a save in flight, a list loading).
    if (el && !(el as HTMLInputElement).disabled) {
      const top = el.getBoundingClientRect().top;
      still = Math.abs(top - lastTop) < 1 ? still + 1 : 0;
      lastTop = top;
      // ~180 ms without moving: the sections above it have loaded.
      if (still >= 3) return done(el);
    }
    if (performance.now() - start < 4000) timer = window.setTimeout(tick, 60);
    else if (el) done(el);
    else me.stop();
  };
  timer = window.setTimeout(tick, 0);
}

/** Ends a wait still in progress, if any (the test setup calls it after every test). */
export const cancelReveal = (): void => pending?.stop();

/** Mounted where the palette is: the first location change after the reveal's own
 *  navigation (Back, a link, another choice) cancels a wait still in progress. */
export function useCancelRevealOnNavigation(): void {
  const { key } = useLocation();
  useEffect(() => {
    if (!pending) return;
    if (pending.at === null) {
      if (key !== pending.from) pending.at = key;
    } else if (key !== pending.at) pending.stop();
  }, [key]);
}
