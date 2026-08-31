// Pure helpers for the Stream profiles tab (v3 handoff §A.17).
// A stream profile is ONE encode rung — see docs/design/plans/
// 2026-07-28-phase4-profile-restructure-respec.md §1.

import type { CatalogCodec, StreamProfile } from "../../../api/types";

/** Catalog vocabulary, best-codec-first — matches the mock's group order
 *  (AV1, then HEVC, then H.264: resolution rises with codec efficiency, so
 *  the "best" codec heads the page). */
export const CODEC_GROUPS: CatalogCodec[] = ["av1", "hevc", "h264"];

export const CODEC_GROUP_LABEL: Record<CatalogCodec, string> = {
  av1: "AV1",
  hevc: "H.265 / HEVC",
  h264: "H.264",
};

/** Live constraints an operator needs when deciding rungs for this codec —
 *  rendered as a `.hint` under each codec card's head; the mock carries no
 *  equivalent, so this is kept as extra information (handoff §A.17: "Keep
 *  the group notes if they carry information the mock lacks"). */
export const CODEC_GROUP_NOTE: Record<CatalogCodec, string> = {
  av1: "Decodes in Chrome on every platform. Needs an AV1 encoder on the host.",
  hevc: "Chrome only decodes this with hardware support, never on Linux.",
  h264: "Every client can decode it. Every launch profile must end with one of these.",
};

/** universal fallback (h264, neutral chip) vs hardware required (accent). */
export function codecFallbackLabel(codec: CatalogCodec): string {
  return codec === "h264" ? "universal fallback" : "hardware required";
}

/**
 * UX half of delete-while-in-use (the server 409s regardless; ON DELETE
 * RESTRICT is the backstop). Both dimensions matter: `session_count` too,
 * because sessions.stream_profile_id has no ON DELETE clause — used_by alone
 * left Delete enabled on rungs whose delete would raw-FK-violate.
 */
export function isStreamProfileInUse(profile: Pick<StreamProfile, "used_by" | "session_count">): boolean {
  return (profile.used_by?.length ?? 0) > 0 || (profile.session_count ?? 0) > 0;
}

/** Delete-disabled title: session-recorded rungs never free up; an in-use
 *  rung frees up once every launch profile drops it. */
export function deleteDisabledTitle(sessionCount: number): string {
  return sessionCount > 0
    ? "Recorded by past sessions as the rung they resolved to. It can no longer be deleted."
    : "In use. Remove it from every launch profile first.";
}

/** Group profiles by codec, preserving the server's within-group order and
 *  the CODEC_GROUPS preference order across groups. */
export function groupByCodec(profiles: StreamProfile[]): Map<CatalogCodec, StreamProfile[]> {
  const groups = new Map<CatalogCodec, StreamProfile[]>();
  for (const codec of CODEC_GROUPS) groups.set(codec, []);
  for (const p of profiles) {
    const bucket = groups.get(p.codec);
    if (bucket) bucket.push(p);
    else groups.set(p.codec, [p]);
  }
  return groups;
}
