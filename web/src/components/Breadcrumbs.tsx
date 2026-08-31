// Breadcrumb trail for /admin drill-down routes.
//
// The rail deliberately carries no entry for the four drill-downs (app editor,
// session detail, host settings, host console) — their parent item stays active
// instead. That leaves "where am I and how do I get back" unanswered on the
// page itself, which is what this fills. Renders as the first child of .page.

import { Link } from "react-router-dom";

export interface Crumb {
  label: string;
  /** Omit on the final crumb — the current page is not a link. */
  to?: string;
  /** Render the label in the mono face (ids, hostnames). */
  mono?: boolean;
  /** Hover text — the full id behind a shortened label. */
  title?: string;
}

function Sep() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
      <path d="M6 3.5L10.5 8 6 12.5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function Breadcrumbs({ items }: { items: Crumb[] }) {
  return (
    <nav className="crumbs" aria-label="Breadcrumb">
      {items.map((c, i) => {
        const last = i === items.length - 1;
        return (
          <span key={`${c.label}-${i}`} style={{ display: "contents" }}>
            {i > 0 && <Sep />}
            {c.to && !last ? (
              <Link to={c.to} className={c.mono ? "mono" : undefined} title={c.title}>
                {c.label}
              </Link>
            ) : (
              <span
                className={`cur${c.mono ? " mono" : ""}`}
                aria-current={last ? "page" : undefined}
                title={c.title}
              >
                {c.label}
              </span>
            )}
          </span>
        );
      })}
    </nav>
  );
}
