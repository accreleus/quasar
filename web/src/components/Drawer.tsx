/**
 * Drawer (UI-04).
 * Slides in from the right with a dimming scrim.
 *
 * Dialog focus contract (V2 hardening — behavior, not skin, so it applies on
 * every route that renders a Drawer):
 *   - initial focus moves into the drawer when it opens (first focusable,
 *     which in practice is the close button in `.drawer-head`);
 *   - Tab/Shift+Tab are contained while open;
 *   - Escape closes (unchanged);
 *   - focus returns to the element that was focused before opening;
 *   - body scroll is locked while open and restored on close/unmount.
 * The component API is unchanged.
 */
import { useEffect, useRef, type ReactNode } from "react";

import { IconClose } from "./icons";

const FOCUSABLE =
  'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

interface DrawerProps {
  open: boolean;
  onClose: () => void;
  title: string;
  /** Optional kicker line above the title (`.dw-head`'s eyebrow in the v3
   *  mock, e.g. "launch profile · ordered chain of stream profiles"). */
  eyebrow?: ReactNode;
  children: ReactNode;
  /** Drawer width override, default 400px */
  width?: number | string;
  /**
   * Optional sticky action bar (rendered in `.drawer-foot`, already a flex
   * row with a top border — see components.css). Mirrors Modal's `footer`
   * prop. Omit it to get today's behaviour: actions live inline at the end
   * of the scrollable body (as UserDrawer's Actions section does).
   */
  footer?: ReactNode;
}

export function Drawer({ open, onClose, title, eyebrow, children, width = 400, footer }: DrawerProps) {
  const panelRef = useRef<HTMLDivElement>(null);

  // Escape closes.
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [open, onClose]);

  // Focus management + body scroll lock for the open lifetime. Runs on the
  // open->true transition; cleanup runs on close OR unmount, so focus is
  // returned and scroll restored on both paths (no trap after close).
  useEffect(() => {
    if (!open) return;
    const opener = document.activeElement as HTMLElement | null;

    // Initial focus: first focusable inside the panel (close button), falling
    // back to the panel itself.
    const panel = panelRef.current;
    const first = panel?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? panel)?.focus();

    // Containment: cycle Tab within the panel.
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== "Tab" || !panelRef.current) return;
      const focusable = [
        ...panelRef.current.querySelectorAll<HTMLElement>(FOCUSABLE),
      ].filter((el) => el.offsetParent !== null || el === document.activeElement);
      if (focusable.length === 0) {
        e.preventDefault();
        panelRef.current.focus();
        return;
      }
      const firstEl = focusable[0];
      const lastEl = focusable[focusable.length - 1];
      const active = document.activeElement;
      if (e.shiftKey && (active === firstEl || active === panelRef.current)) {
        e.preventDefault();
        lastEl.focus();
      } else if (!e.shiftKey && active === lastEl) {
        e.preventDefault();
        firstEl.focus();
      }
    };
    document.addEventListener("keydown", onKeyDown);

    // Body scroll lock. Restore the previous inline value, not "", so we
    // compose with anything else managing body overflow.
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = prevOverflow;
      // Return focus to the opener if it is still in the document.
      if (opener && opener.isConnected) opener.focus();
    };
  }, [open]);

  if (!open) return null;

  return (
    <>
      <div
        className="drawer-scrim open"
        onClick={onClose}
        aria-hidden="true"
      />
      <div
        ref={panelRef}
        className="drawer open"
        style={{ width }}
        role="dialog"
        aria-label={title}
        aria-modal="true"
        tabIndex={-1}
      >
        <div className="drawer-head">
          <div>
            {eyebrow && (
              <div className="eyebrow mono" style={{ textTransform: "none", letterSpacing: 0, marginBottom: 4 }}>
                {eyebrow}
              </div>
            )}
            <h3>{title}</h3>
          </div>
          <button
            className="btn-icon btn-ghost"
            onClick={onClose}
            aria-label="Close drawer"
          >
            <IconClose />
          </button>
        </div>
        <div className="drawer-body">{children}</div>
        {footer && <div className="drawer-foot">{footer}</div>}
      </div>
    </>
  );
}
