// Codec truth-telling for the host card (wizard-v2 §S5: "tell the truth, do
// not add a toggle"). Codecs are not ship-dark — migration 0046 already chains
// AV1 → HEVC → H.264 in every ≥1080p launch profile; the only gap was that
// nothing said which codecs a HOST can produce.

import type { ReadinessCheck } from "../api/types";
import { codecDisplayName } from "./codecDisplay";

const ALL_WIRE_CODECS = ["h264", "h265", "av1"] as const;

export interface CodecGap {
  /** Which of the three wire codecs the host did NOT report. */
  missing: string[];
  /** One sentence naming the specific, actionable cause — never implies
   *  misconfiguration when the real cause is "no encoder element for it". */
  reason: string;
}

/** Explain the reported codec set without inferring driver compatibility from
 * a missing element. The agent owns compatibility policy and its explanation. */
export function explainCodecGap(
  codecs: string[] | null | undefined,
  encoder: string | null | undefined,
  readiness?: readonly ReadinessCheck[] | null,
): CodecGap | null {
  if (!codecs || codecs.length === 0) return null; // "not reported", handled separately by the caller
  const have = new Set(codecs);
  const missing = ALL_WIRE_CODECS.filter((c) => !have.has(c));
  if (missing.length === 0) return null;

  const compatibility = readiness?.find(
    (check) => check.id === "nvidia_vulkan_av1_compatibility" && check.status === "warn",
  );
  if (missing.includes("av1") && compatibility) {
    return {
      missing,
      reason: `${compatibility.summary} See Vulkan AV1 compatibility in Readiness for driver guidance.`,
    };
  }

  const missingLabel = missing.map((c) => codecDisplayName(c)).join(" and ");

  if (have.size === 1 && have.has("h264") && encoder === "vulkan" && missing.includes("h265")) {
    // The one real, findable, one-line fix (S5's specific example).
    const av1Note = missing.includes("av1")
      ? " AV1 may also be unavailable because of driver compatibility. Check Readiness for the agent’s diagnosis."
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
      `This host does not report ${missingLabel}. The encoder or RTP payloader element ` +
      `for ${missing.length > 1 ? "those codecs may be" : "that codec may be"} unavailable, or a driver compatibility check may have disabled it. Check Readiness for the agent’s diagnosis. This is ` +
      `a host/driver capability gap, separate from the catalog (which ` +
      `already chains AV1 → HEVC → H.264 and will simply skip what this host cannot produce).`,
  };
}
