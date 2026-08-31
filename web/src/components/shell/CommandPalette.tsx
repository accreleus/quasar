/**
 * The command palette (design_handoff_v3/screens/admin-console-v3.html
 * `openPalette`/`palResults`, spec §A.0).
 *
 * All the search logic is in ./paletteSearch.ts, which is pure and has no
 * React, no router and no API import. What is left here is the dialog: the
 * global shortcuts, the selection cursor, the modal focus contract, and
 * turning a chosen row into a navigation or a side effect.
 *
 * Mounted once per shell. The shell owns `open` so the topbar's `.cmdk`
 * trigger and the ⌘K shortcut cannot disagree about it.
 *
 * Modal focus contract (it is `aria-modal`, so all three halves are owed):
 *   · opening captures whatever had focus and hands it to the input;
 *   · Tab and Shift+Tab cycle inside the panel, never out of it;
 *   · closing — by Escape, Enter, a click outside or a run — puts focus back
 *     where it came from, so a keyboard user is returned to the trigger
 *     rather than to the top of the document.
 * `<body>` also loses its scrollbar while the palette is up: the page behind
 * a modal must not scroll under the pointer.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAuth } from "../../auth/context";
import { useTheme } from "../../settings/ThemeContext";
import { IconSearch, ShellIcon } from "../icons";
import {
  palettePlaceholder,
  paletteScope,
  searchPalette,
  type PaletteApp,
  type PaletteHost,
  type PaletteItem,
  type PaletteSession,
  type PaletteUser,
} from "./paletteSearch";

/** One frozen empty list for every absent source, so an omitted prop does not
 *  churn the search memo on each render. */
const NONE: readonly never[] = [];

/** True when a keystroke landed in something that eats plain characters — the
 *  `/` shortcut must not steal a slash the user is typing into a field. */
function isTypingTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || !el.tagName) return false;
  if (/^(input|textarea|select)$/i.test(el.tagName)) return true;
  return el.isContentEditable === true;
}

