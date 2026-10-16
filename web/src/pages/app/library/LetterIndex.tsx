// The A–Z index: a thin column in the grid's right gutter, stuck under the
// topbar, only while A–Z splits the list into letter sections. It marks the
// section a jump would show, jumps on click, and on touch a finger dragged along
// it scrubs. Rendered in LibraryGrid's `index` slot, between the head and the grids.

import { useEffect, useMemo, useRef, useState } from "react";
import type { KeyboardEvent, PointerEvent } from "react";
import type { App } from "../../../api/types";
import { LETTERS, letterOf, lettered, spyLetter } from "./organise";
import type { Organised } from "./useOrganised";

export function LetterIndex({ o, apps }: { o: Organised; apps: readonly App[] }) {
  const present = useMemo(() => new Set(apps.map((a) => letterOf(a.name))), [apps]);
  const on = lettered(o.sort, apps.length);
  const [current, setCurrent] = useState<string | null>(null);
  const [stop, setStop] = useState<string | null>(null);
  const jumped = useRef<string | null>(null);
  const scrubbing = useRef(false);

  useEffect(() => {
    if (!on) return;
    let raf = 0;
    const update = () => {
      raf = 0;
      const els = [...document.querySelectorAll<HTMLElement>("[data-letter-section]")];
      if (!els.length) return setCurrent(null);
      const edge = parseFloat(getComputedStyle(els[0]).scrollMarginTop) + 2;
      const atEnd = window.scrollY + window.innerHeight >= document.documentElement.scrollHeight - 2;
      if (!atEnd) jumped.current = null;
      const secs = els.map((el) => ({ letter: el.dataset.letterSection ?? "", top: el.getBoundingClientRect().top }));
      setCurrent(spyLetter(secs, edge, atEnd, jumped.current, window.innerHeight));
    };
    const queue = () => {
      if (!raf) raf = requestAnimationFrame(update);
    };
    update();
    window.addEventListener("scroll", queue, { passive: true });
    window.addEventListener("resize", queue);
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("scroll", queue);
      window.removeEventListener("resize", queue);
    };
  }, [on, apps]);

  if (!on) return null;

  const jump = (letter: string, byKey: boolean) => {
    const sec = document.querySelector<HTMLElement>(`[data-letter-section="${letter}"]`);
    if (!sec) return;
    jumped.current = letter;
    // Instant, like a contacts index: a jump is a jump, not a tour.
    sec.scrollIntoView({ block: "start" });
    // Next frame: the scroll has to land before the windowed grid mounts this section's tiles.
    if (byKey) requestAnimationFrame(() => sec.querySelector<HTMLElement>(".lib-tile-surface")?.focus({ preventScroll: true }));
  };

  // One Tab stop; Up/Down (and Left/Right) rove, Home/End go to the ends.
  const rove = (e: KeyboardEvent<HTMLElement>) => {
    const btns = [...e.currentTarget.querySelectorAll<HTMLButtonElement>("button:not(:disabled)")];
    const i = btns.indexOf(document.activeElement as HTMLButtonElement);
    const to = ({ ArrowDown: i + 1, ArrowRight: i + 1, ArrowUp: i - 1, ArrowLeft: i - 1, Home: 0, End: btns.length - 1 } as Record<string, number>)[e.key];
    if (to === undefined || i < 0) return;
    e.preventDefault();
    btns[(to + btns.length) % btns.length].focus();
  };

  const tabStop = [stop, current].find((x) => x && present.has(x)) ?? LETTERS.find((x) => present.has(x));

  return (
    <nav
      className="lib-index"
      aria-label="Jump to letter"
      onKeyDown={rove}
      onPointerDown={(e: PointerEvent<HTMLElement>) => {
        if (e.pointerType === "mouse") return;
        scrubbing.current = true;
        e.currentTarget.setPointerCapture(e.pointerId);
      }}
      onPointerMove={(e: PointerEvent<HTMLElement>) => {
        if (!scrubbing.current) return;
        const b = document.elementFromPoint(e.clientX, e.clientY)?.closest<HTMLButtonElement>(".lib-index button[data-letter]");
        const l = b && !b.disabled ? b.dataset.letter : null;
        if (l && l !== jumped.current) jump(l, false);
      }}
      onPointerUp={() => (scrubbing.current = false)}
      onPointerCancel={() => (scrubbing.current = false)}
    >
      {LETTERS.map((l) => (
        <button
          key={l}
          type="button"
          data-letter={l}
          disabled={!present.has(l)}
          aria-current={l === current ? "location" : undefined}
          tabIndex={l === tabStop ? 0 : -1}
          onFocus={() => setStop(l)}
          onBlur={() => setStop(null)}
          onClick={(e) => jump(l, e.detail === 0)}
        >
          {l}
        </button>
      ))}
    </nav>
  );
}
