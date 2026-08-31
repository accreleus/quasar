/**
 * The topbar user menu (design_handoff_v3: admin-console-v3.html `#userpop`
 * for the console, home.html `.user-pop` for the home shell; spec §A.0/§B).
 *
 * One component, two modes, because the two mocks differ only in which of two
 * cross-area entries they carry:
 *   console — "Game library" back to /app at the top.
 *   home    — "Admin console" (admins only) with the Admin chip.
 * Everything else — the identity header, the theme item, the density item,
 * Account and Sign out — is the same menu in both.
 *
 * `mode` is presentation. The Admin console entry is gated on the server-
 * confirmed role for tidiness only; /admin's API is enforced by
 * RequireAuth → RequireAdmin regardless of what any client renders.
 *
 * Keyboard: it claims `role="menu"`, so it behaves like one — opening moves
 * focus to the first item, Arrow keys and Home/End rove between them, and
 * Escape or Tab closes and hands focus back to the trigger. A menu you can
 * open with the keyboard but only read with the mouse is a lie told in ARIA.
 *
 * Open/closed is a class, not a conditional render. `.user-pop` is always in
 * the DOM and `.user-pop.open` reveals it (shell.css). That CSS rule carries
 * `visibility: hidden`, which is what keeps every item — Sign out included —
 * out of the Tab order while the menu is closed; a conditional render would
 * lose the transition, and an opacity-only rule would leave the items
 * focusable. UserMenu.test.tsx asserts the computed style, not the class.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../../auth/context";
import { useTheme } from "../../settings/ThemeContext";
import { IconChevronDown, ShellIcon } from "../icons";

export type UserMenuMode = "console" | "home";

export function UserMenu({ mode }: { mode: UserMenuMode }) {
  const { user, isAdmin, logout } = useAuth();
  const { theme, density, toggleTheme, toggleDensity } = useTheme();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const popRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  /** Close and hand focus back — the keyboard's way out, as opposed to a
   *  click elsewhere, which has already put focus where the user wanted it. */
  const dismiss = useCallback(() => {
    setOpen(false);
    triggerRef.current?.focus();
  }, []);

  const items = useCallback(
    () => [...(popRef.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])],
    [],
  );

  // Close on outside click.
  useEffect(() => {
    if (!open) return;
    function onPointerDown(e: PointerEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  // Close on Escape from anywhere — focus is normally inside the menu, but a
  // mouse-opened menu can leave it on the body.
  useEffect(() => {
    if (!open) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") dismiss();
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, dismiss]);

  // Opening moves focus into the menu; that is what makes the arrow keys
  // below reachable at all without a mouse.
  useEffect(() => {
    if (!open) return;
    items()[0]?.focus();
  }, [open, items]);

  /** Roving focus across the items. */
  const onMenuKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Tab") {
        // Tab means "done here", not "walk into a hidden menu".
        dismiss();
        return;
      }
      const list = items();
      if (!list.length) return;
      const here = list.indexOf(document.activeElement as HTMLElement);
      let next: number | null = null;
      if (e.key === "ArrowDown") next = here < 0 ? 0 : (here + 1) % list.length;
      else if (e.key === "ArrowUp") next = here <= 0 ? list.length - 1 : here - 1;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = list.length - 1;
      if (next === null) return;
      e.preventDefault();
      list[next]?.focus();
    },
    [dismiss, items],
  );

  if (!user) return null;

  const initials = user.username.slice(0, 2).toUpperCase();
  const dark = theme === "dark";
  const dense = density === "dense";

  return (
    <div className="usermenu" ref={ref}>
      <button
        type="button"
        ref={triggerRef}
        className="user-btn"
        aria-expanded={open}
        aria-haspopup="true"
        onClick={() => setOpen((v) => !v)}
      >
        <span className="u-ava">{initials}</span>
        <span className="u-nm">{user.username}</span>
        <IconChevronDown className="u-chev" />
      </button>

      <div
        className={open ? "user-pop open" : "user-pop"}
        role="menu"
        aria-label="User menu"
        ref={popRef}
        onKeyDown={onMenuKeyDown}
      >
        <div className="up-head">
          <span className="u-ava">{initials}</span>
          <div>
            <div className="up-name">{user.username}</div>
            <div className="up-mail mono">{user.email}</div>
          </div>
        </div>

        {mode === "console" && (
          <>
            <Link className="up-item" role="menuitem" to="/app" onClick={() => setOpen(false)}>
              <ShellIcon name="library" />
              Game library
            </Link>
            <div className="up-div" />
          </>
        )}

        <button
          type="button"
          className="up-item"
          role="menuitem"
          onClick={() => toggleTheme()}
        >
          <ShellIcon name={dark ? "sun" : "moon"} />
          {dark ? "Light mode" : "Dark mode"}
          <span className="up-val">{dark ? "☀" : "☾"}</span>
        </button>

        <button
          type="button"
          className="up-item"
          role="menuitem"
          onClick={() => toggleDensity()}
        >
          <ShellIcon name="density" dense={dense} />
          {dense ? "Comfortable mode" : "Dense mode"}
        </button>

        <div className="up-div" />

        <Link
          className="up-item"
          role="menuitem"
          to="/app/account/profile"
          onClick={() => setOpen(false)}
        >
          <ShellIcon name="profile" />
          Account
        </Link>

        {mode === "home" && isAdmin && (
          <Link className="up-item" role="menuitem" to="/admin" onClick={() => setOpen(false)}>
            <ShellIcon name="admin" />
            Admin console
            <span className="chip chip-accent up-val">Admin</span>
          </Link>
        )}

        <div className="up-div" />

        <button
          type="button"
          className="up-item danger"
          role="menuitem"
          onClick={() => {
            setOpen(false);
            void Promise.resolve(logout()).finally(() => navigate("/login"));
          }}
        >
          <ShellIcon name="signout" />
          Sign out
        </button>
      </div>
    </div>
  );
}
