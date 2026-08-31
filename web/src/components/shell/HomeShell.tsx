/**
 * The home shell — the 64px topbar over /app and /app/library
 * (design_handoff_v3/screens/home.html, spec §B).
 *
 * Brand, Home/Library pills, an absolutely centred search box, the user menu
 * in "home" mode (Admin console instead of Game library). No rail: the user
 * area is two destinations, and a rail for two rows is furniture.
 *
 * The search box is the LibrarySearchContext consumer's other end — this shell
 * renders the field, AppLayout owns the query, and the routed page reads it
 * through the context. Rendered only on the routes that have a grid to filter
 * (`SEARCHABLE_ROUTES`).
 *
 * The command palette mounts here too, for the ⌘K shortcut. There is no `.cmdk`
 * trigger in the home topbar — the mock has none, and the centred search field
 * already owns that spot. `/` is left to that field (AppHomeNext binds it) for
 * the same reason. It searches the same catalogue the page below renders
 * (pages/app/libraryCatalog), so the two can never disagree about what exists.
 */

import { useState } from "react";
import type { ReactNode } from "react";
import { Link, NavLink, useLocation } from "react-router-dom";
import { QuasarMark } from "../QuasarMark";
import { IconSearch } from "../icons";
import { CommandPalette } from "./CommandPalette";
import { TabBar } from "./TabBar";
import { UserMenu } from "./UserMenu";
import { useLibraryCatalog } from "../../pages/app/libraryCatalog";
import { buildUserNav, SEARCHABLE_ROUTES } from "../../pages/app/navItems";
import { useLibrarySearch } from "../../pages/app/librarySearchContext";

export function HomeShell({ children }: { children: ReactNode }) {
  const { pathname } = useLocation();
  const { query, setQuery } = useLibrarySearch();
  const { apps } = useLibraryCatalog();
  const [paletteOpen, setPaletteOpen] = useState(false);

  // Exact match, not `startsWith` — /app/session/:id has no shell; a future
  // /app/* child must opt in explicitly.
  const searchable = (SEARCHABLE_ROUTES as readonly string[]).includes(pathname);

  return (
    <div className="app app-home">
      <header className="topbar home">
        <Link to="/app" className="brand" aria-label="Quasar home">
          <QuasarMark size={24} className="mark" />
          <span className="wordmark">Quasar</span>
        </Link>

        <nav className="nav" aria-label="Primary navigation">
          {buildUserNav().map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end
              className={({ isActive }) => (isActive ? "active" : undefined)}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="spacer" />

        {searchable && (
          <div className="search">
            <IconSearch className="ic" />
            <input
              placeholder="Search games and hosts…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              aria-label="Search library"
            />
          </div>
        )}

        <UserMenu mode="home" />
      </header>

      <main className="main main-home">{children}</main>

      <TabBar />

      <CommandPalette
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
        slashShortcut={false}
        apps={apps}
      />
    </div>
  );
}
