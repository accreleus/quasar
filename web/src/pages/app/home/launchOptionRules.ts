// The launch-options panel's three columns, its verdict line, the band's
// recommendation line and the healing of an edited draft. Pure. Rows, bitrates
// and availability come from the rung table, not from the mock's constants.

import type { AppDisplayStream } from "../../../api/types";
import {
  availableFps,
  availableResolutions,
  codecBasis,
  codecLabel,
  fpsUniverse,
  reasonSentences,
  resolveSelection,
  type DraftCodec,
  type LaunchDraft,
  type OptionSpace,
  type ResolutionRow,
} from "../launchOptions";

/** #525: what a policy-pinned app's frozen rows say for themselves. */
export const PINNED_NOTE = "Fixed by this app, it always launches at its own setting.";

/** Fallback when a verdict or a row carries no reason at all — `reasons[]` is
 *  legal but empty on the wire. */
export const UNEXPLAINED =
  "Your host flagged this option without saying why. If it won't start, pick a lower quality.";

export interface OptionRow<V> {
  value: V;
  /** `.qr-label` */
  label: string;
  /** `.qr-sub`, absent when there is nothing to add. */
  sub?: string;
  enabled: boolean;
  /** Native tooltip on a disabled row — why it cannot be picked. */
  title?: string;
  selected: boolean;
  /** `.qp-tag`s, in the mock's order. Both can be true of one row: the server
   *  recommends the lowest-demand entry even when nothing is fully eligible
   *  (profile/launch.go recommendLaunch), and hiding the risk behind the
   *  recommendation would suppress the warning on the row users are steered
   *  toward. */
  tags?: Array<"recommended" | "risky">;
  /** A risky row's reason, printed under its sub-line and on the tag. An
   *  ineligible row carries its reason in `sub` instead: it has no spec line
   *  worth printing. */
  why?: string;
}

export interface OptionColumns {
  codec: OptionRow<DraftCodec>[];
  fps: OptionRow<number>[];
  /** Keyed by height, which is what a draft carries. */
  resolution: OptionRow<number>[];
  /** The `.seg-hint` under each column, absent when the column has nothing to
   *  explain. */
  codecHint: string;
  fpsHint?: string;
  resolutionHint?: string;
  /** Present only while the app's profile is policy-pinned. */
  pinnedNote?: string;
}

export interface ColumnOptions {
  /** #525: the app pins its launch profile and the server refuses any other. */
  pinned?: boolean;
}

export interface VerdictOptions {
  /** `ProfilesResponse.confidence`: whether the network was actually measured. */
  confidence?: "high" | "low" | string;
}

export interface Verdict {
  tone: "ok" | "risky" | "off";
  text: string;
}

/** "4K" / "1440p" / "1080p" — the row label, from the rung's own height. */
export function resolutionLabel(height: number): string {
  if (height >= 2160) return "4K";
  if (height >= 1440) return "1440p";
  if (height >= 1080) return "1080p";
  if (height >= 720) return "720p";
  if (height >= 480) return "480p";
  return `${height}p`;
}

/** kbps → "12 Mbps" / "800 Kbps", one decimal place at most. */
export function formatBitrate(kbps: number): string {
  if (kbps >= 1000) {
    const mbps = kbps / 1000;
    const rounded = Math.round(mbps * 10) / 10;
    return `${Number.isInteger(rounded) ? rounded.toFixed(0) : rounded.toFixed(1)} Mbps`;
  }
  return `${kbps} Kbps`;
}

/** `.qp-tag`s for a row, in the mock's order. */
function tagsFor(row: ResolutionRow): { tags?: Array<"recommended" | "risky"> } {
  const tags: Array<"recommended" | "risky"> = [];
  if (row.recommended) tags.push("recommended");
  if (row.eligibility === "risky") tags.push("risky");
  return tags.length > 0 ? { tags } : {};
}

/** The row the draft currently points at, or undefined for a combination with
 *  no rung behind it. */
function rowFor(space: OptionSpace, draft: LaunchDraft): ResolutionRow | undefined {
  return availableResolutions(space, draft.codec, draft.fps).find((r) => r.height === draft.height);
}

/** The recommended row's height for this codec+fps, when it has one. */
function recommendedHeight(space: OptionSpace, draft: LaunchDraft): number | null {
  const rec = availableResolutions(space, draft.codec, draft.fps).find(
    (r) => r.recommended && r.selectable,
  );
  return rec?.height ?? null;
}

/**
 * The three columns for the current draft. Every column always renders its full
 * universe: a frame rate or a resolution the chosen codec cannot reach stays on
 * screen, disabled and explained, because a row that vanishes teaches nobody
 * why it went.
 */
