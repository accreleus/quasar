import { describe, expect, it } from "vitest";
import type { LaunchProfileEvaluation, ProfileEvaluation, ProfileReason, ProfilesResponse } from "../../api/types";
import type { CodecCapabilities } from "../../webrtc/capability";
import {
  availableFps,
  availableResolutions,
  buildOptionSpace,
  codecLabel,
  defaultDraft,
  fpsUniverse,
  isRecommendationEligible,
  normalizeDraft,
  recommendedProfile,
  resolveSelection,
  toWireCodec,
} from "./launchOptions";

// ── Fixture modelled on migration 0046's default catalog ────────────────────
//
// Real chain shapes (control-plane/migrations/0046_default_profile_catalog.up.sql):
//   4k120    -> 4k120-av1, 4k120-hevc, 1440p120-av1, 1080p120-h264
//   1440p120 -> 1440p120-av1, 1440p120-hevc, 1080p120-av1, 1080p120-h264
//   1080p120 -> 1080p120-av1, 1080p120-hevc, 1080p120-h264
//   720p120  -> 720p120-h264
// (60/90 fps chains mirror the same shape at their own fps — one fps/codec
// pair per test keeps the fixture readable; the module has no fps-specific
// branches so this generalises.)

const OK: ProfileReason[] = [];

function rung(
  id: string,
  codec: "h264" | "hevc" | "av1",
  width: number,
  height: number,
  fps: number,
  position: number,
  bitrateKbps: number,
  eligibility: ProfileEvaluation["eligibility"] = "eligible",
  reasons: ProfileReason[] = OK,
): ProfileEvaluation {
  return {
    id,
    display_name: id,
    codec,
    width,
    height,
    fps,
    h264_profile: codec === "h264" ? "constrained-baseline" : "main",
    nominal_bitrate_kbps: bitrateKbps,
    min_offer_bandwidth_kbps: Math.round(bitrateKbps * 1.2),
    recommended_offer_bandwidth_kbps: Math.round(bitrateKbps * 1.5),
    headroom_factor: 1.5,
    abr_floor_kbps: Math.round(bitrateKbps * 0.4),
    max_startup_rtt_ms: 40,
    position,
    eligibility,
    reasons,
  } as ProfileEvaluation;
}

function profile(
  id: string,
  rungs: ProfileEvaluation[],
  eligibility: LaunchProfileEvaluation["eligibility"] = "eligible",
  reasons: ProfileReason[] = OK,
): LaunchProfileEvaluation {
  const top = rungs[0];
  return {
    id,
    display_name: id,
    description: "",
    nominal: { width: top.width, height: top.height, fps: top.fps, bitrate_kbps: top.nominal_bitrate_kbps },
    eligibility,
    reasons,
    rungs,
  };
}

/** 120fps slice of the 0046 catalog: 4k120, 1440p120, 1080p120, 720p120. */
function catalog120(): LaunchProfileEvaluation[] {
  return [
    profile("4k120", [
      rung("4k120-av1", "av1", 3840, 2160, 120, 1, 18000),
      rung("4k120-hevc", "hevc", 3840, 2160, 120, 2, 21500),
      rung("1440p120-av1", "av1", 2560, 1440, 120, 3, 10000),
      rung("1080p120-h264", "h264", 1920, 1080, 120, 4, 11500),
    ]),
    profile("1440p120", [
      rung("1440p120-av1", "av1", 2560, 1440, 120, 1, 10000),
      rung("1440p120-hevc", "hevc", 2560, 1440, 120, 2, 11500),
      rung("1080p120-av1", "av1", 1920, 1080, 120, 3, 6500),
      rung("1080p120-h264", "h264", 1920, 1080, 120, 4, 11500),
    ]),
    profile("1080p120", [
      rung("1080p120-av1", "av1", 1920, 1080, 120, 1, 6500),
      rung("1080p120-hevc", "hevc", 1920, 1080, 120, 2, 7500),
      rung("1080p120-h264", "h264", 1920, 1080, 120, 3, 11500),
    ]),
    profile("720p120", [rung("720p120-h264", "h264", 1280, 720, 120, 1, 6500)]),
  ];
}

const ALL_CODECS: CodecCapabilities = { h264: true, hevc: true, av1: true, vp9: false };
const H264_ONLY: CodecCapabilities = { h264: true, hevc: false, av1: false, vp9: false };

