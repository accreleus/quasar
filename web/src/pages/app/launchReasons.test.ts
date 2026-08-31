// Eligibility reasons, in the user's words (UX assessment §2.6).
//
// `GET /v1/me/profiles` attaches a `reasons[]` of {code, message} to every
// verdict and the client used to drop all of it — a bare "RISKY" tag and a
// dead-end "no launchable stream profile" line. These tests pin the three
// properties that make the translation trustworthy:
//
//   · every code the control plane can emit (profile/eligibility.go's
//     ReasonCode block) has copy, and none of the copy is the wire's own
//     phrasing,
//   · a code this build has never seen DEGRADES rather than disappearing or
//     being printed raw — which is not hypothetical, the code set is
//     documented append-only,
//   · one code that means two different things at two severities says two
//     different things.

import { describe, expect, it } from "vitest";
import {
  blockingReasons,
  codecBasis,
  reasonSentence,
  reasonSentences,
} from "./launchOptions";
import type { ProfileReason, ProfilesResponse } from "../../api/types";

const r = (code: string, message = ""): ProfileReason => ({ code, message }) as ProfileReason;

/** Every code in control-plane/internal/profile/eligibility.go. If the control
 *  plane appends one, this list is where the client notices. */
const CONTRACT_CODES = [
  "bandwidth_too_low",
  "rtt_too_high",
  "decode_height_too_low",
  "codec_not_supported",
  "host_encoder_not_supported",
  "display_refresh_unknown",
  "display_refresh_too_low",
  "browser_playout_unsupported",
  "historical_client_performance_failed",
  "probe_missing",
  "probe_stale",
];

describe("reasonSentence", () => {
  it("gives every contract code its own sentence, in the user's words", () => {
    const seen = new Set<string>();
    for (const code of CONTRACT_CODES) {
      // The wire message is deliberately present and deliberately ignored: a
      // recognised code must never fall through to operator phrasing.
      const s = reasonSentence(r(code, "measured bandwidth is below the profile minimum"));
      expect(s, code).not.toContain("profile minimum");
      expect(s, code).not.toContain("rung");
      expect(s, code).toMatch(/[.!?]$/);
      expect(s.length, code).toBeGreaterThan(20);
      expect(seen.has(s), `${code} duplicates another code's copy`).toBe(false);
      seen.add(s);
    }
  });

  it("says something different for a risky bandwidth verdict than an ineligible one", () => {
    // eligibility.go emits `bandwidth_too_low` twice — hard under the minimum,
    // soft under the recommended headroom. "This will not run" and "this may
    // wobble" are not the same news.
    const hard = reasonSentence(r("bandwidth_too_low"), "ineligible");
    const soft = reasonSentence(r("bandwidth_too_low"), "risky");
    expect(soft).not.toBe(hard);
    expect(hard).toMatch(/slower than/i);
    expect(soft).toMatch(/wobble|room to spare/i);
  });

  it("falls back to the same wording for a code with no soft variant", () => {
    expect(reasonSentence(r("decode_height_too_low"), "risky")).toBe(
      reasonSentence(r("decode_height_too_low"), "ineligible"),
    );
  });

  it("degrades an UNKNOWN code to the server's own message, as a sentence", () => {
    const s = reasonSentence(r("thermal_budget_exceeded", "the host GPU is thermally throttled"));
    expect(s).toBe("The host GPU is thermally throttled.");
  });

  it("keeps an unknown code's message punctuation rather than doubling it", () => {
    expect(reasonSentence(r("some_new_code", "Try again later!"))).toBe("Try again later!");
  });

  it("still says something when an unknown code carries no message at all", () => {
    const s = reasonSentence(r("some_new_code", ""));
    expect(s).not.toBe("");
    // Honest, and never the raw code.
    expect(s).not.toContain("some_new_code");
    expect(s).toMatch(/flagged/i);
  });
});

describe("reasonSentences", () => {
  it("dedupes by code — one verdict repeated across rungs is one sentence", () => {
    const lines = reasonSentences([
      r("codec_not_supported"),
      r("codec_not_supported"),
      r("bandwidth_too_low"),
    ]);
    expect(lines).toHaveLength(2);
  });

  it("returns nothing for an absent or empty reasons array", () => {
    expect(reasonSentences(undefined)).toEqual([]);
    expect(reasonSentences([])).toEqual([]);
  });

  it("carries the severity through to each sentence", () => {
    expect(reasonSentences([r("bandwidth_too_low")], "risky")[0]).toBe(
      reasonSentence(r("bandwidth_too_low"), "risky"),
    );
  });
});

describe("blockingReasons", () => {
  const profilesResponse = (): ProfilesResponse =>
    ({
      recommended_id: "1080p60",
      confidence: "low",
      notes: [r("probe_missing", "no device probe available; network not measured")],
      profiles: [
        {
          id: "4k60",
          display_name: "4K 60",
          description: "",
          nominal: { width: 3840, height: 2160, fps: 60, bitrate_kbps: 25000 },
          eligibility: "ineligible",
          reasons: [r("decode_height_too_low", "client decode height is below the profile resolution")],
          rungs: [
            {
              position: 1,
              eligibility: "ineligible",
              reasons: [r("decode_height_too_low", "…")],
            },
            {
              position: 2,
              eligibility: "ineligible",
              reasons: [r("codec_not_supported", "client does not accept this rung's codec")],
            },
          ],
        },
      ],
    }) as unknown as ProfilesResponse;

  it("collects the chain's own reasons, its LOWER rungs' reasons, and the notes", () => {
    // A launch profile's `reasons` carry only its TOP rung's verdict, so the
    // rest of the answer to "why can nothing run" is down the chain.
    expect(blockingReasons(profilesResponse()).map((x) => x.code)).toEqual([
      "decode_height_too_low",
      "codec_not_supported",
      "probe_missing",
    ]);
  });

  it("is empty for a response with nothing to say", () => {
    expect(
      blockingReasons({
        recommended_id: "x",
        confidence: "high",
        notes: [],
        profiles: [],
      } as unknown as ProfilesResponse),
    ).toEqual([]);
  });
});

describe("codecBasis", () => {
  it("gives each codec segment a distinct reason to pick it", () => {
    const all = (["auto", "h264", "hevc", "av1"] as const).map(codecBasis);
    expect(new Set(all).size).toBe(4);
    for (const s of all) expect(s).toMatch(/[.!?]$/);
    expect(codecBasis("h264")).toMatch(/every device/i);
    expect(codecBasis("hevc")).toMatch(/less bandwidth/i);
    expect(codecBasis("av1")).toMatch(/efficient/i);
  });
});
