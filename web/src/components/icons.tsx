/**
 * Shared icon module — every glyph whose path data appeared ≥2 times across
 * `web/src` (or was an ad-hoc local `Icon*` page component), deduped into one
 * definition each. See the Step-1 inventory in the commit body that added
 * this file for the full duplicate-site accounting.
 *
 * Convention: 16×16 viewBox, stroke=currentColor, strokeWidth 1.5, className
 * "ic" by default, aria-hidden unless a label is given — `iconAttrs` below.
 * A few glyphs (non-16×16 viewBox, filled/no-stroke badges, a different
 * default strokeWidth) override specific fields via a trailing JSX prop
 * after the `iconAttrs(props)` spread; callers can still override further
 * via their own props since `iconAttrs` folds `...rest` in last.
 */
import type { JSX, SVGProps } from "react";

/** Every icon: 16×16 viewBox, stroke=currentColor, strokeWidth 1.5,
 *  className "ic" by default, aria-hidden unless a label is given. */
type IconProps = SVGProps<SVGSVGElement> & { label?: string };

function iconAttrs({ label, className, ...rest }: IconProps) {
  return {
    className: className ?? "ic",
    viewBox: "0 0 16 16",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.5,
    ...(label ? { role: "img" as const, "aria-label": label } : { "aria-hidden": true }),
    ...rest,
  };
}

/** Variant for filled (no-stroke-inheritance) glyphs — a bare stroke default
 * would otherwise leak an unwanted outline onto un-stroked children (dots,
 * badge circles) via SVG presentation-attribute inheritance. */
function iconAttrsFilled({ label, className, ...rest }: IconProps) {
  return {
    className: className ?? "ic",
    viewBox: "0 0 16 16",
    fill: "currentColor",
    ...(label ? { role: "img" as const, "aria-label": label } : { "aria-hidden": true }),
    ...rest,
  };
}

/** Variant for outline glyphs whose children own their own fill/stroke
 * (status badges) — no stroke default, same inheritance-leak reason. */
function iconAttrsPlain({ label, className, ...rest }: IconProps) {
  return {
    className: className ?? "ic",
    viewBox: "0 0 16 16",
    fill: "none",
    ...(label ? { role: "img" as const, "aria-label": label } : { "aria-hidden": true }),
    ...rest,
  };
}

// ── Close / search / chevrons ────────────────────────────────────────────────

export function IconClose(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M4 4l8 8M12 4l-8 8" strokeLinecap="round" />
    </svg>
  );
}

export function IconSearch(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <circle cx="7" cy="7" r="5" />
      <path d="M11 11l3 3" strokeLinecap="round" />
    </svg>
  );
}

/** Diagonal-cross "remove" glyph — distinct path data from IconClose. */
export function IconCross(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M5 5l6 6M11 5l-6 6" strokeLinecap="round" />
    </svg>
  );
}

export function IconChevronLeft(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M10 3L5 8l5 5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function IconChevronRight(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M6 3l5 5-5 5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Small down chevron — dropdown/disclosure affordance. */
export function IconChevronDown(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} strokeWidth={props.strokeWidth ?? 1.6}>
      <path d="M4 6l4 4 4-4" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function IconPlus(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} strokeWidth={props.strokeWidth ?? 1.6}>
      <path d="M8 3v10M3 8h10" strokeLinecap="round" />
    </svg>
  );
}

/** The v3 mock's `icon('refresh')` — an open circle with an arrow head. */
export function IconRefresh(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path
        d="M13.5 8a5.5 5.5 0 1 1-1.7-4M13.5 2v3.6H10"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

/** Download glyph (mockup `icon('download')`) — used by client-side CSV
 *  export actions (users, audit log). */
export function IconDownload(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M8 2.6v7.6M4.8 7.4L8 10.6l3.2-3.2" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M2.8 12.8h10.4" strokeLinecap="round" />
    </svg>
  );
}

// ── Info / warning / trash ───────────────────────────────────────────────────

export function IconInfo(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <circle cx="8" cy="8" r="6.5" />
      <path d="M8 7.2v4M8 5.2v.2" strokeLinecap="round" />
    </svg>
  );
}

export function IconWarning(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M8 2L14.6 13.5H1.4L8 2Z" strokeLinejoin="round" />
      <path d="M8 6.5v3M8 11.3v.1" strokeLinecap="round" />
    </svg>
  );
}

export function IconEdit(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M11.5 2.5l2 2L6 12l-2.5.5L4 10z" strokeLinejoin="round" />
    </svg>
  );
}

