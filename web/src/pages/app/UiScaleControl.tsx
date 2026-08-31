/**
 * Interface size — the `wp_fractional_scale_v1` preferred_scale hint pushed to
 * the session's toplevels (control-api.md `PATCH /v1/sessions/{id}/display`, `ui_scale`).
 * Scales the app's own chrome without touching the stream or render resolution.
 * Presentational only: debounce, `busy`, revert-to-last-acked live in SessionPage.
 */

/** Contract range is [1.0, 3.0]; these are the rungs we expose. */
export const UI_SCALE_STEPS: readonly number[] = [1, 1.25, 1.5, 1.75, 2, 2.5, 3];

/** Caption: `wp_fractional_scale_v1` is a hint — a desktop shell honours it,
 *  a game rendering its own UI does not, so the limit is stated up front. */
export const UI_SCALE_NOTE = "Scales the desktop UI (KDE); games ignore it.";

/** Stable `<option value>` — a fixed 2dp string so 1 and 1.0 can never differ. */
function optionValue(v: number): string {
  return v.toFixed(2);
}

function stepLabel(v: number): string {
  return `${Math.round(v * 100)}%`;
}

export interface UiScaleControlProps {
  value: number;
  onChange: (v: number) => void;
  /** True while a PATCH is in flight. */
  busy?: boolean;
}

export function UiScaleControl({ value, onChange, busy }: UiScaleControlProps) {
  const steps = [...UI_SCALE_STEPS];
  // As in RenderResolutionControl: show an off-ladder live value rather than
  // letting the select disagree with the session.
  if (!steps.some((s) => optionValue(s) === optionValue(value))) {
    steps.push(value);
    steps.sort((a, b) => a - b);
  }

  return (
    <select
      className="session-scaling-select"
      value={optionValue(value)}
      disabled={busy}
      onChange={(e) => {
        const picked = steps.find((s) => optionValue(s) === e.target.value);
        if (picked !== undefined) onChange(picked);
      }}
      aria-label="Interface size"
    >
      {steps.map((s) => (
        <option key={optionValue(s)} value={optionValue(s)}>
          {stepLabel(s)}
        </option>
      ))}
    </select>
  );
}
