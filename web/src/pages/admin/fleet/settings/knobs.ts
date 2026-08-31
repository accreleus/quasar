// Pure helpers for the host-settings knob catalog: label/help copy, grouping,
// and value coercion/formatting. No React, no fetch — HostSettings.tsx and its
// KnobRow/KnobControl subcomponents are the only callers.

import type { ConfigKnob } from "../../../../api/types";

export type SettingValue = boolean | number | string;
export type OverrideValue = SettingValue | null;
export type KnobGroup = "runtime" | "encoder" | "adaptation" | "advanced";

/** `effective` arrives as raw wire strings (the agent stringifies env values) —
 *  coerce to the knob's declared type so it compares/displays like `resolved`. */
export function coerceEffective(k: ConfigKnob, raw: string): SettingValue {
  if (k.type === "bool") return raw === "true" || raw === "1";
  if (k.type === "int" || k.type === "float") {
    const n = Number(raw);
    return Number.isFinite(n) ? n : raw;
  }
  return raw; // "enum" | "string"
}

export function valueLabel(v: SettingValue | undefined): string {
  if (v === undefined) return "Unset";
  if (typeof v === "boolean") return v ? "On" : "Off";
  return String(v);
}

export const KNOB_COPY: Record<string, { label: string; help: string; group: KnobGroup }> = {
  idle_timeout_secs: {
    label: "Idle timeout",
    help: "Stops idle sessions after this many seconds.",
    group: "runtime",
  },
  abr_enabled: {
    label: "Adaptive bitrate",
    help: "Deprecated. Use Adaptation mode instead; off still forces fixed bitrate.",
    group: "adaptation",
  },
  zerocopy: {
    label: "Zero-copy path",
    help: "Uses the experimental GPU memory path when supported.",
    group: "runtime",
  },
  latency_probe: {
    label: "Host latency probe",
    help: "Measurement instrument, not a tuning knob. Times each frame across the host pipeline stages and reports them per session. Leave off unless you are running a latency investigation.",
    group: "advanced",
  },
  home_root: {
    label: "Managed-home root (host path)",
    help: "Must be this host's agent-reported root or a subdirectory of it (the setup wizard's Host & GPU step offers a constrained control for this); empty = agent's QUASAR_HOME_ROOT env. A different root entirely needs QUASAR_HOME_ROOT in deploy/.env + a redeploy.",
    group: "runtime",
  },
  encoder: {
    label: "Encoder",
    help: "Selects the encoder backend used by new agent processes.",
    group: "encoder",
  },
  render_node: {
    label: "Render node",
    help: "GPU render device path, or software rendering.",
    group: "encoder",
  },
  cuda_device: {
    label: "CUDA device",
    help: "NVENC CUDA device index.",
    group: "encoder",
  },
  nvidia_lib32_path: {
    label: "32-bit NVIDIA libs (host path)",
    help: "Host directory holding 32-bit NVIDIA driver libs; auto-detected when empty",
    group: "encoder",
  },
  abr_floor_kbps: {
    label: "ABR floor",
    help: "Minimum adaptive bitrate before quality is considered unsustainable.",
    group: "adaptation",
  },
  abr_floor_ratio: {
    label: "ABR floor ratio",
    help: "Fallback floor as a ratio of the selected profile bitrate.",
    group: "adaptation",
  },
  abr_mode: {
    label: "Adaptation mode",
    help: "off = fixed bitrate; protective = classic step-down; smooth = encoder-aware, smoothness-biased (default). Replaces the older Adaptive bitrate switch.",
    group: "adaptation",
  },
  abr_ewma_alpha: {
    label: "Up-path smoothing",
    help: "EWMA smoothing factor for tracking the bandwidth estimate upward. Higher tracks up faster. Advanced governor tuning knob; default matches the previous hardcoded value.",
    group: "adaptation",
  },
  abr_deadband: {
    label: "Retarget deadband",
    help: "How far the estimate must move from the current setpoint before any retarget fires, as a fraction. Advanced governor tuning knob; default matches the previous hardcoded value.",
    group: "adaptation",
  },
  abr_max_up_step: {
    label: "Max up-step",
    help: "Largest fractional bitrate increase allowed per up-step. Advanced governor tuning knob; default matches the previous hardcoded value.",
    group: "adaptation",
  },
  abr_min_interval_ms: {
    label: "Min retarget interval",
    help: "Minimum time (ms) between retargets in either direction. This is the anti-thrash floor. Advanced governor tuning knob; default matches the previous hardcoded 2 s.",
    group: "adaptation",
  },
  abr_max_down_step: {
    label: "Max down-step (smooth)",
    help: "Smooth mode only. Largest fractional bitrate decrease per non-emergency downshift. Advanced governor tuning knob; default matches the previous hardcoded −12.5%.",
    group: "adaptation",
  },
  abr_down_dwell_ms: {
    label: "Down-step dwell (smooth)",
    help: "Smooth mode only. Minimum time (ms) between non-emergency downshifts. Emergency drops on confirmed congestion bypass it. Advanced governor tuning knob; default matches the previous hardcoded 7 s.",
    group: "adaptation",
  },
  abr_cliff_guard_frac: {
    label: "Cliff guard fraction (smooth)",
    help: "Smooth mode only. Under fresh encoder saturation, the bitrate is never driven below this fraction of the current setpoint in one move. Advanced governor tuning knob; default matches the previous hardcoded 50%.",
    group: "adaptation",
  },
  abr_ladder: {
    label: "Encoder speed ladder",
    help: "Lets a smooth-mode session bias the encoder toward speed when it cannot keep up. Off = that rung only; the resolution and fps ladders below have their own switches and are unaffected.",
    group: "adaptation",
  },
  abr_ladder_max_bias: {
    label: "Encoder speed steps",
    help: "How many steps the encoder may be biased toward speed when it cannot keep up. 0 disables that rung.",
    group: "adaptation",
  },
  abr_ladder_engage_dwell: {
    label: "Speed step-down dwell",
    help: "Consecutive 5s windows of encoder saturation before a speed step. Higher = slower to react, less twitchy.",
    group: "adaptation",
  },
  abr_ladder_recover_dwell: {
    label: "Speed step-up dwell",
    help: "Consecutive healthy 5s windows before a speed step is given back.",
    group: "adaptation",
  },
  abr_ladder_resolution: {
    label: "Resolution ladder",
    help: "Lets a session drop the STREAMED resolution when the bitrate can no longer carry the picture, and climb back when it recovers. Off by default until this host has been soak-tested.",
    group: "adaptation",
  },
  abr_ladder_res_exponent: {
    label: "Comfort bitrate exponent",
    help: "Bits-per-pixel power law. At 0.75 a 1440p120 @ 10 Mbps session is comfortable at 6.5 Mbps for 1080p, 4.9 for 900p, 3.5 for 720p. Lower = drops resolution sooner.",
    group: "adaptation",
  },
  abr_ladder_res_engage_frac: {
    label: "Resolution drop threshold",
    help: "Drop a rung when the bitrate falls below this fraction of the current rung's comfort bitrate (0.6 = 60%).",
    group: "adaptation",
  },
  abr_ladder_res_recover_frac: {
    label: "Resolution recovery threshold",
    help: "Climb a rung when the bitrate reaches this fraction of the next rung up's comfort bitrate (0.8 = 80%). Must stay at least 0.05 above the drop threshold.",
    group: "adaptation",
  },
  abr_ladder_res_engage_dwell: {
    label: "Resolution drop dwell",
    help: "Consecutive 5s windows below the drop threshold before a rung is dropped.",
    group: "adaptation",
  },
  abr_ladder_res_recover_dwell: {
    label: "Resolution recovery dwell",
    help: "Consecutive 5s windows above the recovery threshold before a rung is restored. Longer than the drop dwell so recovery is conservative.",
    group: "adaptation",
  },
  abr_ladder_res_min_step_s: {
    label: "Minimum seconds between resolution steps",
    help: "Hard floor on step frequency, in either direction.",
    group: "adaptation",
  },
  abr_ladder_res_min_height: {
    label: "Lowest streamed height",
    help: "The resolution ladder never goes below this many lines.",
    group: "adaptation",
  },
  abr_ladder_fps: {
    label: "Frame-rate ladder",
    help: "Lets a 120fps session drop to 60fps as a step before dropping resolution further. Works with or without the resolution ladder — on its own it is the only rung that steps. Needs the videorate element in this host's image; without it the rung reports itself unavailable (one warning in the agent log) and nothing else changes. Off until validated on this host.",
    group: "adaptation",
  },
  abr_ladder_floor_follows_rung: {
    label: "Floor follows the ladder",
    help: "When the ladder drops resolution or frame rate, drop the minimum bitrate with it. A smaller picture is comfortable on less, and without this the bitrate cannot fall far enough to stop overdriving a slow link. On by default; only takes effect while a ladder rung is engaged. An explicitly set ABR floor always wins.",
    group: "adaptation",
  },
  abr_ladder_order: {
    label: "Ladder order",
    help: "hybrid = resolution down to 1080p, then frame rate, then lower resolutions. res_first / fps_first are the two pure orders.",
    group: "adaptation",
  },
  gop: {
    label: "GOP length",
    help: "Keyframe interval for new sessions.",
    group: "advanced",
  },
  slices: {
    label: "Encoder slices",
    help: "Slice count passed to the encoder.",
    group: "advanced",
  },
  target_usage: {
    label: "Target usage",
    help: "Encoder speed and quality hint.",
    group: "advanced",
  },
  queue_buffers: {
    label: "Queue buffers",
    help: "Pipeline buffering depth before encode.",
    group: "advanced",
  },
};

export function groupFor(k: ConfigKnob): KnobGroup {
  return KNOB_COPY[k.key]?.group ?? (k.class === "restart" ? "encoder" : "advanced");
}

export function knobLabel(k: ConfigKnob): string {
  return KNOB_COPY[k.key]?.label ?? k.key.replaceAll("_", " ");
}

export function knobHelp(k: ConfigKnob): string {
  return KNOB_COPY[k.key]?.help ?? "Advanced node-agent runtime setting.";
}

/** Same keys, same values (`Object.is`) — used to derive dirty (draft vs. the
 *  last-persisted overrides map) without a JSON round-trip's key-order risk. */
export function shallowEqualOverrides(
  a: Record<string, OverrideValue>,
  b: Record<string, OverrideValue>,
): boolean {
  const aKeys = Object.keys(a);
  if (aKeys.length !== Object.keys(b).length) return false;
  return aKeys.every((k) => Object.is(a[k], b[k]));
}
