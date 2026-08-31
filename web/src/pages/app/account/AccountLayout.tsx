// Layout for /app/account/*. Uses the same console shell the admin area uses
// (components/shell/ConsoleShell.tsx) with the account rail's three sections —
// one shell implementation, two ILs, so a user who is also an operator does
// not learn two navigation models.
//
// This layout supplies its own shell, which is why /app/account sits outside
// AppLayout (App.tsx): the home shell's pill row would be redundant with the
// rail this one renders.
//
// No role check here or anywhere in this file — this is a user-area route.
// Authorization is server-enforced (CLAUDE.md invariant #6); nothing client-
// side ever gates access.
//
// Small screens: the console shell's optional bottom tab bar is on here. The
// rail answers "which section of my account"; the tab bar answers "which area
// of the app" (Home / Library / Account). Before it existed, Account was a
// one-way door on a phone. /admin deliberately leaves it off — eight subject
// areas is a rail problem, not a three-slot one (see components/shell/TabBar).

import { Outlet } from "react-router-dom";
import "../../../styles/account.css";
import { ConsoleShell } from "../../../components/shell/ConsoleShell";
import { LibraryCatalogProvider, useLibraryCatalog } from "../libraryCatalog";
import { buildAccountSections } from "./accountNav";

export function AccountLayout() {
  // The catalogue is the only thing this area can hand the command palette:
  // hosts, sessions and users are admin reads, and the account area must never
  // make one (see lib/fleet/FleetContext). The palette's promise narrows to
  // match — components/shell/paletteSearch `paletteScope`.
  return (
    <LibraryCatalogProvider>
      <AccountConsoleShell />
    </LibraryCatalogProvider>
  );
}

function AccountConsoleShell() {
  const { apps } = useLibraryCatalog();
  return (
    <ConsoleShell
      sections={buildAccountSections()}
      railLabel="Account sections"
      pageClassName="ac-page"
      palette={{ apps }}
      showTabBar
    >
      <Outlet />
    </ConsoleShell>
  );
}
