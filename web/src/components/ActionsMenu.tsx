/**
 * ActionsMenu — compact kebab-button row-actions popover.
 * Mirrors the topbar `.usermenu`/`.user-pop` outside-click/Escape pattern
 * (AppShell.tsx) at table-row scale. No dedicated mockup covers a per-row
 * actions menu (admin-hosts.html uses full-width inline buttons on cards);
 * built from styleguide tokens/components as an extrapolation for the
 * table-with-expandable-rows redesign (host-observability round 2).
 *
 * The popover is `position: fixed`, anchored from the trigger button's
 * `getBoundingClientRect()` rather than `.row-menu`'s own box (like `.menu`,
 * primitives.css) — it renders inside `.table-wrap { overflow: auto }`
 * (HostsTab/StorageTab), which clips anything `absolute` past the table's
 * edge (#101). It defaults below the button and flips above when a
 * post-mount measurement of its own height would overflow the viewport, and
 * closes on any ancestor scroll (capture phase, since scroll doesn't bubble)
 * or a window resize, since a stale fixed position no longer points at the
 * button.
 */
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";

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

/** Gap between the button and the popover, in either direction. */
const GAP = 6;

export function ActionsMenu({ items, label = "Actions" }: ActionsMenuProps) {
  const [open, setOpen] = useState(false);
  const [placement, setPlacement] = useState<"below" | "above">("below");
  const [right, setRight] = useState(0);
  const [offset, setOffset] = useState(0);
  const ref = useRef<HTMLDivElement>(null);
  const btnRef = useRef<HTMLButtonElement>(null);
  const popRef = useRef<HTMLDivElement>(null);

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

  // A fixed-position popover stops tracking the button the moment its
  // scroll container moves, so any scroll (capture: scroll doesn't bubble)
  // or resize closes it rather than leaving it floating over the wrong row.
  useEffect(() => {
    if (!open) return;
    function close() {
      setOpen(false);
    }
    document.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [open]);

  // Anchor from the button's own rect, not `.row-menu`'s, so the popover
  // reads correctly regardless of what clips its offset parent. Runs before
  // paint so the flip (measured against the popover's real height) never
  // flashes at the wrong spot.
  useLayoutEffect(() => {
    if (!open) return;
    const btn = btnRef.current;
    if (!btn) return;
    const rect = btn.getBoundingClientRect();
    const popHeight = popRef.current?.getBoundingClientRect().height ?? 0;
    const fitsBelow = rect.bottom + GAP + popHeight <= window.innerHeight;
    setRight(window.innerWidth - rect.right);
    if (fitsBelow) {
      setPlacement("below");
      setOffset(rect.bottom + GAP);
    } else {
      setPlacement("above");
      setOffset(window.innerHeight - rect.top + GAP);
    }
  }, [open]);

  const popStyle: CSSProperties =
    placement === "below" ? { top: offset, right } : { bottom: offset, right };

  return (
    <div className="row-menu" ref={ref}>
      <button
        type="button"
        ref={btnRef}
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
        <div className="row-menu-pop" role="menu" aria-label={label} ref={popRef} style={popStyle}>
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