export function optionColumns(
  space: OptionSpace,
  draft: LaunchDraft,
  { pinned = false }: ColumnOptions = {},
): OptionColumns {
  const codecName = codecLabel(draft.codec);
  const auto = draft.codec === "auto";
  // On Auto there is no chosen codec to blame, so a dead row names itself
  // instead: "not available with Auto" reads as a fault of the setting.
  const unavailable = (what: string) =>
    auto
      ? `Not available at ${what} on this device`
      : `Not available with ${codecName} on this device`;
  const rows = availableResolutions(space, draft.codec, draft.fps);
  const fpsAvailable = new Set(availableFps(space, draft.codec));

  const codec: OptionRow<DraftCodec>[] = space.codecs.map((c) => ({
    value: c,
    label: codecLabel(c),
    // The codec column stays live even on a pinned app: a `stream.codec`-only
    // override is accepted on a forced profile (control-plane launcher.go).
    enabled: true,
    selected: draft.codec === c,
  }));

  const fps: OptionRow<number>[] = fpsUniverse(space).map((value) => {
    const has = fpsAvailable.has(value);
    // Each row is priced at its own frame rate, so the column compares rates
    // rather than repeating the selected row's cost.
    const at = availableResolutions(space, draft.codec, value).find(
      (r) => r.height === draft.height,
    );
    return {
      value,
      label: `${value} fps`,
      sub: has
        ? at?.bitrateKbps !== undefined
          ? `${formatBitrate(at.bitrateKbps)} at ${resolutionLabel(draft.height)}`
          : undefined
        : auto
          ? "Not available on this device"
          : `Not available with ${codecName}`,
      enabled: has && !pinned,
      title: pinned ? PINNED_NOTE : has ? undefined : unavailable(`${value} fps`),
      selected: draft.fps === value,
    };
  });

  const resolution: OptionRow<number>[] = rows.map((row) => {
    const label = resolutionLabel(row.height);
    const pixels = `${row.width}×${row.height}`;
    // An ineligible row replaces its spec line with the reason it cannot run;
    // "no rung at all" is a different fact and says that instead.
    const why =
      row.eligibility && row.eligibility !== "eligible"
        ? reasonSentences(row.reasons, row.eligibility)[0] ?? UNEXPLAINED
        : null;
    const missing = auto
      ? `${pixels} · not available at ${draft.fps} fps`
      : `${pixels} · not available with ${codecName} at ${draft.fps} fps`;
    const sub = !row.available
      ? missing
      : row.eligibility === "ineligible"
        ? why ?? missing
        : `${pixels} · ${formatBitrate(row.bitrateKbps ?? 0)} at ${draft.fps} fps`;
    return {
      value: row.height,
      label,
      sub,
      enabled: row.selectable && !pinned,
      title: pinned ? PINNED_NOTE : row.selectable ? undefined : unavailable(label),
      selected: draft.height === row.height,
      ...tagsFor(row),
      ...(row.eligibility === "risky" && why ? { why } : {}),
    };
  });

  // Auto also says what it lands on for the drafted row: "the host picks" is
  // only half an answer when the row in front of you has already resolved.
  const landsOn = rowFor(space, draft)?.entryCodec;
  const codecHint = auto
    ? `${codecBasis("auto")}${landsOn ? ` Here it lands on ${codecLabel(landsOn)}.` : ""}`
    : codecBasis(draft.codec);

  if (pinned) {
    return {
      codec,
      fps,
      resolution,
      codecHint,
      fpsHint: PINNED_NOTE,
      resolutionHint: PINNED_NOTE,
      pinnedNote: PINNED_NOTE,
    };
  }
  return {
    codec,
    fps,
    resolution,
    codecHint,
    // The mock says "a rate your device cannot decode"; here a missing row is
    // usually a rung the catalogue does not have, which is the honest half.
    // Nothing to explain when every rate is offered.
    ...(fps.some((r) => !r.enabled)
      ? { fpsHint: "A frame rate this codec cannot reach is shown but cannot be picked." }
      : {}),
  };
}

/**
 * The `.qp-foot` sentence for the current draft. The mock's four cases, filled
 * from the matching rung rather than from its bitrate table — and where the
 * server gave a reason, that reason is the sentence: it is more specific than
 * anything this file could compose.
 */
export function verdict(
  space: OptionSpace,
  draft: LaunchDraft,
  { confidence = "high" }: VerdictOptions = {},
): Verdict {
  const row = rowFor(space, draft);
  const label = resolutionLabel(draft.height);
  const codecName = codecLabel(draft.codec);

  if (!row?.available) {
    const missing =
      draft.codec === "auto"
        ? `${label} is not available at ${draft.fps} fps on this device.`
        : `${label} is not available with ${codecName} at ${draft.fps} fps.`;
    return { tone: "off", text: `${missing} Pick a lower resolution or frame rate.` };
  }
  if (row.eligibility === "ineligible") {
    return { tone: "off", text: reasonSentences(row.reasons, "ineligible")[0] ?? UNEXPLAINED };
  }
  if (row.eligibility === "risky") {
    return { tone: "risky", text: reasonSentences(row.reasons, "risky")[0] ?? UNEXPLAINED };
  }

  const measured = confidence === "high";
  if (row.recommended) {
    return {
      tone: "ok",
      text: measured
        ? `Recommended for this device. Your measured network carries ${label} at ${draft.fps} fps with room to spare.`
        : `Recommended for this device. Your network has not been measured yet, so that is a conservative estimate.`,
    };
  }
  const cost = formatBitrate(row.bitrateKbps ?? 0);
  return {
    tone: "ok",
    text: measured
      ? `${label} at ${draft.fps} fps needs ${cost}, which your measured network carries comfortably.`
      : `${label} at ${draft.fps} fps needs ${cost}. Your network has not been measured yet, so that is an estimate.`,
  };
}

