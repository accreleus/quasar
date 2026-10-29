// What the shells mount. The palette (search, the settings index, fuzzy game
// search) loads the first time it opens; the open shortcuts and the reveal's
// cancel-on-navigation must work before that, so they live here.
import { lazy, Suspense, useEffect, type ComponentProps } from "react";
import type { CommandPalette as Palette } from "./CommandPalette";
import { useCancelRevealOnNavigation } from "./settingsSearch/reveal";

const CommandPalette = lazy(() => import("./CommandPalette").then((m) => ({ default: m.CommandPalette })));

/** True when a keystroke landed in something that eats plain characters — the
 *  `/` shortcut must not steal a slash the user is typing into a field. */
export function isTypingTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el || !el.tagName) return false;
  if (/^(input|textarea|select)$/i.test(el.tagName)) return true;
  return el.isContentEditable === true;
}

export function LazyCommandPalette(props: ComponentProps<typeof Palette>) {
  const { open, onOpenChange, slashShortcut = true } = props;
  useCancelRevealOnNavigation();
  // Once open, the palette handles its own keys.
  useEffect(() => {
    if (open) return;
    function onKeyDown(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        onOpenChange(true);
      } else if (slashShortcut && e.key === "/" && !isTypingTarget(e.target)) {
        e.preventDefault();
        onOpenChange(true);
      }
    }
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open, onOpenChange, slashShortcut]);
  return open ? (
    <Suspense fallback={null}>
      <CommandPalette {...props} />
    </Suspense>
  ) : null;
}
