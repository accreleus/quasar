import { useId } from "react";

/**
 * Shared Quasar brand mark SVG. Parameterised by size and optional className.
 *
 * The gradient is in objectBoundingBox units (the SVG default), so its coordinates
 * must stay inside 0..1 — each shape then gets the full #6A45F5 -> #5B6BFF -> #00C0FF
 * sweep across its own box, matching how `--brand-grad` is applied everywhere else.
 * The gradient id is per-instance so two marks on one page can't collide.
 */
export function QuasarMark({ size = 26, className }: { size?: number; className?: string }) {
  // useId() emits colons (":r3:"), which are legal in an id but hostile inside a
  // functional-IRI and unusable with querySelector — strip them.
  const gradId = `qg-${useId().replace(/:/g, "")}`;
  return (
    <svg
      className={className}
      viewBox="0 0 32 32"
      fill="none"
      width={size}
      height={size}
      aria-hidden="true"
    >
      <defs>
        <linearGradient id={gradId} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#6A45F5" />
          <stop offset=".5" stopColor="#5B6BFF" />
          <stop offset="1" stopColor="#00C0FF" />
        </linearGradient>
      </defs>
      <ellipse
        cx="16"
        cy="16"
        rx="14.2"
        ry="5.4"
        transform="rotate(-30 16 16)"
        stroke={`url(#${gradId})`}
        strokeWidth="1.6"
        opacity=".75"
      />
      <circle cx="16" cy="16" r="6.2" fill={`url(#${gradId})`} />
      <circle cx="16" cy="16" r="2.1" fill="#fff" />
    </svg>
  );
}