export interface RecommendationInput {
  /** The `GET /v1/me/profiles` evaluation's state. */
  state: "loading" | "failed" | "loaded";
  /** Nothing in the response can stream here. */
  deadEnd: boolean;
  /** What the committed draft resolves to, or null when it resolves to nothing. */
  selection: { eligibility: string; recommended: boolean } | null;
  /** The committed draft's codec — only Auto can be the recommendation. */
  codec?: DraftCodec;
  /** The catalogue's recommended row, named when the user has moved off it. */
  recommended?: { height: number; fps: number } | null;
}

/** The band's `.d-rec` line: one sentence for the committed selection. */
export function recommendation({
  state,
  deadEnd,
  selection,
  codec,
  recommended,
}: RecommendationInput): Verdict {
  if (state === "failed") return { tone: "off", text: "Could not load stream profiles." };
  if (state === "loading") return { tone: "ok", text: "Loading recommendation…" };
  if (deadEnd) return { tone: "off", text: "Nothing in this list can stream to this device." };
  if (selection?.eligibility === "risky") {
    return { tone: "risky", text: "This quality may not hold up on this device." };
  }
  if (codec === "auto" && selection?.recommended) {
    return { tone: "ok", text: "Recommended for this device." };
  }
  return {
    tone: "ok",
    text: recommended
      ? `Custom setting. The recommendation is ${resolutionLabel(recommended.height)} at ${recommended.fps} fps.`
      : "Custom setting.",
  };
}

/** Which column the user just touched. Healing keeps that column's choice and
 *  moves the others. */
export type EditedColumn = "codec" | "fps" | "resolution";

/**
 * Snaps a draft back onto the catalogue after an edit. A disabled row cannot be
 * clicked, so this only ever runs for the columns the edit invalidated:
 * changing codec can strip the current frame rate and resolution; choosing a
 * resolution the current frame rate cannot reach moves the frame rate, because
 * the resolution is the thing the user just asked for.
 */
export function heal(space: OptionSpace, draft: LaunchDraft, edited: EditedColumn): LaunchDraft {
  let fps = draft.fps;
  const height = draft.height;

  if (edited === "resolution") {
    const reachable = fpsUniverse(space).filter((f) =>
      availableResolutions(space, draft.codec, f).some((r) => r.height === height && r.selectable),
    );
    if (reachable.length > 0 && !reachable.includes(fps)) {
      fps = reachable.reduce((a, b) => (Math.abs(b - fps) < Math.abs(a - fps) ? b : a));
    }
    return { codec: draft.codec, fps, height };
  }

  const fpsOptions = availableFps(space, draft.codec);
  if (fpsOptions.length > 0 && !fpsOptions.includes(fps)) {
    fps = fpsOptions.reduce((a, b) => (Math.abs(b - fps) < Math.abs(a - fps) ? b : a));
  }

  const rows = availableResolutions(space, draft.codec, fps);
  if (rows.some((r) => r.height === height && r.selectable)) {
    return { codec: draft.codec, fps, height };
  }
  // Prefer the recommendation, then the tallest row that can actually run.
  const rec = recommendedHeight(space, { codec: draft.codec, fps, height });
  const fallback = rows.find((r) => r.selectable)?.height ?? rows[0]?.height ?? height;
  return { codec: draft.codec, fps, height: rec ?? fallback };
}

export interface LaunchSpec {
  resolution: string;
  fps: string;
  bitrate: string;
  codec: string;
}

/**
 * The four values the band's `.d-specs` strip and the panel's `.qp-spec` both
 * print. `fallback` is the app's advertised stream, used when the draft
 * resolves to nothing — the strip must never render blanks.
 */
export function launchSpec(
  space: OptionSpace,
  draft: LaunchDraft,
  fallback?: AppDisplayStream,
): LaunchSpec {
  const resolved = resolveSelection(space, draft);
  const width = resolved?.width ?? fallback?.width ?? 0;
  const height = resolved?.height ?? fallback?.height ?? 0;
  const fps = resolved?.fps ?? fallback?.fps ?? 0;
  const kbps = resolved?.bitrateKbps ?? fallback?.bitrate_kbps ?? 0;
  // "Auto" names what it picked: the wire still carries no codec, but a user
  // reading the strip is owed the answer.
  const codec =
    draft.codec === "auto"
      ? resolved
        ? `Auto · ${codecLabel(resolved.entryCodec)}`
        : "Auto"
      : codecLabel(draft.codec);
  return {
    resolution: `${width}×${height}`,
    fps: `${fps} fps`,
    bitrate: formatBitrate(kbps),
    codec,
  };
}
