// Layout for /app/*: the home shell (components/shell/HomeShell.tsx).
//
// Owns the topbar search box's query state and nothing else; HomeShell renders
// the field and the routed page reads the live query through
// LibrarySearchContext — one source of truth, and the shell never learns what
// a library is.
//
// The topbar carries Home | Library as routed links (not component state), so
// the view is bookmarkable and back/forward works. Storage moved to the
// account rail (/app/account/storage); /app/storage stays a redirect in
// App.tsx.
//
// Nav lives in navItems.ts (buildUserNav, SEARCHABLE_ROUTES, re-exported
// below) since userTabs.tsx also derives the small-screen tab bar from it.
//
// The catalogue provider sits here rather than in the page because the shell's
// command palette is above the Outlet and searches the same list.

import { useEffect, useState } from "react";
import { Outlet, useSearchParams } from "react-router-dom";
import { HomeShell } from "../../components/shell/HomeShell";
import { LibraryCatalogProvider } from "./libraryCatalog";
import { LibrarySearchContext } from "./librarySearchContext";
import { buildUserNav, SEARCHABLE_ROUTES } from "./navItems";

export { buildUserNav, SEARCHABLE_ROUTES };

export function AppLayout() {
  // `?q=` seeds the box: the command palette links a chosen app to
  // /app/library?q=<name>, and a shared or bookmarked URL should arrive filtered.
  // Only the param changing re-seeds, so typing (which does not touch the URL)
  // is never overwritten.
  const [params] = useSearchParams();
  const q = params.get("q") ?? "";
  const [query, setQuery] = useState(q);
  useEffect(() => {
    if (q) setQuery(q);
  }, [q]);

  return (
    <LibrarySearchContext.Provider value={{ query, setQuery }}>
      <LibraryCatalogProvider>
        <HomeShell>
          <Outlet />
        </HomeShell>
      </LibraryCatalogProvider>
    </LibrarySearchContext.Provider>
  );
}
