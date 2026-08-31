/**
 * Render resolution — the app-facing `wl_output` logical mode, decoupled from
 * the session's pinned stream WxH (control-api.md `PATCH /v1/sessions/{id}/display`).
 *
 * Not the "Scaling" control beside it: Scaling is CSS `object-fit` on
 * the local <video>, browser-only. This changes what the host composites — the
 * app draws at a smaller surface and the compositor upscales into the unchanged
 * stream — so every change is a live PATCH with a server-side veto. "Scaling" is
 * deliberately absent from the copy here so the two rows can't be confused.
 *
 * Presentational only: debounce, in-flight `busy`, and
 * revert-to-last-acked live in SessionPage. This file owns the ladder and copy,
 * unit-testable without the WebRTC / auth / router stack.
 */

/** Candidate render sizes, descending; offered only when same aspect as the
 *  stream and fitting inside it (`renderResolutionOptions`). Every entry is
 *  even on both axes — the contract rejects an odd dimension with `400 validation_failed`. */
export const RENDER_RESOLUTION_LADDER: ReadonlyArray<{ w: number; h: number }> = [
  { w: 3840, h: 2160 },
  { w: 2560, h: 1440 },
  { w: 1920, h: 1080 },
  { w: 1600, h: 900 },
  { w: 1280, h: 720 },
  { w: 960, h: 540 },
];

/** Aspect comparison slack — a non-standard stream size (e.g. 1366×768) needs
 *  room before it's called 16:9. */
const ASPECT_TOLERANCE = 0.01;

export interface RenderResolutionOption {
  w: number;
  h: number;
  label: string;
}

/** Stable `<option value>` for a size — parsed back in the change handler. */
function optionValue(w: number, h: number): string {
  return `${w}x${h}`;
}

function sizeLabel(w: number, h: number): string {
  return `${w}×${h}`;
}

/**
 * "Match stream" first (session default, always accepted), then every
 * same-aspect rung strictly below it, descending. Off-aspect rungs are never
 * offered: rendering 16:9 into a 16:10 stream would letterbox the app inside
 * its own surface, worse than the bandwidth it saves.
 */
export function renderResolutionOptions(
  streamWidth: number,
  streamHeight: number,
): RenderResolutionOption[] {
  const aspect = streamWidth / streamHeight;
  const rungs = RENDER_RESOLUTION_LADDER.filter(
    (r) =>
      r.w <= streamWidth &&
      r.h <= streamHeight &&
      !(r.w === streamWidth && r.h === streamHeight) &&
      Math.abs(r.w / r.h - aspect) <= ASPECT_TOLERANCE,
  ).map((r) => ({ w: r.w, h: r.h, label: sizeLabel(r.w, r.h) }));

  return [
    { w: streamWidth, h: streamHeight, label: `Match stream (${sizeLabel(streamWidth, streamHeight)})` },
    ...rungs,
  ];
}

/** Caption: stream size (bitrate) doesn't move; honouring the new size is up to
 *  the app — a launcher that pins its own mode (Steam Big Picture) keeps its
 *  launch resolution, which otherwise reads as a broken control. */
export function renderResolutionNote(streamWidth: number, streamHeight: number): string {
  return `Keeps the stream at ${sizeLabel(streamWidth, streamHeight)}. Desktops redraw at the new size; some launchers (Steam Big Picture) keep their launch resolution.`;
}

export interface RenderResolutionControlProps {
  /** The session's pinned stream size — the ceiling and the default. */
  streamWidth: number;
  streamHeight: number;
  /** Last-acked render size (the parent reverts to it on a rejected PATCH). */
  value: { w: number; h: number };
  onChange: (v: { w: number; h: number }) => void;
  /** True while a PATCH is in flight. */
  busy?: boolean;
}

export function RenderResolutionControl({
  streamWidth,
  streamHeight,
  value,
  onChange,
  busy,
}: RenderResolutionControlProps) {
  let options = renderResolutionOptions(streamWidth, streamHeight);
  // Off-ladder value possible (admin/API-set, or a future clamp) — show it
  // rather than let the select disagree with the live session.
  if (!options.some((o) => o.w === value.w && o.h === value.h)) {
    options.push({ w: value.w, h: value.h, label: sizeLabel(value.w, value.h) });
    // Keep "Match stream" pinned first (the default, not just the largest rung).
    options = [options[0], ...options.slice(1).sort((a, b) => b.w - a.w)];
  }

  return (
    <select
      className="session-scaling-select"
      value={optionValue(value.w, value.h)}
      disabled={busy}
      onChange={(e) => {
        const picked = options.find((o) => optionValue(o.w, o.h) === e.target.value);
        if (picked) onChange({ w: picked.w, h: picked.h });
      }}
      aria-label="Render resolution"
    >
      {options.map((o) => (
        <option key={optionValue(o.w, o.h)} value={optionValue(o.w, o.h)}>
          {o.label}
        </option>
      ))}
    </select>
  );
}
