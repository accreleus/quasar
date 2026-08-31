/**
 * The console rail — the left column of ConsoleShell
 * (design_handoff_v3/screens/admin-console-v3.html `renderRail`, spec §A.0).
 *
 * A flat list of `.rail-item` links (both IAs render one section with no title;
 * `.rail-sec` headings are supported but unused), a `.rail-foot` Collapse
 * button, and two live markers.
 *
 * Rows are plain <Link>s, not NavLinks: which row is lit is the nav item's own
 * `match` predicate, and NavLink would fight it. NavLink derives both the
 * `active` class and `aria-current` from its own route match, so Overview
 * (`to="/admin"`, which prefixes every admin route) would light on every page
 * unless `end` were threaded per item — and an account row lights for every
 * page in its section, not just its own `to`, which no `end` flag can express.
 * One predicate, one lit row.
 *
 * The rail is UX, never authorization: /admin endpoints are enforced
 * server-side (RequireAuth → RequireAdmin).
 */

import { Fragment } from "react";
import { Link, useLocation } from "react-router-dom";
import { ShellIcon } from "../icons";
import { useTheme } from "../../settings/ThemeContext";
import type { RailBadge, RailBadgeCounts, RailSection } from "./navTypes";

function Marker({ kind, count }: { kind: RailBadge; count: number }) {
  const label =
    kind === "live"
      ? `${count} ${count === 1 ? "session" : "sessions"} running now`
      : `${count} ${count === 1 ? "host needs" : "hosts need"} attention`;
  return (
    <span className={`mk mk-${kind}`} aria-label={label}>
      {count}
    </span>
  );
}

export function Rail({
  sections,
  badges,
  label,
  onNavigate,
}: {
  sections: RailSection[];
  badges?: RailBadgeCounts;
  /** Accessible name for the <nav> — "Admin sections" / "Account sections". */
  label: string;
  /** Called when a row is followed, so a narrow-screen drawer can close itself. */
  onNavigate?: () => void;
}) {
  const { pathname } = useLocation();
  const { rail, toggleRail } = useTheme();
  const collapsed = rail === "collapsed";

  return (
    <nav className="rail" aria-label={label}>
      {sections.map((section, i) => (
        <Fragment key={section.title ?? `section-${i}`}>
          {section.title && <div className="rail-sec">{section.title}</div>}
          {section.items.map((item) => {
            const active = item.match(pathname);
            const count = item.badge ? (badges?.[item.badge] ?? 0) : 0;
            return (
              <Link
                key={item.id}
                to={item.to}
                title={item.label}
                onClick={onNavigate}
                aria-current={active ? "page" : undefined}
                className={active ? "rail-item active" : "rail-item"}
              >
                <ShellIcon name={item.icon} />
                <span className="lbl">{item.label}</span>
                {item.badge && count > 0 && <Marker kind={item.badge} count={count} />}
              </Link>
            );
          })}
        </Fragment>
      ))}

      <div className="rail-foot">
        <button
          type="button"
          className="rail-item rail-collapse"
          aria-expanded={!collapsed}
          title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          onClick={toggleRail}
        >
          <ShellIcon name="collapse" />
          <span className="lbl">Collapse</span>
        </button>
      </div>
    </nav>
  );
}
