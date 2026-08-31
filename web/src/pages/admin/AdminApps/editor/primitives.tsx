// The three presentational pieces every tab renders: a section, a fact row, and
// the gradient frame that stands in for missing artwork.

import type { ReactNode } from "react";
import { appGlyph } from "../../../../lib/appGlyph";

/** One `.fsec`: the section's title and description beside its fields. */
export function Section({
  title,
  desc,
  children,
}: {
  title: string;
  desc: string;
  children: ReactNode;
}) {
  return (
    <div className="fsec">
      <div className="fs-label">
        <h4>{title}</h4>
        <p>{desc}</p>
      </div>
      <div className="fs-fields">{children}</div>
    </div>
  );
}

/** One `.ae-fact` row: a label and its value. */
export function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="ae-fact">
      <span>{label}</span>
      <span>{children}</span>
    </div>
  );
}

/** The gradient frame both crops and the rail hero use. `url` renders over it. */
export function AppFrame({
  name,
  url,
  variant,
  flush,
}: {
  name: string;
  url?: string | null;
  variant: "tile" | "hero";
  flush?: boolean;
}) {
  return (
    <div className={`ae-frame ${variant}${flush ? " flush" : ""}`}>
      {url ? (
        <img src={url} alt="" className="cover-img" loading="lazy" decoding="async" />
      ) : (
        appGlyph(name)
      )}
    </div>
  );
}
