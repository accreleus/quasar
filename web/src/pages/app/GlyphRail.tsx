/**
 * The launch loader's four-step progress rail (handoff-v3 §D).
 *
 * Painted from one step index, like the mock: the headline word and the rail
 * read the same number, so they can never disagree about where the handshake
 * is. A completed step keeps its own glyph and gains a ring — four identical
 * ticks would erase which phase finished.
 *
 * The glyphs are the mock's verbatim (`glyph-network`, a bespoke padlock, the
 * play-with-arcs, `glyph-gamepad`); the class names on their parts
 * (`.arc`, `.shackle`, `.play`, `.key`) are what SessionLoader.css animates
 * while a step is active.
 */

import { Fragment } from "react";

type StepState = "idle" | "active" | "done";

function NetworkGlyph() {
  return (
    <svg className="glyph" viewBox="0 0 24 24" aria-hidden="true">
      <circle className="arc" cx="12" cy="5" r="2" />
      <circle className="arc" cx="5" cy="19" r="2" />
      <circle className="arc" cx="19" cy="19" r="2" />
      <path d="M12 7v4M5 17v-2a4 4 0 0 1 4-4h6a4 4 0 0 1 4 4v2" />
    </svg>
  );
}

function LockGlyph() {
  return (
    <svg className="glyph" viewBox="0 0 24 24" aria-hidden="true">
      <rect x="5.4" y="10.6" width="13.2" height="9" rx="2" />
      <path className="shackle" d="M8.6 10.6V8.2a3.4 3.4 0 0 1 6.8 0v2.4" />
    </svg>
  );
}

function StreamGlyph() {
  return (
    <svg className="glyph" viewBox="0 0 24 24" aria-hidden="true">
      <path className="play" d="m10 8 6 4-6 4z" />
      <path className="arc" d="M5.6 6.1a8 8 0 0 0 0 11.8" />
      <path className="arc" d="M18.4 6.1a8 8 0 0 1 0 11.8" />
    </svg>
  );
}

function GamepadGlyph() {
  return (
    <svg className="glyph" viewBox="0 0 24 24" aria-hidden="true">
      <path d="M8.5 8h7a5 5 0 0 1 4.8 3.6l1.1 3.8a3 3 0 0 1-5.1 2.8L14.5 16h-5l-1.8 2.2a3 3 0 0 1-5.1-2.8l1.1-3.8A5 5 0 0 1 8.5 8Z" />
      <path className="key" d="M7 11v4M5 13h4" />
      <path className="key key-b" d="M16.5 12h.01M18.5 14h.01" />
    </svg>
  );
}

/** The rail's four steps, in order. The labels are screen-reader only: the
 *  headline already names the live phase in full. */
export const RAIL_STEPS = [
  { label: "Signalling", Glyph: NetworkGlyph },
  { label: "Secure path", Glyph: LockGlyph },
  { label: "Video channel", Glyph: StreamGlyph },
  { label: "Input capture", Glyph: GamepadGlyph },
] as const;

function stateFor(index: number, step: number): StepState {
  if (index < step) return "done";
  return index === step ? "active" : "idle";
}

export function GlyphRail({ step }: { step: number }) {
  return (
    <ol className="sl-rail" aria-label="Connection progress">
      {RAIL_STEPS.map(({ label, Glyph }, i) => (
        // The connector is a list item too — the mock uses a bare <span>, which
        // is not valid inside an <ol>; `role="presentation"` keeps it out of the
        // list for assistive tech while leaving it a flex sibling.
        <Fragment key={label}>
          {i > 0 && (
            <li
              className="sl-link"
              data-state={stateFor(i - 1, step)}
              role="presentation"
              aria-hidden="true"
            />
          )}
          <li className="sl-step" data-state={stateFor(i, step)}>
            <Glyph />
            <span className="sr-only">{label}</span>
          </li>
        </Fragment>
      ))}
    </ol>
  );
}