describe("buildOptionSpace", () => {
  it("offers auto + every catalog codec the client can decode", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    expect(space.codecs).toEqual(["auto", "h264", "hevc", "av1"]);
  });

  it("hides a codec entirely (not merely disables it) when the client cannot decode it", () => {
    const space = buildOptionSpace(catalog120(), H264_ONLY, "1080p120");
    expect(space.codecs).toEqual(["auto", "h264"]);
    expect(space.entriesByCodec.has("hevc")).toBe(false);
    expect(space.entriesByCodec.has("av1")).toBe(false);
  });

  it("auto rows echo each launch profile's nominal + top-rung codec", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const auto = space.entriesByCodec.get("auto")!;
    const fourK = auto.find((e) => e.profileId === "4k120")!;
    expect(fourK).toMatchObject({ codec: "av1", width: 3840, height: 2160, fps: 120, bitrateKbps: 18000 });
    const recommended = auto.filter((e) => e.recommended);
    expect(recommended.map((e) => e.profileId)).toEqual(["1080p120"]);
  });

  it("explicit-codec rows use each profile's FIRST rung of that codec (clamp-0 mirror)", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const av1 = space.entriesByCodec.get("av1")!;
    // 4k120's first av1 rung is 4k120-av1 itself (position 1) -> a 4K row.
    expect(av1.some((e) => e.profileId === "4k120" && e.height === 2160)).toBe(true);
    // 1440p120's first av1 rung is its own top rung -> a 1440p row mapped to
    // the 1440p120 profile, NOT deduped away by 4k120's 1440p step-down rung.
    expect(av1.some((e) => e.profileId === "1440p120" && e.height === 1440)).toBe(true);
    // 1080p120's first av1 rung -> a 1080p row.
    expect(av1.some((e) => e.profileId === "1080p120" && e.height === 1080)).toBe(true);
    // 720p120 has no av1 rung at all -> no 720p row under the av1 segment.
    expect(av1.some((e) => e.height === 720)).toBe(false);
  });

  it("h264 rows never reach above 1080p — no native h264 rung exists there (spec §2)", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const h264 = space.entriesByCodec.get("h264")!;
    expect(Math.max(...h264.map((e) => e.height))).toBe(1080);
  });

  it("dedupes explicit-codec rows landing on the identical rung", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const h264 = space.entriesByCodec.get("h264")!;
    // 4k120, 1440p120 AND 1080p120 all fall through to 1080p120-h264 as their
    // first h264 rung — exactly one row should survive, credited to the
    // NATURAL chain (1080p120, whose own nominal matches the rung) so the
    // launched session's profile_id is honest for observability, not to
    // 4k120 which merely came first in preference order.
    const at1080 = h264.filter((e) => e.height === 1080 && e.fps === 120);
    expect(at1080).toHaveLength(1);
    expect(at1080[0].profileId).toBe("1080p120");
  });
});

describe("fpsUniverse / availableFps", () => {
  it("fpsUniverse spans every codec's fps values", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    expect(fpsUniverse(space)).toEqual([120]);
  });

  it("availableFps narrows to what a specific codec actually offers", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    expect(availableFps(space, "auto")).toEqual([120]);
    expect(availableFps(space, "h264")).toEqual([120]);
  });
});

describe("availableResolutions", () => {
  it("marks a resolution unavailable (not merely ineligible) when no rung exists for it", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const rows = availableResolutions(space, "h264", 120);
    const at4k = rows.find((r) => r.height === 2160)!;
    expect(at4k.available).toBe(false);
    expect(at4k.selectable).toBe(false);
  });

  it("keeps a risky row selectable (only ineligible + unavailable disable)", () => {
    const catalog = catalog120();
    catalog[0].rungs[0] = { ...catalog[0].rungs[0], eligibility: "risky", reasons: [{ code: "r", message: "risky" }] };
    const space = buildOptionSpace(catalog, ALL_CODECS, "1080p120");
    const rows = availableResolutions(space, "av1", 120);
    const at4k = rows.find((r) => r.height === 2160)!;
    expect(at4k.available).toBe(true);
    expect(at4k.eligibility).toBe("risky");
    expect(at4k.selectable).toBe(true);
  });

  it("disables an ineligible row and carries its reason", () => {
    const catalog = catalog120();
    catalog[2] = { ...catalog[2], eligibility: "ineligible", reasons: [{ code: "codec_not_supported", message: "no HEVC decode" }] };
    const space = buildOptionSpace(catalog, ALL_CODECS, "1080p120");
    const rows = availableResolutions(space, "auto", 120);
    const at1080 = rows.find((r) => r.height === 1080)!;
    expect(at1080.selectable).toBe(false);
    expect(at1080.eligibility).toBe("ineligible");
  });

  it("resolution universe is stable across codecs even where a codec can't reach every row", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const autoHeights = availableResolutions(space, "auto", 120).map((r) => r.height);
    const h264Heights = availableResolutions(space, "h264", 120).map((r) => r.height);
    expect(h264Heights).toEqual(autoHeights); // same universe, different availability
  });
});

