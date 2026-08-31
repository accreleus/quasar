/**
 * The console shell — the `.app` grid used by /admin/* and /app/account/*
 * (design_handoff_v3/screens/admin-console-v3.html, spec §3.2).
 *
 * Two columns (rail, page) under a full-width topbar, both tracks sized from
 * `--rail-w` / `--topbar-h` so collapsing the rail is a token change and not a
 * re-layout. The shell owns exactly three pieces of state — palette open, rail
 * drawer open, and nothing else. Page state belongs to pages.
 *
 * Below 820px the rail becomes a slide-over behind a scrim (the topbar's
 * `.rail-btn` opens it) — the same responsive pattern the previous shell used,
 * kept because the v3 mocks are desktop-only and dropping it would leave the
 * console unnavigable on a phone.
 */

import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { useLocation } from "react-router-dom";
import { useAuth } from "../../auth/context";
import { CommandPalette } from "./CommandPalette";
import {
  paletteScope,
  paletteTriggerLabel,
  type PaletteApp,
  type PaletteHost,
  type PaletteSession,
  type PaletteUser,
} from "./paletteSearch";
import { Rail } from "./Rail";
import type { RailBadgeCounts, RailSection } from "./navTypes";
import { TabBar } from "./TabBar";
import { Topbar } from "./Topbar";

/** What the palette searches, if the area has anything to offer it. The shell
 *  never fetches: an area that already holds hosts/sessions/apps/users hands
 *  them down (the admin console does, from FleetContext), and one that does
 *  not simply leaves the palette with its action list. */
export interface PaletteSourceProps {
  hosts?: readonly PaletteHost[];
  sessions?: readonly PaletteSession[];
  apps?: readonly PaletteApp[];
  users?: readonly PaletteUser[];
}

export function ConsoleShell({
  sections,
  badges,
  railLabel,
  palette,
  showTabBar = false,
  pageClassName,
  children,
}: {
  sections: RailSection[];
  badges?: RailBadgeCounts;
  /** Accessible name for the rail — "Admin sections" / "Account sections". */
  railLabel: string;
  palette?: PaletteSourceProps;
  /** The user area's three-area tab bar. Off for /admin (see TabBar). */
  showTabBar?: boolean;
  /** Extra class on `.page`; the account area narrows the column (§A.22). */
  pageClassName?: string;
  children: ReactNode;
}) {
  const { pathname } = useLocation();
  const { isAdmin } = useAuth();
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [railOpen, setRailOpen] = useState(false);

  // The trigger names what this area actually handed the palette, so /admin and
  // /app/account promise different things from the same component.
  const searchLabel = useMemo(
    () => paletteTriggerLabel(paletteScope({ isAdmin, ...palette })),
    [isAdmin, palette],
  );

  // A followed link must not stay covered by the drawer it was tapped in.
  useEffect(() => {
    setRailOpen(false);
  }, [pathname]);

  useEffect(() => {
    if (!railOpen) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") setRailOpen(false);
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [railOpen]);

  return (
    <div className={railOpen ? "app app-rail-open" : "app"}>
      <Topbar
        onOpenPalette={() => setPaletteOpen(true)}
        searchLabel={searchLabel}
        onToggleRail={() => setRailOpen((v) => !v)}
        railOpen={railOpen}
      />

      <Rail
        sections={sections}
        badges={badges}
        label={railLabel}
        onNavigate={() => setRailOpen(false)}
      />

      {railOpen && (
        <div className="rail-scrim" onClick={() => setRailOpen(false)} aria-hidden="true" />
      )}

      <main className="main">
        <div className={pageClassName ? `page ${pageClassName}` : "page"}>{children}</div>
      </main>

      {showTabBar && <TabBar />}

      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} {...palette} />
    </div>
  );
}