export function IconTrash(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M3.5 4.5h9M6.5 4.5V3h3v1.5M5 4.5l.5 8h5l.5-8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** Pin glyph (mockup `icon('pin')`) — image-catalog pin/unpin. */
export function IconPin(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M6.3 2.4h3.4l-.5 4 2.2 2.3H4.6l2.2-2.3z" strokeLinejoin="round" />
      <path d="M8 8.7v4.9" strokeLinecap="round" />
    </svg>
  );
}

/** Closed padlock. */
export function IconLock(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <rect x="3.4" y="6.9" width="9.2" height="6.7" rx="1.4" />
      <path d="M5.6 6.9V5.2a2.4 2.4 0 0 1 4.8 0v1.7" strokeLinecap="round" />
    </svg>
  );
}

// ── Copy / download / check (audit log — Task 28) ────────────────────────────

export function IconCopy(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <rect x="5.4" y="5.4" width="8.2" height="8.2" rx="1.6" />
      <path
        d="M10.6 5.4V4a1.6 1.6 0 0 0-1.6-1.6H4a1.6 1.6 0 0 0-1.6 1.6v5a1.6 1.6 0 0 0 1.6 1.6h1.4"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function IconCheck(props: IconProps) {
  return (
    <svg {...iconAttrs(props)}>
      <path d="M3.2 8.4l3 3 6.6-7" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

// ── Ellipsis / play / heart / sliders ────────────────────────────────────────

/** Horizontal ellipsis / kebab-menu trigger — filled dots, no stroke. */
export function IconMore(props: IconProps) {
  return (
    <svg {...iconAttrsFilled(props)}>
      <circle cx="3" cy="8" r="1.5" />
      <circle cx="8" cy="8" r="1.5" />
      <circle cx="13" cy="8" r="1.5" />
    </svg>
  );
}

/** Filled play triangle — also imported by AppHomeNext.tsx via
 * pages/app/libraryDetail.tsx's re-export (same glyph, one definition). */
export function IconPlayGlyph(props: IconProps) {
  return (
    <svg {...iconAttrsFilled(props)}>
      <path d="M4.5 3.2v9.6l8-4.8z" />
    </svg>
  );
}

export function IconHeart({ filled, ...props }: IconProps & { filled?: boolean }) {
  return (
    <svg {...iconAttrs(props)} fill={filled ? "currentColor" : "none"}>
      <path
        d="M8 13.5S2.2 10.2 2.2 6.4A3 3 0 0 1 8 5.1a3 3 0 0 1 5.8 1.3c0 3.8-5.8 7.1-5.8 7.1z"
        strokeLinejoin="round"
      />
    </svg>
  );
}

/** The launch card's "Adjust" affordance — three sliders, ported verbatim
 * from design_mockups/library-expanded-hero.html's `.lc-edit svg`. */
export function IconSliders(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} viewBox={props.viewBox ?? "0 0 24 24"} strokeWidth={props.strokeWidth ?? 2} strokeLinecap="round">
      <path d="M4 6h10M18 6h2M4 12h2M10 12h10M4 18h13M21 18h-1" />
      <circle cx="16" cy="6" r="2" />
      <circle cx="8" cy="12" r="2" />
      <circle cx="19" cy="18" r="2" />
    </svg>
  );
}

// ── Mic / gamepad / swap ─────────────────────────────────────────────────────

export function IconMic(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} viewBox={props.viewBox ?? "0 0 18 18"} strokeLinecap="round" strokeLinejoin="round">
      <rect x="6.6" y="1.9" width="4.8" height="8.6" rx="2.4" />
      <path d="M4 8.4v.6a5 5 0 0 0 10 0v-.6" />
      <path d="M9 14v2.1" />
    </svg>
  );
}

export function IconMicOff(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} viewBox={props.viewBox ?? "0 0 18 18"} strokeLinecap="round" strokeLinejoin="round">
      <rect x="6.6" y="1.9" width="4.8" height="8.6" rx="2.4" />
      <path d="M4 8.4v.6a5 5 0 0 0 10 0v-.6" />
      <path d="M9 14v2.1" />
      <path d="M2.6 2.6l12.8 12.8" />
    </svg>
  );
}

/** Gamepad / "capture input" glyph — was exported as `CaptureIcon` from
 * SessionDrawerInput.tsx and reused by SessionDrawer.tsx; SessionStrip.tsx
 * carried a second, drifted-free inline copy. One definition now. */