describe("normalizeDraft", () => {
  it("snaps fps to the nearest one the new codec actually offers", () => {
    // A minimal fixture where av1 exists ONLY at 60fps (catalog120's av1
    // rungs all sit at 120) — switching the codec segment to av1 must snap
    // fps down to the only value that codec actually has.
    const catalog: LaunchProfileEvaluation[] = [
      profile("1080p120", [rung("1080p120-h264", "h264", 1920, 1080, 120, 1, 11500)]),
      profile("1080p60", [rung("1080p60-av1", "av1", 1920, 1080, 60, 1, 4500)]),
    ];
    const space = buildOptionSpace(catalog, ALL_CODECS, "1080p120");
    expect(availableFps(space, "av1")).toEqual([60]);
    const normalized = normalizeDraft(space, { codec: "av1", fps: 120, height: 1080 });
    expect(normalized.fps).toBe(60);
  });

  it("snaps an unavailable resolution to the highest selectable one", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    // h264 tops out at 1080p — a draft claiming 4k must snap down.
    const normalized = normalizeDraft(space, { codec: "h264", fps: 120, height: 2160 });
    expect(normalized.height).toBe(1080);
  });

  it("leaves an already-valid draft untouched", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const draft = { codec: "av1" as const, fps: 120, height: 2160 };
    expect(normalizeDraft(space, draft)).toEqual(draft);
  });

  it("skips a risky-but-selectable resolution when normalizing (does not force off it)", () => {
    const catalog = catalog120();
    catalog[0].rungs[0] = { ...catalog[0].rungs[0], eligibility: "risky", reasons: [{ code: "r", message: "risky" }] };
    const space = buildOptionSpace(catalog, ALL_CODECS, "1080p120");
    const draft = { codec: "av1" as const, fps: 120, height: 2160 };
    expect(normalizeDraft(space, draft).height).toBe(2160);
  });
});

describe("resolveSelection", () => {
  it("resolves an auto draft to its profile id with codec=null (server decides)", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const resolved = resolveSelection(space, { codec: "auto", fps: 120, height: 1080 });
    expect(resolved).toMatchObject({ profileId: "1080p120", codec: null, width: 1920, height: 1080, fps: 120 });
  });

  it("resolves an explicit-codec draft to its rung's codec and mapped profile", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    const resolved = resolveSelection(space, { codec: "hevc", fps: 120, height: 2160 });
    expect(resolved).toMatchObject({ profileId: "4k120", codec: "hevc", width: 3840, height: 2160 });
  });

  it("returns null for a combo with no matching entry", () => {
    const space = buildOptionSpace(catalog120(), H264_ONLY, "1080p120");
    // "hevc" isn't even in the option space when undecodable.
    expect(resolveSelection(space, { codec: "hevc", fps: 120, height: 1080 })).toBeNull();
  });
});

describe("defaultDraft", () => {
  it("defaults to auto at the recommended profile's nominal", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "1080p120");
    expect(defaultDraft(space, "1080p120")).toEqual({ codec: "auto", fps: 120, height: 1080 });
  });

  it("falls back to the first non-ineligible auto row when the recommendation is phantom", () => {
    const space = buildOptionSpace(catalog120(), ALL_CODECS, "does-not-exist");
    const d = defaultDraft(space, "does-not-exist");
    const auto = space.entriesByCodec.get("auto")!;
    expect(auto.some((e) => e.height === d.height && e.fps === d.fps)).toBe(true);
  });

  it("returns a static fallback for zero launch profiles", () => {
    const space = buildOptionSpace([], ALL_CODECS, "anything");
    expect(defaultDraft(space, "anything")).toEqual({ codec: "auto", fps: 60, height: 1080 });
  });
});

describe("codecLabel / toWireCodec", () => {
  it("labels the segment buttons", () => {
    expect(codecLabel("auto")).toBe("Auto");
    expect(codecLabel("h264")).toBe("H.264");
    expect(codecLabel("hevc")).toBe("HEVC");
    expect(codecLabel("av1")).toBe("AV1");
  });

  it("maps catalog hevc to wire h265, and leaves everything else identity", () => {
    expect(toWireCodec("hevc")).toBe("h265");
    expect(toWireCodec("h264")).toBe("h264");
    expect(toWireCodec("av1")).toBe("av1");
  });
});

// ── Moved from ProfilePicker.test.tsx (B5) ───────────────────────────────────

function profilesResponse(over: Partial<ProfilesResponse> = {}): ProfilesResponse {
  return {
    recommended_id: "1080p120",
    confidence: "high",
    notes: [],
    profiles: catalog120(),
    ...over,
  };
}

describe("recommendedProfile / isRecommendationEligible", () => {
  it("resolves recommended_id to the matching profile", () => {
    const data = profilesResponse();
    expect(recommendedProfile(data)?.id).toBe("1080p120");
  });

  it("returns null for a phantom recommended_id", () => {
    const data = profilesResponse({ recommended_id: "not-a-real-id" });
    expect(recommendedProfile(data)).toBeNull();
  });

  it("is eligible only when the recommendation resolves AND is outright eligible", () => {
    expect(isRecommendationEligible(profilesResponse())).toBe(true);
    expect(isRecommendationEligible(profilesResponse({ recommended_id: "nope" }))).toBe(false);
    const risky = catalog120();
    risky[2] = { ...risky[2], eligibility: "risky" };
    expect(isRecommendationEligible(profilesResponse({ profiles: risky }))).toBe(false);
  });
});
