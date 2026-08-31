/**
 * Stream (external) resolution — what the host encodes and sends, changed live
 * via `PATCH /v1/sessions/{id}/display` `stream_width`/`stream_height`
 * (adaptive-external-resolution design §D6).
 *
 * Distinct from Scaling (CSS `object-fit`, browser-only) and
 * Render resolution (`RenderResolutionControl`, the app-facing `wl_output`
 * mode): this changes the encoded frame size, so it's the actual bandwidth/
 * decode-cost lever — the app itself keeps drawing at its own size.
 *
 * `session.stream.rungs` is server-owned (control-plane `internal/profile/rungs.go`,
 * already filtered ≤ launch size) — rendered in the order given, never re-sorted,
 * except `fallbackStreamRungs` for an older control plane with no `rungs` at all.
 *
 * Presentational only, like `RenderResolutionControl`: debounce, in-flight guard,
 * and revert-to-last-acked live in SessionPage.
 */

/** Aspect comparison slack — same rationale (and value) as RenderResolutionControl. */
const ASPECT_TOLERANCE = 0.01;

/**
 * Control plane's 16:9 external-rung family, verbatim
 * (`docs/superpowers/specs/2026-08-16-adaptive-external-resolution-design.md` §D4).
 * Mirror of server data, used only when `session.stream.rungs` is absent (an
 * older control plane). 16:9 only — the server also owns 16:10/21:9/4:3, and
 * guessing a family we can't see risks offering rungs it would reject; other
 * aspects fall back to launch-size-only.
 */
const FALLBACK_RUNGS_16_9: ReadonlyArray<readonly [number, number]> = [
  [3840, 2160],
  [2560, 1440],
  [1920, 1080],
  [1600, 900],
  [1280, 720],
];

/** Client-side stand-in for `session.stream.rungs`: 16:9 family filtered to
 *  ≤ launch size, descending; any other aspect returns just the launch size. */
export function fallbackStreamRungs(launch: { w: number; h: number }): [number, number][] {
  const aspect = launch.w / launch.h;
  if (Math.abs(aspect - 16 / 9) > ASPECT_TOLERANCE) return [[launch.w, launch.h]];
  const rungs = FALLBACK_RUNGS_16_9.filter(([w, h]) => w <= launch.w && h <= launch.h).map(
    ([w, h]) => [w, h] as [number, number],
  );
  // Launch size may not be a family member; always offered so "back to launch" is reachable.
  if (!rungs.some(([w, h]) => w === launch.w && h === launch.h)) {
    rungs.unshift([launch.w, launch.h]);
  }
  return rungs;
}

/** Caption: this is the lever that actually changes bandwidth (unlike Render
 *  resolution); the app does not resize with it. */
export const STREAM_RESOLUTION_NOTE =
  "Changes what is encoded and sent to you; the app keeps drawing at its own size.";

export interface StreamResolutionOption {
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

/** Options in server order; launch size labelled "Launch (W×H)" since it's the
 *  default and the only guaranteed-accepted value. Prepended if missing from rungs. */
export function streamResolutionOptions(
  launch: { w: number; h: number },
  rungs: ReadonlyArray<readonly [number, number]>,
): StreamResolutionOption[] {
  const opts = rungs.map(([w, h]) => ({
    w,
    h,
    label:
      w === launch.w && h === launch.h
        ? `Launch (${sizeLabel(w, h)})`
        : sizeLabel(w, h),
  }));
  if (!opts.some((o) => o.w === launch.w && o.h === launch.h)) {
    opts.unshift({ w: launch.w, h: launch.h, label: `Launch (${sizeLabel(launch.w, launch.h)})` });
  }
  return opts;
}

/** `<option value>` for "let the host decide" — distinct from any WxH so the
 *  select can show "Auto" while the session sits on a rung. */
export const AUTO_OPTION_VALUE = "auto";

/**
 * `"auto"` means the ABR ladder owns the size (readback, not a choice) — the
 * "Auto · " prefix stops that reading as a user action. Unknown owner (pre-ladder
 * agent) renders as a plain size rather than falsely claiming "Auto".
 */
export function streamRowLabel(
  owner: "auto" | "pinned" | undefined,
  w: number,
  h: number,
): string {
  return owner === "auto" ? `Auto · ${w}×${h}` : `${w}×${h}`;
}

export interface StreamResolutionControlProps {
  /** Session's launch size (`session.stream.width/height`) — the ceiling, the
   *  default, and the option labelled "Launch". */
  launch: { w: number; h: number };
  /** `session.stream.rungs`, server-ordered. Rendered as given. */
  rungs: ReadonlyArray<readonly [number, number]>;
  /** Current external size (launch when nothing has been changed). */
  value: { w: number; h: number };
  onChange: (v: { w: number; h: number }) => void;
  /** True while a display PATCH is in flight. */
  busy?: boolean;
  /** Set when the encoder can't resize live (`stream.external_resize_supported
   *  === false`); select goes inert, caller renders this as a second `.sd-note`
   *  line so the row stays visible and the limitation is explained. */
  disabledReason?: string;
  /** Who owns the external size: the ladder ("auto") or a user pick ("pinned").
   *  Undefined for a pre-ladder agent — treated like "pinned". */
  owner?: "auto" | "pinned";
}

export function StreamResolutionControl({
  launch,
  rungs,
  value,
  onChange,
  busy,
  disabledReason,
  owner,
}: StreamResolutionControlProps) {
  const options = streamResolutionOptions(launch, rungs);
  // Off-ladder value possible (admin/API-set, or rungs changed live) — show it
  // rather than let the select disagree with the session (as RenderResolutionControl).
  if (!options.some((o) => o.w === value.w && o.h === value.h)) {
    options.push({ w: value.w, h: value.h, label: sizeLabel(value.w, value.h) });
  }

  // "Auto" = release; sending the launch size is how the contract expresses that
  // (control-api.md — no separate release field). Selected when the ladder owns the size.
  const autoFirst = { value: AUTO_OPTION_VALUE, label: `Auto (host decides)` };
  const selected = owner === "auto" ? AUTO_OPTION_VALUE : optionValue(value.w, value.h);

  return (
    <select
      className="session-scaling-select"
      value={selected}
      disabled={busy || disabledReason != null}
      title={disabledReason}
      onChange={(e) => {
        if (e.target.value === AUTO_OPTION_VALUE) {
          onChange({ w: launch.w, h: launch.h }); // release = PATCH the launch size
          return;
        }
        const picked = options.find((o) => optionValue(o.w, o.h) === e.target.value);
        if (picked) onChange({ w: picked.w, h: picked.h });
      }}
      aria-label="Stream resolution"
    >
      <option value={autoFirst.value}>{autoFirst.label}</option>
      {options.map((o) => (
        <option key={optionValue(o.w, o.h)} value={optionValue(o.w, o.h)}>
          {o.label}
        </option>
      ))}
    </select>
  );
}
