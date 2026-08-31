/**
 * ActionsMenu — compact kebab-button row-actions popover.
 * Mirrors the topbar `.usermenu`/`.user-pop` outside-click/Escape pattern
 * (AppShell.tsx) at table-row scale. No dedicated mockup covers a per-row
 * actions menu (admin-hosts.html uses full-width inline buttons on cards);
 * built from styleguide tokens/components as an extrapolation for the
 * table-with-expandable-rows redesign (host-observability round 2).
 */
import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";

import { IconMore } from "./icons";

export interface ActionsMenuItem {
  key: string;
  label: ReactNode;
  onClick: () => void;
  disabled?: boolean;
  variant?: "default" | "danger";
  /** Native tooltip — mainly for a disabled item's reason (e.g. "In use."). */
  title?: string;
}

/** A rule between two groups of items (the mock's `'-'` entry, §A.4): it
 *  separates the navigation items from the ones that change the host. */
export interface ActionsMenuSeparator {
  key: string;
  separator: true;
}

export type ActionsMenuEntry = ActionsMenuItem | ActionsMenuSeparator;

function isSeparator(entry: ActionsMenuEntry): entry is ActionsMenuSeparator {
  return "separator" in entry;
}

interface ActionsMenuProps {
  items: ActionsMenuEntry[];
  label?: string;
}

export function ActionsMenu({ items, label = "Actions" }: ActionsMenuProps) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onPointerDown(e: PointerEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  return (
    <div className="row-menu" ref={ref}>
      <button
        type="button"
        className="row-menu-btn"
        aria-haspopup="true"
        aria-expanded={open}
        aria-label={label}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
      >
        <IconMore className="" />
      </button>
      {open && (
        <div className="row-menu-pop" role="menu" aria-label={label}>
          {items.map((item) =>
            isSeparator(item) ? (
              <hr key={item.key} />
            ) : (
              <button
                key={item.key}
                type="button"
                role="menuitem"
                className={`row-menu-item${item.variant === "danger" ? " danger" : ""}`}
                disabled={item.disabled}
                title={item.title}
                onClick={(e) => {
                  e.stopPropagation();
                  setOpen(false);
                  item.onClick();
                }}
              >
                {item.label}
              </button>
            ),
          )}
        </div>
      )}
    </div>
  );
}
