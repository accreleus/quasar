/**
 * SegmentedControl — mutually-exclusive pill group (`.segmented`), a roving
 * tabstop: the tablist is one tab stop, arrows/Home/End move within (UX §2.5).
 *
 * Activation: `"automatic"` (default) selects on focus move, like the APG tabs
 * pattern — automatic activation must never drive a fetch; fetch-backed groups
 * use `"manual"`, where nothing selects until Enter/Space/click, the roving
 * tabindex follows focus, and `aria-selected` stays on the value-selected tab.
 *
 * `.focus()` is imperative: the tabIndex swap lands only after re-render, and
 * focus must not be left on an element about to become tabIndex=-1.
 */
import { useRef, useState, type KeyboardEvent, type ReactNode } from "react";

export interface SegmentOption<T extends string = string> {
  value: T;
  label: ReactNode;
  /** aria-label override for icon-only segments */
  ariaLabel?: string;
  /** A segment the group can be in but cannot be moved to — a state the value
   *  reaches by some other route (the overlay's "Custom" preset). Keyboard
   *  navigation skips it and it never takes the tab stop, so a disabled
   *  segment can never be the only way out of the group. */
  disabled?: boolean;
}

interface SegmentedControlProps<T extends string = string> {
  options: SegmentOption<T>[];
  /** `null` = nothing selected (the current value is unknown — e.g. a
   *  settings read failed and the caller refuses to guess). The first
   *  segment still holds the tab stop so the group stays reachable. */
  value: T | null;
  onChange: (value: T) => void;
  "aria-label"?: string;
  /** Disables clicks and arrow-key activation while an in-flight save owns
   *  the current value. */
  disabled?: boolean;
  /** See the header: automatic fires onChange on every navigation keystroke
   *  and is never acceptable for a fetch-backed onChange. */
  activation?: "automatic" | "manual";
}

export function SegmentedControl<T extends string = string>({
  options,
  value,
  onChange,
  "aria-label": ariaLabel,
  disabled = false,
  activation = "automatic",
}: SegmentedControlProps<T>) {
  const btnRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const manual = activation === "manual";
  // If `value` matches nothing, the first segment holds the tab stop so the
  // group is never unreachable by Tab.
  const selectedIndex = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );
  // Manual mode's tabindex follows focus (header); seeded from selectedIndex
  // so the first Tab in lands on the selected segment.
  const [focusedIndex, setFocusedIndex] = useState(selectedIndex);

  /** First non-disabled index at or after `from`, scanning in `dir` and
   *  wrapping; -1 when every segment is disabled. */
  function enabledFrom(from: number, dir: 1 | -1): number {
    const n = options.length;
    for (let step = 0; step < n; step++) {
      const i = (((from + dir * step) % n) + n) % n;
      if (!options[i].disabled) return i;
    }
    return -1;
  }

  // A disabled segment cannot take focus, so it must not hold the tab stop
  // either — the group would be unreachable by Tab while, say, "Custom" is the
  // current value.
  const preferred = manual ? focusedIndex : selectedIndex;
  const tabStop = options[preferred]?.disabled ? enabledFrom(preferred, 1) : preferred;

  function move(to: number, dir: 1 | -1) {
    if (disabled) return;
    const i = enabledFrom(to, dir);
    if (i < 0) return;
    btnRefs.current[i]?.focus();
    if (manual) {
      setFocusedIndex(i);
    } else {
      onChange(options[i].value);
    }
  }

  function activate(index: number) {
    if (disabled || options[index].disabled) return;
    setFocusedIndex(index);
    onChange(options[index].value);
  }

  function onKeyDown(e: KeyboardEvent<HTMLButtonElement>, index: number) {
    if (disabled) return;
    switch (e.key) {
      case "ArrowRight":
      case "ArrowDown":
        e.preventDefault();
        move(index + 1, 1);
        break;
      case "ArrowLeft":
      case "ArrowUp":
        e.preventDefault();
        move(index - 1, -1);
        break;
      case "Home":
        e.preventDefault();
        move(0, 1);
        break;
      case "End":
        e.preventDefault();
        move(options.length - 1, -1);
        break;
      case "Enter":
      case " ":
        if (manual) {
          e.preventDefault();
          activate(index);
        }
        break;
      default:
        break;
    }
  }

  return (
    <div className="segmented" role="tablist" aria-orientation="horizontal" aria-label={ariaLabel}>
      {options.map((opt, i) => (
        <button
          key={opt.value}
          ref={(el) => {
            btnRefs.current[i] = el;
          }}
          role="tab"
          aria-selected={value === opt.value}
          aria-label={opt.ariaLabel}
          tabIndex={i === tabStop ? 0 : -1}
          disabled={disabled || opt.disabled}
          onKeyDown={(e) => onKeyDown(e, i)}
          onClick={() => activate(i)}
          type="button"
        >
          {opt.label}
        </button>
      ))}
    </div>
  );
}
