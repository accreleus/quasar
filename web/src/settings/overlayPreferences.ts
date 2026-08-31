// The single source of truth for in-session overlay presentation preferences.
//
// Both the Account UI (which edits them) and SessionStrip/SessionDrawer (which
// obey them) import from here, and this module owns the ONLY conversion between
// the snake_case wire shape and the camelCase domain shape. Two conversions
// would eventually disagree, and the disagreement would only show up on someone
// else's machine.

import type { SessionOverlayWire } from "../api/me";

export const STRIP_ITEMS = [
  "signal",
  "identity",
  "codec",
  "metrics",
  "hint",
  "capture",
  "exit",
  "mic",
  "fullscreen",
] as const;
export type StripItem = (typeof STRIP_ITEMS)[number];

export const STRIP_PRESETS = ["full", "minimal", "metrics", "custom"] as const;
export type StripPreset = (typeof STRIP_PRESETS)[number];

/** All four docks. `left`/`right` joined the contract in the UI v3 amendment;
 *  the overlay could already draw them. */
export const STRIP_POSITIONS = ["top", "bottom", "left", "right"] as const;
export type StripPosition = (typeof STRIP_POSITIONS)[number];

export const STRIP_AUTO_HIDE_MODES = ["on_capture", "always_visible", "never_visible"] as const;
export type StripAutoHide = (typeof STRIP_AUTO_HIDE_MODES)[number];

export interface SessionOverlayPreferences {
  stripPreset: StripPreset;
  stripItems: Record<StripItem, boolean>;
  stripPosition: StripPosition;
  stripAutoHide: StripAutoHide;
}

type NamedPreset = Exclude<StripPreset, "custom">;

/** Named preset item sets. capture/exit/mic/fullscreen are actions, not
 *  status, so they default on everywhere; "Minimal" is the deliberate
 *  exception (operator request) and keeps only Signal + Capture + Mic + Exit.
 *
 *  Wire migration: a preset keeps its name across an item-set rollout only if
 *  it agrees with fromWire's absent-key-defaults-true rule for every new key.
 *  A pre-rollout "Minimal" (fullscreen: false disagrees) therefore lands on
 *  "Custom" — accepted: the item set is the authority, the name only a label,
 *  and choices are preserved byte for byte (overlayPreferences.test.ts). */
const PRESET_ITEMS: Record<NamedPreset, Record<StripItem, boolean>> = {
  full: {
    signal: true, identity: true, codec: true, metrics: true, hint: true,
    capture: true, exit: true, mic: true, fullscreen: true,
  },
  minimal: {
    signal: true, identity: false, codec: false, metrics: false, hint: false,
    capture: true, exit: true, mic: true, fullscreen: false,
  },
  metrics: {
    signal: false, identity: false, codec: false, metrics: true, hint: true,
    capture: true, exit: true, mic: true, fullscreen: true,
  },
};

export function itemsForPreset(preset: NamedPreset): Record<StripItem, boolean> {
  return { ...PRESET_ITEMS[preset] };
}

export const DEFAULT_OVERLAY_PREFERENCES: SessionOverlayPreferences = {
  stripPreset: "full",
  stripItems: itemsForPreset("full"),
  stripPosition: "bottom",
  stripAutoHide: "on_capture",
};

export function applyPreset(
  prefs: SessionOverlayPreferences,
  preset: NamedPreset,
): SessionOverlayPreferences {
  return { ...prefs, stripPreset: preset, stripItems: itemsForPreset(preset) };
}

/** The preset name for an item set, or "custom" when it matches none. Editing
 *  an item and then undoing the edit therefore returns the preset label rather
 *  than stranding the user on "Custom". */
function presetFor(items: Record<StripItem, boolean>): StripPreset {
  for (const name of Object.keys(PRESET_ITEMS) as NamedPreset[]) {
    const candidate = PRESET_ITEMS[name];
    if (STRIP_ITEMS.every((k) => candidate[k] === items[k])) return name;
  }
  return "custom";
}

export function toggleItem(
  prefs: SessionOverlayPreferences,
  item: StripItem,
): SessionOverlayPreferences {
  const stripItems = { ...prefs.stripItems, [item]: !prefs.stripItems[item] };
  return { ...prefs, stripItems, stripPreset: presetFor(stripItems) };
}

function oneOf<T extends string>(allowed: readonly T[], raw: unknown, fallback: T): T {
  return typeof raw === "string" && (allowed as readonly string[]).includes(raw)
    ? (raw as T)
    : fallback;
}

/**
 * Wire → domain. Defensive on every field: this data is round-tripped through a
 * database and could have been written by a newer client. An unknown value falls
 * back to the default rather than throwing — a bad preference must never be able
 * to prevent a session from rendering.
 */
export function fromWire(w: SessionOverlayWire | undefined): SessionOverlayPreferences {
  if (!w) return { ...DEFAULT_OVERLAY_PREFERENCES, stripItems: itemsForPreset("full") };

  const storedPreset = oneOf(STRIP_PRESETS, w.strip_preset, "full");

  // No stored item set: the preset name is all we have, so expand it. A stored
  // "custom" with no items is incoherent — treat it as the default set.
  if (!w.strip_items) {
    const items = storedPreset === "custom" ? itemsForPreset("full") : itemsForPreset(storedPreset);
    return {
      stripPreset: storedPreset === "custom" ? "full" : storedPreset,
      stripItems: items,
      stripPosition: oneOf(STRIP_POSITIONS, w.strip_position, DEFAULT_OVERLAY_PREFERENCES.stripPosition),
      stripAutoHide: oneOf(STRIP_AUTO_HIDE_MODES, w.strip_auto_hide, DEFAULT_OVERLAY_PREFERENCES.stripAutoHide),
    };
  }

  const rawItems = w.strip_items as Partial<Record<StripItem, unknown>>;
  const stripItems = STRIP_ITEMS.reduce(
    (acc, k) => {
      acc[k] = typeof rawItems[k] === "boolean" ? (rawItems[k] as boolean) : true;
      return acc;
    },
    {} as Record<StripItem, boolean>,
  );

  return {
    // The stored preset is a label, not an authority: when it disagrees with the
    // stored item set, the item set wins — it is what the user will actually see.
    stripPreset: presetFor(stripItems),
    stripItems,
    stripPosition: oneOf(STRIP_POSITIONS, w.strip_position, DEFAULT_OVERLAY_PREFERENCES.stripPosition),
    stripAutoHide: oneOf(STRIP_AUTO_HIDE_MODES, w.strip_auto_hide, DEFAULT_OVERLAY_PREFERENCES.stripAutoHide),
  };
}

/** Domain → wire. Always complete, never partial: the caller decides what to
 *  send by picking fields off the result, and a half-built object here would
 *  make that decision twice. */
export function toWire(p: SessionOverlayPreferences): SessionOverlayWire {
  return {
    strip_preset: p.stripPreset,
    strip_items: { ...p.stripItems },
    strip_position: p.stripPosition,
    strip_auto_hide: p.stripAutoHide,
  };
}