export function IconGamepad(props: IconProps) {
  return (
    <svg
      {...iconAttrs(props)}
      viewBox={props.viewBox ?? "0 0 24 24"}
      strokeWidth={props.strokeWidth ?? 1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M7 8h10a4.5 4.5 0 0 1 4.4 5.5l-.7 3a2 2 0 0 1-3.6.6L15.5 15h-7l-1.6 2.1a2 2 0 0 1-3.6-.6l-.7-3A4.5 4.5 0 0 1 7 8Z" />
      <line x1="7" y1="11" x2="7" y2="13" />
      <line x1="6" y1="12" x2="8" y2="12" />
      <circle cx="16" cy="11.4" r=".55" fill="currentColor" />
      <circle cx="17.6" cy="13" r=".55" fill="currentColor" />
    </svg>
  );
}

/** Swap/exit glyph — an inward bracket + outward-pointing arrow. */
export function IconSwap(props: IconProps) {
  return (
    <svg {...iconAttrs(props)} strokeWidth={props.strokeWidth ?? 1.4}>
      <path d="M6 2.5H3.5v11H6M10 4.5L13.5 8 10 11.5M13.5 8H6.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

// ── Status badges (toast) ────────────────────────────────────────────────────

export function IconStatusSuccess(props: IconProps) {
  return (
    <svg {...iconAttrsPlain(props)}>
      <circle cx="8" cy="8" r="8" fill="var(--success)" />
      <path d="M4.5 8.2l2.2 2.2 4-4.4" stroke="#08080c" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function IconStatusDanger(props: IconProps) {
  return (
    <svg {...iconAttrsPlain(props)}>
      <circle cx="8" cy="8" r="8" fill="var(--danger)" />
      <path d="M8 4.4v4.2M8 11.2v.2" stroke="#08080c" strokeWidth={1.8} strokeLinecap="round" />
    </svg>
  );
}

// ── Shell glyph registry (rail, user menu, command palette) ──────────────────
//
// The v3 shell addresses its glyphs by name: `adminNav.ts`/`accountNav.ts`
// describe the IA as data (`icon: "fleet"`), and Rail/CommandPalette turn a
// name into a glyph. That keeps the nav definitions component-free and
// testable, and it is the same indirection the mock uses (`I{}` +
// `icon(name)` in design_handoff_v3/screens/assets/ui.js) — the path data
// below is that map, transcribed. 16x16, stroke 1.5, aria-hidden, per the
// module convention above.

/** Every glyph the shell can name. Mirrors the mock's `I{}` keys. */
export type IconName =
  | "overview"
  | "sessions"
  | "fleet"
  | "library"
  | "streaming"
  | "people"
  | "audit"
  | "settings"
  | "storage"
  | "image"
  | "profile"
  | "overlay"
  | "devices"
  | "lock"
  | "collapse"
  | "sun"
  | "moon"
  | "search"
  | "plus"
  | "home"
  | "admin"
  | "signout"
  | "density";

/** Path data only — the <svg> wrapper is shared by <ShellIcon>. */
const SHELL_GLYPHS: Record<IconName, JSX.Element> = {
  overview: <path d="M2.5 3.5h5v4h-5zM2.5 10h5v3h-5zM9.5 3.5h4v3h-4zM9.5 9h4v4h-4z" />,
  sessions: (
    <path d="M1.5 8h3l2-4.5L9 12.5l1.8-4.5h3.7" strokeLinecap="round" strokeLinejoin="round" />
  ),
  fleet: (
    <>
      <rect x="2" y="2.5" width="12" height="4.5" rx="1.2" />
      <rect x="2" y="9" width="12" height="4.5" rx="1.2" />
      <path d="M4.6 4.75h.01M4.6 11.25h.01" strokeLinecap="round" />
    </>
  ),
  library: (
    <>
      <rect x="2" y="2.5" width="5" height="5" rx="1" />
      <rect x="9" y="2.5" width="5" height="5" rx="1" />
      <rect x="2" y="9" width="5" height="5" rx="1" />
      <rect x="9" y="9" width="5" height="5" rx="1" />
    </>
  ),
  streaming: (
    <>
      <path d="M2 11.5V8a6 6 0 0 1 12 0v3.5" />
      <rect x="1.5" y="9.5" width="3" height="4.5" rx="1.2" />
      <rect x="11.5" y="9.5" width="3" height="4.5" rx="1.2" />
    </>
  ),
  people: (
    <>
      <circle cx="6.2" cy="5.6" r="2.4" />
      <path d="M1.8 13.2a4.4 4.4 0 0 1 8.8 0" strokeLinecap="round" />
      <path d="M10.8 3.6a2.4 2.4 0 0 1 0 4.6M11.6 13.2a4.4 4.4 0 0 0-1.1-2.9" strokeLinecap="round" />
    </>
  ),
  audit: (
    <>
      <rect x="3" y="1.8" width="10" height="12.4" rx="1.6" />
      <path d="M5.6 5.4h4.8M5.6 8h4.8M5.6 10.6h2.8" strokeLinecap="round" />
    </>
  ),
  settings: (
    <>
      <circle cx="8" cy="8" r="2.2" />
      <path
        d="M8 1.6v1.6M8 12.8v1.6M1.6 8h1.6M12.8 8h1.6M3.5 3.5l1.1 1.1M11.4 11.4l1.1 1.1M12.5 3.5l-1.1 1.1M4.6 11.4l-1.1 1.1"
        strokeLinecap="round"
      />
    </>
  ),
  storage: (
    <>
      <ellipse cx="8" cy="4" rx="5.5" ry="2.2" />
      <path d="M2.5 4v8c0 1.2 2.5 2.2 5.5 2.2s5.5-1 5.5-2.2V4" />
      <path d="M2.5 8c0 1.2 2.5 2.2 5.5 2.2s5.5-1 5.5-2.2" />
    </>
  ),
  image: (
    <>
      <rect x="2" y="3" width="12" height="10" rx="1.6" />
      <circle cx="5.8" cy="6.6" r="1.1" />
      <path d="M2.4 11.4l3.4-3 3 2.4 2.2-1.8 2.6 2.2" strokeLinecap="round" strokeLinejoin="round" />
    </>
  ),
  profile: (
    <>
      <circle cx="8" cy="5.6" r="2.6" />
      <path d="M2.8 13.5a5.2 5.2 0 0 1 10.4 0" strokeLinecap="round" />
    </>
  ),
  overlay: (
    <>
      <rect x="2" y="2.5" width="12" height="8.5" rx="1.5" />
      <rect x="8.5" y="4.3" width="4" height="2.6" rx=".6" />
    </>
  ),
  devices: (
    <>
      <rect x="1.8" y="3" width="9" height="6.2" rx="1" />
      <path d="M1.3 11h10" strokeLinecap="round" />
      <rect x="10.5" y="5.3" width="4.2" height="7.4" rx="1" />
    </>
  ),
  lock: (
    <>
      <rect x="3.2" y="7.3" width="9.6" height="6.2" rx="1.3" />
      <path d="M5.3 7.3V5.2a2.7 2.7 0 0 1 5.4 0v2.1" strokeLinecap="round" />
    </>
  ),
  collapse: (
    <>
      <path d="M9.5 4L5.5 8l4 4" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M13 3v10" strokeLinecap="round" />
    </>
  ),
  sun: (
    <>
      <circle cx="8" cy="8" r="3.3" />
      <path
        d="M8 1v1.6M8 13.4V15M1 8h1.6M13.4 8H15M3.2 3.2l1.1 1.1M11.7 11.7l1.1 1.1M12.8 3.2l-1.1 1.1M4.3 11.7l-1.1 1.1"
        strokeLinecap="round"
      />
    </>
  ),
  moon: <path d="M13.4 9.6A5.8 5.8 0 0 1 6.4 2.6a5.9 5.9 0 1 0 7 7z" strokeLinejoin="round" />,
  search: (
    <>
      <circle cx="7" cy="7" r="4.4" />
      <path d="M10.4 10.4L14 14" strokeLinecap="round" />
    </>
  ),
  plus: <path d="M8 3.2v9.6M3.2 8h9.6" strokeLinecap="round" />,
  home: (
    <path
      d="M2.6 6.8L8 2.6l5.4 4.2v6a.8.8 0 0 1-.8.8h-2.8V9.6H6.2v4H3.4a.8.8 0 0 1-.8-.8z"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  // The mock's home.html "Admin console" shield.
  admin: (
    <path d="M8 1.8l5.2 2v3.4c0 3-2.2 5.3-5.2 6.4-3-1.1-5.2-3.4-5.2-6.4V3.8z" strokeLinejoin="round" />
  ),
  signout: (
    <path
      d="M6 2H3a1 1 0 0 0-1 1v10a1 1 0 0 0 1 1h3M10.5 11l3.5-3-3.5-3M14 8H6.5"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  ),
  // Density: three rules comfortable / four rules dense. The caller picks the
  // variant by passing "density"; the four-rule form is `<ShellIcon dense>`.
  density: <path d="M2 3.5h12M2 8h12M2 12.5h12" strokeLinecap="round" />,
};

const DENSE_GLYPH = <path d="M2 2.8h12M2 6.3h12M2 9.7h12M2 13.2h12" strokeLinecap="round" />;

/** Render a named shell glyph. */
export function ShellIcon({ name, dense, ...props }: IconProps & { name: IconName; dense?: boolean }) {
  return (
    <svg {...iconAttrs(props)}>{dense && name === "density" ? DENSE_GLYPH : SHELL_GLYPHS[name]}</svg>
  );
}
