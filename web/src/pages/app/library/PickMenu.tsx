// A toolbar button that opens a single-choice list (Genre, Sort), in the
// house menu's material (`.user-pop`). It is a listbox, not a menu: picking sets
// a value, like a select. Keyboard as UserMenu: opening moves focus to the
// chosen option, arrows/Home/End rove, a letter jumps, Enter/Space picks,
// Escape closes and hands focus back, Tab closes.

import { useEffect, useId, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { IconCheck, IconChevronDown } from "../../../components/icons";

export interface PickOption<T extends string> {
  value: T;
  label: string;
  count?: number;
}

interface PickMenuProps<T extends string> {
  /** Names the control: "Genre" → "Genre" / "Genre: Racing". */
  name: string;
  /** What the button says on wide screens; phones and tablets show the icon. */
  text: string;
  icon: ReactNode;
  value: T;
  /** The chosen option's label for the accessible name; null = no choice yet. */
  valueLabel: string | null;
  options: PickOption<T>[];
  onPick: (value: T) => void;
}

export function PickMenu<T extends string>({ name, text, icon, value, valueLabel, options, onPick }: PickMenuProps<T>) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const btn = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const id = useId();
  // Where focus lands on open: the chosen option, or the last one for ArrowUp.
  const landing = useRef<"chosen" | "last">("chosen");

  const opts = () => [...(list.current?.querySelectorAll<HTMLElement>('[role="option"]') ?? [])];
  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus) btn.current?.focus();
  };

  useEffect(() => {
    if (!open) return;
    const all = opts();
    const at = landing.current === "last" ? all[all.length - 1] : (all.find((o) => o.getAttribute("aria-selected") === "true") ?? all[0]);
    at?.focus({ preventScroll: true });
    at?.scrollIntoView?.({ block: "nearest" });
    const away = (e: PointerEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open]);

  const openWith = (to: "chosen" | "last") => {
    landing.current = to;
    setOpen(true);
  };

  const onListKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      return close(true);
    }
    if (e.key === "Tab") return close(false);
    const all = opts();
    const i = all.indexOf(document.activeElement as HTMLElement);
    let to: number | null = null;
    if (e.key === "ArrowDown") to = Math.min(i + 1, all.length - 1);
    else if (e.key === "ArrowUp") to = Math.max(i - 1, 0);
    else if (e.key === "Home") to = 0;
    else if (e.key === "End") to = all.length - 1;
    else if (e.key.length === 1 && /\S/.test(e.key)) {
      // Type-ahead: the next option starting with that letter, wrapping.
      const k = e.key.toLowerCase();
      const hit = [...all.slice(i + 1), ...all.slice(0, i + 1)].find((o) => o.dataset.label?.toLowerCase().startsWith(k));
      if (hit) to = all.indexOf(hit);
    }
    if (to === null) return;
    e.preventDefault();
    all[to]?.focus();
  };

  return (
    <div className="lib-pick" ref={wrap}>
      <button
        type="button"
        ref={btn}
        className="lib-trigger"
        aria-label={valueLabel ? `${name}: ${valueLabel}` : name}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? id : undefined}
        onClick={() => (open ? close(false) : openWith("chosen"))}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            openWith(e.key === "ArrowUp" ? "last" : "chosen");
          }
        }}
      >
        {icon}
        <span className="lib-text">{text}</span>
        <IconChevronDown className="lib-chev" />
      </button>
      {open && (
        <div className="lib-pop" role="listbox" id={id} aria-label={name} ref={list} onKeyDown={onListKey}>
          {options.map((o) => (
            <button
              key={o.value}
              type="button"
              role="option"
              tabIndex={-1}
              className="lib-opt"
              aria-selected={o.value === value}
              data-label={o.label}
              onClick={() => {
                close(true);
                if (o.value !== value) onPick(o.value);
              }}
            >
              <IconCheck className="lib-tick" />
              <span className="lib-opt-label">{o.label}</span>
              {o.count !== undefined && <span className="lib-opt-count">{o.count}</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
