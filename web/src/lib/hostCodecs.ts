// Codec truth-telling for the host card (wizard-v2 §S5: "tell the truth, do
// not add a toggle"). Codecs are not ship-dark — migration 0046 already chains
// AV1 → HEVC → H.264 in every ≥1080p launch profile; the only gap was that
// nothing said which codecs a HOST can produce.

import { codecDisplayName } from "./codecDisplay";

const ALL_WIRE_CODECS = ["h264", "h265", "av1"] as const;

export interface CodecGap {
  /** Which of the three wire codecs the host did NOT report. */
  missing: string[];
  /** One sentence naming the specific, actionable cause — never implies
   *  misconfiguration when the real cause is "no encoder element for it". */
  reason: string;
}

/**
 * Explains a host's missing codecs (wire vocab h264|h265|av1, not the
 * catalog's `hevc`); null when complete or not-yet-reported. The one
 * operator-checkable knob is HEVC on a Vulkan host (QUASAR_VULKAN_HEVC,
 * default on — so h264-only means an explicit =0 or a missing element; the
 * agent's "vulkan codec plan" startup line says which). Every other gap is an
 * element/registry fact, never operator misconfiguration.
 */
export function explainCodecGap(
  codecs: string[] | null | undefined,
  encoder: string | null | undefined,
): CodecGap | null {
  if (!codecs || codecs.length === 0) return null; // "not reported", handled separately by the caller
  const have = new Set(codecs);
  const missing = ALL_WIRE_CODECS.filter((c) => !have.has(c));
  if (missing.length === 0) return null;

  const missingLabel = missing.map((c) => codecDisplayName(c)).join(" and ");

  if (have.size === 1 && have.has("h264") && encoder === "vulkan" && missing.includes("h265")) {
    // The one real, findable, one-line fix (S5's specific example).
    const av1Note = missing.includes("av1")
      ? " AV1 is on by default too; when a Vulkan host cannot produce it, sessions fall back to the vendor AV1 encoder, which this host does not have either."
      : "";
    return {
      missing,
      reason:
        `This host reports H.264 only. HEVC runs on the Vulkan encoder by default, so either ` +
        `QUASAR_VULKAN_HEVC is set to 0 on this host's agent, or its vulkanh265enc element is not ` +
        `registered. The agent's "vulkan codec plan" startup log line says which.${av1Note}`,
    };
  }

  return {
    missing,
    reason:
      `This host does not report ${missingLabel}. That means the encoder or RTP payloader element ` +
      `for ${missing.length > 1 ? "those codecs is" : "that codec is"} not registered on this host — ` +
      `not a setting to flip. This is a host/driver capability gap, separate from the catalog (which ` +
      `already chains AV1 → HEVC → H.264 and will simply skip what this host cannot produce).`,
  };
}
