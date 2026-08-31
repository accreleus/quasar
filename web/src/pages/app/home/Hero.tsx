// The home hero band (v3 handoff §B "Hero + featured rail"): "Game / on" over
// the sweep, with the featured rail beside it.
//
// The hero is unmounted in the library view, not collapsed. The mock collapses
// `.hero-wrap` to `max-height: 0` on `body.view-lib`; a collapsed hero still
// holds focusable rail cards, which is an invisible keyboard trap, so the page
// renders it or does not (AppHomeNext). The visible result is the same, and the
// view swap has its own transition (`.home-view`).

import type { ReactNode } from "react";

export interface HeroProps {
  /** Whether a featured rail is being rendered beside the copy. Drives the
   *  grid (one column without it) and the subline's promise. */
  hasRail: boolean;
  /** The rail. */
  children?: ReactNode;
}

export function Hero({ hasRail, children }: HeroProps) {
  return (
    <div className="home-hero-wrap">
      {/* Decorative arc of brand light behind the hero — texture, not content. */}
      <div className="home-sweep" aria-hidden />
      <section className="home-hero" data-rail={hasRail ? "true" : "false"}>
        <div className="home-hero-copy">
          <h1>
            Game<br />
            <em>on</em>
          </h1>
          {/* The subline follows the rail: with no rail there is nothing to
              resume, so "resume where you left off" would be a promise the
              screen does not keep. */}
          <p className="home-hero-sub">
            {hasRail
              ? "Resume where you left off, or launch anything from your hosts."
              : "Launch anything from your hosts."}
          </p>
        </div>
        {children}
      </section>
    </div>
  );
}
