// Pure helper for the Runtime-preset "inherited from preset" placeholder
// (UI-P3). Kept standalone so the inherit/override display rule is unit
// tested without mounting the whole Runtime tab.

/**
 * The placeholder shown in the app editor's Image field. When a runtime
 * preset is selected, a blank image inherits the preset's image at launch
 * (server-side merge — see runtime_preset.go / control-api.md), so the field
 * says so instead of showing a generic example. An app that already has an
 * image typed in overrides the preset regardless of this placeholder — a
 * placeholder only ever shows for an empty input, never masks a real value.
 */
export function imageFieldPlaceholder(hasPreset: boolean): string {
  return hasPreset ? "inherited from preset" : "e.g. quasar-agent-dev:latest";
}
