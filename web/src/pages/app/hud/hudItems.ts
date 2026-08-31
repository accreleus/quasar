// The one place the frozen `session_overlay.strip_items` vocabulary meets the
// HUD's: `identity` is the bar title, and the three metrics collapse into one
// group. A second mapping would disagree, on someone else's account.

import {
  itemsForPreset,
  type StripItem,
  type StripPreset,
} from "../../../settings/overlayPreferences";

export interface HudItems {
  /** Four-bar connection glyph. */
  signal: boolean;
  /** Frame rate, plus latency and bitrate once the shelf is open. */
  metrics: boolean;
  /** App name in the bar (`strip_items.identity`). */
  title: boolean;
  /** Codec chip beside the title. */
  codec: boolean;
  /** "Menu Ctrl Alt ⇧ Q" reminder. */
  hint: boolean;
  capture: boolean;
  mic: boolean;
  fullscreen: boolean;
  exit: boolean;
}

/** Contract item set → HUD parts. The tabs, the chevron and the shelf are not
 *  preference-gated: turning off a readout hides a number, but turning off the
 *  only route into the shelf would strand the session. */
export function hudItems(items: Record<StripItem, boolean>): HudItems {
  return {
    signal: items.signal,
    metrics: items.metrics,
    title: items.identity,
    codec: items.codec,
    hint: items.hint,
    capture: items.capture,
    mic: items.mic,
    fullscreen: items.fullscreen,
    exit: items.exit,
  };
}

/** The named presets, expressed in HUD vocabulary. Derived from
 *  `itemsForPreset` rather than re-listed, so a preset edit lands in both. */
export function hudItemsForPreset(preset: Exclude<StripPreset, "custom">): HudItems {
  return hudItems(itemsForPreset(preset));
}
