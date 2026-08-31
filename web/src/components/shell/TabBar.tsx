/**
 * The user area's small-screen primary nav — Home / Library / Account.
 *
 * `display: none` above 820px (shell.css), so on a desktop it is not in the
 * layout, not painted, and not in the accessibility tree or the tab order.
 * Its items are derived from `buildUserTabs` (pages/app/userTabs.tsx), which
 * derives them from `buildUserNav` — the pill row and this bar are two
 * expressions of one nav and cannot drift.
 *
 * Its aria-label names the breakpoint rather than repeating "Primary
 * navigation": exactly one of the two is ever exposed, and a screen-reader
 * user landing on a duplicate label would have no way to tell them apart.
 *
 * Not rendered by the admin console: eight subject areas is a rail problem,
 * and /admin is not part of the user area's IA.
 */

import { NavLink } from "react-router-dom";
import { buildUserTabs } from "../../pages/app/userTabs";

export function TabBar() {
  return (
    <nav className="tabbar" aria-label="Primary navigation (small screens)">
      {buildUserTabs().map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          className={({ isActive }) => (isActive ? "active" : undefined)}
        >
          <span className="tb-ic" aria-hidden="true">
            {item.icon}
          </span>
          <span className="tb-lbl">{item.label}</span>
        </NavLink>
      ))}
    </nav>
  );
}
