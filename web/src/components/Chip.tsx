/**
 * Chip — status badge / role tag.
 * Matches the `.chip` + variant classes from the design system.
 */
import type { ReactNode } from "react";

export type ChipVariant = "success" | "warning" | "danger" | "info" | "accent" | "neutral";

interface ChipProps {
  variant?: ChipVariant;
  /** Show a filled dot before the label (used for "Online", "Running") */
  dot?: boolean;
  /** Native tooltip. A chip's label is a fixed vocabulary term; free text that
   *  would blow the chip's width (e.g. a session's state_detail) belongs here. */
  title?: string;
  children: ReactNode;
  className?: string;
}

export function Chip({ variant, dot, title, children, className }: ChipProps) {
  const classes = [
    "chip",
    variant ? `chip-${variant}` : "",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <span className={classes} title={title}>
      {dot && <span className="dot" aria-hidden="true" />}
      {children}
    </span>
  );
}

/* ------------------------------------------------------------------ */
/* LiveDot — pulsing green dot for "live" indicator                    */
/* ------------------------------------------------------------------ */

interface LiveDotProps {
  label?: string;
}

export function LiveDot({ label = "Live" }: LiveDotProps) {
  return (
    <span className="row gap2" style={{ color: "var(--text-2)", fontSize: "var(--t-sm)", display: "inline-flex", alignItems: "center", gap: "var(--s2)" }}>
      <span className="live-dot" aria-hidden="true" />
      {label}
    </span>
  );
}