export function CommandPalette({
  open,
  onOpenChange,
  slashShortcut = true,
  hosts,
  sessions,
  apps,
  users,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Whether a bare `/` opens the palette. Off on the home shell, where `/`
   *  already focuses the topbar's own search field (AppHomeNext) — two
   *  handlers on one key is a conflict, not a feature. Cmd/Ctrl+K always
   *  works. */
  slashShortcut?: boolean;
  hosts?: readonly PaletteHost[];
  sessions?: readonly PaletteSession[];
  apps?: readonly PaletteApp[];
  users?: readonly PaletteUser[];
}) {
  const { isAdmin } = useAuth();
  const { toggleTheme } = useTheme();
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [sel, setSel] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  /** Whatever had focus when the palette opened, owed it back on close. */
  const returnFocusRef = useRef<HTMLElement | null>(null);

  const groups = useMemo(
    () =>
      searchPalette(query, {
        hosts: hosts ?? NONE,
        sessions: sessions ?? NONE,
        apps: apps ?? NONE,
        users: users ?? NONE,
        isAdmin,
      }),
    [query, hosts, sessions, apps, users, isAdmin],
  );
  // Derived from the sources actually handed down, not from the role alone —
  // see paletteScope. The account area gets the same palette with a shorter
  // promise, rather than an advert for three lists it does not hold.
  const placeholder = useMemo(
    () => palettePlaceholder(paletteScope({ isAdmin, hosts, sessions, apps, users })),
    [isAdmin, hosts, sessions, apps, users],
  );
  const flat = useMemo(() => groups.flatMap((g) => g.items), [groups]);
  // Clamped rather than reset: the list shrinks under the cursor as you type.
  const selected = Math.min(sel, Math.max(flat.length - 1, 0));

  // Global shortcuts. Registered whether or not the palette is open, because
  // opening it is one of them.
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        onOpenChange(true);
        return;
      }
      if (open && e.key === "Escape") {
        onOpenChange(false);
        return;
      }
      if (slashShortcut && !open && e.key === "/" && !isTypingTarget(e.target)) {
        e.preventDefault();
        onOpenChange(true);
      }
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, onOpenChange, slashShortcut]);

  // The whole open/close side-effect set in one place, so nothing can be
  // acquired without its release: a fresh query (a palette that remembers the
  // last search makes the shortcut unpredictable), the input focused, the
  // page frozen, and the caller's focus restored on the way out.
  useEffect(() => {
    if (!open) return;
    returnFocusRef.current = document.activeElement as HTMLElement | null;
    setQuery("");
    setSel(0);
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    inputRef.current?.focus();
    return () => {
      document.body.style.overflow = previousOverflow;
      const target = returnFocusRef.current;
      returnFocusRef.current = null;
      // The trigger outlives the palette; a removed one simply cannot take it.
      if (target?.isConnected) target.focus();
    };
  }, [open]);

  const run = useCallback(
    (item: PaletteItem | undefined) => {
      if (!item) return;
      onOpenChange(false);
      if (item.action === "toggle-appearance") toggleTheme();
      else if (item.to) navigate(item.to);
    },
    [navigate, onOpenChange, toggleTheme],
  );

  /** Cycle focus inside the panel. Tab is handled outright rather than only
   *  fenced at the edges: the panel's focusables are its own children, so
   *  moving through them explicitly is both the trap and the behaviour. */
  const trapTab = useCallback((e: React.KeyboardEvent) => {
    if (e.key !== "Tab") return;
    const panel = panelRef.current;
    if (!panel) return;
    const focusables = [...panel.querySelectorAll<HTMLElement>("input, button")];
    if (!focusables.length) return;
    e.preventDefault();
    const here = focusables.indexOf(document.activeElement as HTMLElement);
    const next = e.shiftKey
      ? (here <= 0 ? focusables.length : here) - 1
      : (here + 1) % focusables.length;
    focusables[next]?.focus();
  }, []);

  if (!open) return null;

  let index = -1;

  return (
    <div
      className="scrim scrim-top"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onOpenChange(false);
      }}
    >
      <div
        className="pal"
        role="dialog"
        aria-modal="true"
        aria-label="Command palette"
        ref={panelRef}
        onKeyDown={trapTab}
      >
        <div className="pal-in">
          <IconSearch />
          <input
            ref={inputRef}
            role="combobox"
            aria-expanded="true"
            aria-controls="palette-results"
            aria-label={placeholder}
            aria-activedescendant={flat.length ? `palette-option-${selected}` : undefined}
            placeholder={placeholder}
            autoComplete="off"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setSel(0);
            }}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setSel(Math.min(selected + 1, flat.length - 1));
              } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setSel(Math.max(selected - 1, 0));
              } else if (e.key === "Enter") {
                e.preventDefault();
                run(flat[selected]);
              }
            }}
          />
        </div>

        {/* A listbox may only contain options and groups, so each section is a
            real group and `.pal-sec` is its visual label — the accessible one
            rides on aria-label, which is why the heading is hidden from AT. */}
        <div className="pal-list" id="palette-results" role="listbox" aria-label="Results">
          {groups.map((group) => (
            <div key={group.title} role="group" aria-label={group.title}>
              <div className="pal-sec" aria-hidden="true">
                {group.title}
              </div>
              {group.items.map((item) => {
                index += 1;
                const i = index;
                return (
                  <button
                    type="button"
                    key={item.id}
                    id={`palette-option-${i}`}
                    role="option"
                    aria-selected={i === selected}
                    className={i === selected ? "pal-item sel" : "pal-item"}
                    onMouseEnter={() => setSel(i)}
                    onClick={() => run(item)}
                  >
                    <ShellIcon name={item.icon} />
                    <span className="k">{item.label}</span>
                    <span className="m">{item.meta ?? ""}</span>
                  </button>
                );
              })}
            </div>
          ))}
        </div>

        <div className="pal-foot">
          <span>↑↓ navigate</span>
          <span>↵ open</span>
          <span>esc close</span>
        </div>
      </div>
    </div>
  );
}
