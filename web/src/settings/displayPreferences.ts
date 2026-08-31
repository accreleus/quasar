/** Persistent display/scaling preferences stored in localStorage. */

const PREFS_KEY = "quasar.display.preferences";

/** Canonical list of supported scaling modes — single source of truth.
 *  Both the load-time validator below and the HUD's Scaling segment
 *  derive from this constant so adding a mode can't desync the two. */
export const SCALING_MODES = ["contain", "cover", "stretch", "integer"] as const;
export type ScalingMode = typeof SCALING_MODES[number];

export interface DisplayPreferences {
  scalingMode: ScalingMode;
  /**
   * Microphone spec §3.4: the capture device the user prefers, if any. Honoured
   * by the mic manager when present (falls back to the system default when the
   * device is gone). There is no picker UI in v1 — the field is written by a
   * later device-selection surface; the ENABLED state is deliberately never
   * persisted (every session starts mic-off).
   */
  preferredMicDeviceId?: string;
}

const DEFAULT_PREFS: DisplayPreferences = {
  scalingMode: "contain",
};

export function loadDisplayPreferences(): DisplayPreferences {
  try {
    const raw = localStorage.getItem(PREFS_KEY);
    if (!raw) return { ...DEFAULT_PREFS };
    const parsed = JSON.parse(raw) as Partial<DisplayPreferences>;
    const scalingMode: ScalingMode = (SCALING_MODES as ReadonlyArray<string>).includes(
      parsed.scalingMode ?? "",
    )
      ? (parsed.scalingMode as ScalingMode)
      : DEFAULT_PREFS.scalingMode;
    // Defensive like the scaling parse: only a non-empty string survives.
    const preferredMicDeviceId =
      typeof parsed.preferredMicDeviceId === "string" && parsed.preferredMicDeviceId !== ""
        ? parsed.preferredMicDeviceId
        : undefined;
    return preferredMicDeviceId ? { scalingMode, preferredMicDeviceId } : { scalingMode };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

/**
 * Persist preferences, MERGING over what is already stored.
 *
 * Merge rather than replace because callers legitimately own only one field —
 * the HUD's scaling control writes `{ scalingMode }` and must not silently drop
 * a stored `preferredMicDeviceId`. Pass an explicit value to change a field;
 * omit it to leave the stored one alone.
 */
export function saveDisplayPreferences(prefs: Partial<DisplayPreferences>): void {
  const merged = { ...loadDisplayPreferences(), ...prefs };
  localStorage.setItem(PREFS_KEY, JSON.stringify(merged));
}
