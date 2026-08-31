// /app/account/overlay — edits the in-session overlay preferences.
//
// Saves immediately on every change rather than behind a Save button: these are
// presentation toggles with no validation step and no destructive outcome, and
// the provider already rolls back on failure (OverlayPreferencesContext). A
// Save button would only add a state where the preview and the stored value
// disagree.
//
// "Custom" is a state the item set lands on, not a choice: the segment is
// rendered (the mock draws it) but always disabled, because a control that
// selects "Custom" cannot say which items it would turn on. The switches below
// are the only way in.

import { SegmentedControl } from "../../../components/SegmentedControl";
import { SelectField, Switch } from "../../../components/TextField";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { useOverlayPreferences } from "../../../settings/OverlayPreferencesContext";
import { OverlayPreview } from "./OverlayPreview";
import {
  STRIP_ITEMS,
  applyPreset,
  toggleItem,
  STRIP_AUTO_HIDE_MODES,
  type StripAutoHide,
  type StripItem,
  type StripPosition,
} from "../../../settings/overlayPreferences";

const ITEM_LABELS: Record<StripItem, string> = {
  signal: "Connection signal",
  identity: "App name and quality",
  codec: "Codec",
  metrics: "FPS, latency, bitrate",
  hint: "Menu shortcut hint",
  capture: "Capture input",
  exit: "Exit session",
  mic: "Microphone",
  fullscreen: "Fullscreen",
};

/** capture/exit are one-click controls, not status readouts — the five items
 *  above them describe the stream, these two act on it. They get their own
 *  group in the list so the switch above them ("Codec", say) doesn't read as
 *  the same kind of thing as "Exit session". */
const ACTION_ITEMS: ReadonlySet<StripItem> = new Set(["capture", "exit"]);

/** Auto-hide, in the user's words. The mock's "After 10 seconds" is dropped:
 *  the contract carries three modes and no duration (spec §9). `never_visible`
 *  hides the overlay outright rather than never auto-hiding it, so it is spelt
 *  out — a bare "Never" reads as the opposite of what it does. */
const AUTO_HIDE_LABELS: Record<StripAutoHide, string> = {
  on_capture: "After 4 seconds",
  always_visible: "Always visible",
  never_visible: "Never show the overlay",
};

export function AccountOverlay() {
  const { prefs, loaded, save, error } = useOverlayPreferences();
  useSectionHead({
    sub: "The overlay shown over a running session. These settings follow your account to every device you sign in on.",
  });

  return (
    <div className="card sec-card">
      {/* First thing on the page and above the controls, because it is what
          they describe: both read the same OverlayPreferences context, so it
          updates as the switches move. Its own embedded buttons are made
          inert by OverlayPreview, independent of `loaded`. */}
      <OverlayPreview />

      <fieldset disabled={!loaded} className="ac-fieldset">
        <div className="field mt5">
          <span className="label">Content</span>
          <SegmentedControl
            aria-label="Overlay content preset"
            value={prefs.stripPreset}
            onChange={(v) => void save(applyPreset(prefs, v as "full" | "minimal" | "metrics"))}
            options={[
              { value: "full", label: "Full" },
              { value: "minimal", label: "Minimal" },
              { value: "metrics", label: "Metrics" },
              { value: "custom", label: "Custom", disabled: true },
            ]}
          />
          <p className="muted mt3">
            Presets are a starting point. Turning any item on or off below keeps your
            choice and moves this to Custom, which is why Custom cannot be picked here.
          </p>
        </div>

        <div className="grid g2 mt5">
          <div>
            <div className="eyebrow ac-group">Readouts</div>
            {STRIP_ITEMS.filter((item) => !ACTION_ITEMS.has(item)).map((item) => (
              <div className="ac-sw" key={item}>
                <Switch
                  label={ITEM_LABELS[item]}
                  id={`strip-item-${item}`}
                  checked={prefs.stripItems[item]}
                  disabled={!loaded}
                  onChange={() => void save(toggleItem(prefs, item))}
                />
              </div>
            ))}
          </div>

          <div>
            <div className="eyebrow ac-group">Controls</div>
            {STRIP_ITEMS.filter((item) => ACTION_ITEMS.has(item)).map((item) => (
              <div className="ac-sw" key={item}>
                <Switch
                  label={ITEM_LABELS[item]}
                  id={`strip-item-${item}`}
                  checked={prefs.stripItems[item]}
                  disabled={!loaded}
                  onChange={() => void save(toggleItem(prefs, item))}
                />
              </div>
            ))}

            <div className="field mt5">
              <span className="label">Position</span>
              <SegmentedControl
                aria-label="Overlay position"
                value={prefs.stripPosition}
                onChange={(v) => void save({ ...prefs, stripPosition: v as StripPosition })}
                options={[
                  { value: "top", label: "Top" },
                  { value: "bottom", label: "Bottom" },
                  { value: "left", label: "Left" },
                  { value: "right", label: "Right" },
                ]}
              />
            </div>

            <div className="field mt4">
              <SelectField
                label="Auto-hide"
                value={prefs.stripAutoHide}
                disabled={!loaded}
                onChange={(e) =>
                  void save({ ...prefs, stripAutoHide: e.target.value as StripAutoHide })
                }
              >
                {STRIP_AUTO_HIDE_MODES.map((mode) => (
                  <option key={mode} value={mode}>
                    {AUTO_HIDE_LABELS[mode]}
                  </option>
                ))}
              </SelectField>
              {prefs.stripAutoHide === "never_visible" && (
                // Targeted by id: OverlayPreview's own placeholder makes the
                // same point in its own words, so text queries cannot tell
                // the two apart.
                <p className="muted mt3" data-testid="strip-hide-hint">
                  With the overlay off, the microphone indicator is the only thing drawn
                  over your session. Open it with Ctrl+Alt+Shift+Q for connection details.
                </p>
              )}
            </div>
          </div>
        </div>
      </fieldset>

      {error && <p className="form-error mt4" role="alert">{error}</p>}
    </div>
  );
}
