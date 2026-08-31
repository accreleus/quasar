/**
 * The console topbar (design_handoff_v3/screens/admin-console-v3.html, §A.0).
 *
 * A three-track grid: brand over the rail's width, the centred command-palette
 * trigger, the user button. The brand hides its wordmark when the rail is
 * collapsed (a CSS rule on `[data-rail="collapsed"]`, so it costs no state).
 *
 * The rail toggle is the one addition to the mock: below 820px the rail is a
 * slide-over, and without a trigger the console would have no navigation at
 * all on a phone. It is `display: none` above that breakpoint.
 *
 * The trigger's label is handed down, not decided here: it has to name exactly
 * what the palette was given to search (paletteScope), and only the shell that
 * supplies those lists knows. The console shell is not admin-only — /app/account
 * renders it for everyone, with apps and nothing else.
 */

import { Link } from "react-router-dom";
import { QuasarMark } from "../QuasarMark";
import { IconSearch } from "../icons";
import { UserMenu } from "./UserMenu";

export function Topbar({
  onOpenPalette,
  searchLabel,
  onToggleRail,
  railOpen,
}: {
  onOpenPalette: () => void;
  /** What the palette can actually find here (components/shell/paletteSearch
   *  `paletteTriggerLabel`). */
  searchLabel: string;
  /** Present only when the shell has a rail to slide over. */
  onToggleRail?: () => void;
  railOpen?: boolean;
}) {
  return (
    <header className="topbar">
      <div className="tb-left">
        {onToggleRail && (
          <button
            type="button"
            className="icon-btn rail-btn"
            aria-label="Sections"
            aria-expanded={!!railOpen}
            onClick={onToggleRail}
          >
            <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
              <path d="M2 4h12M2 8h12M2 12h12" strokeLinecap="round" />
            </svg>
          </button>
        )}
        <Link to="/app" className="brand" aria-label="Quasar home">
          <QuasarMark size={24} className="mark" />
          <span className="wordmark">Quasar</span>
        </Link>
      </div>

      <button type="button" className="cmdk" onClick={onOpenPalette}>
        <IconSearch />
        {searchLabel}
        <kbd>⌘K</kbd>
      </button>

      <div className="tb-right">
        <UserMenu mode="console" />
      </div>
    </header>
  );
}
