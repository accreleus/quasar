/**
 * Codec display helpers shared by the admin drill-down and the in-session HUD.
 * Server-resolved wire codec (`session.stream.codec`, h264|h265|av1) vs the
 * browser-reported getStats mimeType ("video/H264") — a disagreement is how a
 * silent fallback or mis-negotiated m-line presents. The comparison must stay
 * in this one place: drifting copies would flag disagreement differently for
 * the same session on the two surfaces.
 */

/** Operator-facing name for a wire codec id. One token per codec: the
 *  session table and the drill-down chip are sized for it. */
const WIRE_LABELS: Record<string, string> = {
  h264: "H.264",
  h265: "HEVC",
  av1: "AV1",
};

export function codecDisplayName(codec: string | null | undefined): string | null {
  if (!codec) return null;
  return WIRE_LABELS[codec] ?? codec.toUpperCase();
}

/**
 * Normalise either vocabulary to a comparable wire id.
 * Accepts "video/H264", "H264", "h264", "hevc", "av01" and friends.
 */
export function normaliseCodec(raw: string | null | undefined): string | null {
  if (!raw) return null;
  const tail = raw.includes("/") ? raw.slice(raw.lastIndexOf("/") + 1) : raw;
  const key = tail.trim().toLowerCase();
  if (key === "h264" || key === "avc" || key === "avc1") return "h264";
  if (key === "h265" || key === "hevc" || key === "hvc1") return "h265";
  if (key === "av1" || key === "av01") return "av1";
  return key;
}

/**
 * Compare the server-resolved codec against what the browser reports.
 * `agrees` is null when either side is unknown — an unknown is not a mismatch,
 * and claiming one would cry wolf on every session before getStats resolves.
 */
export function compareCodecs(
  resolved: string | null | undefined,
  negotiated: string | null | undefined,
): { resolved: string | null; negotiated: string | null; agrees: boolean | null } {
  const r = normaliseCodec(resolved);
  const n = normaliseCodec(negotiated);
  return { resolved: r, negotiated: n, agrees: r && n ? r === n : null };
}

/**
 * Operator text for a `codec_decision.considered[].rejected_by` clamp. The
 * contract calls the enum open, so an unrecognised value passes through
 * rather than rendering as "unknown".
 */
const REJECTION_LABELS: Record<string, string> = {
  host_encoder: "the host cannot encode this codec",
  client_decode: "this device cannot decode this codec",
  decode_height: "above this device's decode resolution ceiling",
  decode_history: "this device previously failed to decode it",
  hardware_encoder: "the host has no hardware encoder",
  encoder_throughput: "the host's encoder is too slow for this codec at this tier",
  unknown_codec: "the rung names a codec the server does not recognise",
};

export function rungRejectionLabel(reason: string | null | undefined): string | null {
  if (!reason) return null;
  return REJECTION_LABELS[reason] ?? reason;
}

/**
 * How the dispatched rung came to be dispatched — the contract forbids
 * collapsing merit/override/floor (identical `stream.codec` in all three).
 * "override" skipped the clamps, "floor" shipped despite failing them (the
 * selected rung keeps its rejection reason). Override is checked first: a
 * forced codec cannot also be the floor (clamp 0 pre-empts the walk).
 */
export type CodecOutcome = "merit" | "override" | "floor";

interface DecisionLike {
  override?: string | null;
  floor?: boolean;
}

export function codecOutcome(decision: DecisionLike | null | undefined): CodecOutcome | null {
  if (!decision) return null;
  if (decision.override) return "override";
  if (decision.floor) return "floor";
  return "merit";
}

/** One-line operator explanation of an outcome. */
export function codecOutcomeSummary(decision: DecisionLike | null | undefined): string | null {
  const outcome = codecOutcome(decision);
  switch (outcome) {
    case "override":
      return `Forced by an operator override to ${
        codecDisplayName(normaliseCodec(decision?.override)) ?? decision?.override
      } — the device-decode, decode-history and hardware-encoder clamps were skipped, not passed.`;
    case "floor":
      return "No rung survived the clamp chain, so the guaranteed H.264 floor was dispatched — bypassing every clamp, including the one that rejected it.";
    case "merit":
      return "Resolved on merit: the first rung that survived every clamp.";
    default:
      return null;
  }
}
