// Library hero band launch-options cascade (LibraryDetail.tsx is the only consumer).
// Derived from ONE `GET /v1/me/profiles` response — profiles[] are launch profiles,
// each an ordered chain of rungs; no invented CAPS matrix, a walk over real chains.
//
// "auto" codec segment: one row per profile, keyed by the profile's own nominal
// and profile-level eligibility (codec is whatever the top rung is; the server
// picks at launch). An explicit codec: one row per profile's FIRST rung matching
// that codec (mirrors the launch resolver's clamp-0 rule, control-plane rung.go),
// deduped by (width,height,fps); eligibility is rung-level here.
//
// fps segment and resolution list always render the FIXED union of rows across
// every codec, disabling combos the current selection doesn't reach.

import type {
  App,
  CatalogCodec,
  LaunchProfileEvaluation,
  ProfileEvaluation,
  ProfileReason,
  ProfilesResponse,
} from "../../api/types";
import type { CodecCapabilities } from "../../webrtc/capability";

/** The codec segment's selection space: "auto" plus the catalog vocabulary. */
export type DraftCodec = "auto" | CatalogCodec;

/** One cascade selection — codec, then fps, then resolution (by height, the
 * same bucket key `fmtResolution` groups on in libraryDetail.tsx). */
export interface LaunchDraft {
  codec: DraftCodec;
  fps: number;
  height: number;
}

/** One real (codec, height, fps) combo the catalog actually offers. */
interface OptionEntry {
  /** The rung's actual codec — equal to the segment codec for an explicit
   * segment, and the profile's top-rung codec for "auto" rows. */
  codec: CatalogCodec;
  width: number;
  height: number;
  fps: number;
  bitrateKbps: number;
  /** The launch profile id to send at commit. */
  profileId: string;
  eligibility: "eligible" | "risky" | "ineligible";
  reasons: ProfileReason[];
  /** "auto" rows only — true when this is `recommended_id`. */
  recommended: boolean;
}

/** The full derived option space for one `ProfilesResponse`. Opaque —
 * consumed only via the accessor functions below. */
export interface OptionSpace {
  /** Codec segment buttons, in display order: "auto" first, then whichever
   * of h264/hevc/av1 the catalog offers AND the client can decode, in
   * catalog order. A codec with zero resulting rows is not included. */
  codecs: DraftCodec[];
  entriesByCodec: Map<DraftCodec, OptionEntry[]>;
}

/** One resolution-list row, always present for every height in the panel's
 * universe regardless of whether the current codec+fps reaches it. */
export interface ResolutionRow {
  height: number;
  width: number;
  /** A rung/profile exists for this codec+height+fps at all. */
  available: boolean;
  /** available && not ineligible — this is what a click may select. */
  selectable: boolean;
  eligibility?: "eligible" | "risky" | "ineligible";
  reasons?: ProfileReason[];
  /** "auto" rows only. */
  recommended: boolean;
  bitrateKbps?: number;
  profileId?: string;
  /** The rung's actual codec (meaningful for "auto" rows, where it can
   * differ per row — a 4K row may resolve through an av1 top rung while a
   * 720p row is h264-only). */
  entryCodec?: CatalogCodec;
}

/** What committing a draft resolves to — echoed on the launch card and sent
 * to `launchSession`. `codec` is null for "auto" (the server decides). */
export interface ResolvedSelection {
  profileId: string;
  codec: CatalogCodec | null;
  entryCodec: CatalogCodec; // populated even for "auto" (`codec` above is null per wire contract)
  width: number;
  height: number;
  fps: number;
  bitrateKbps: number;
  eligibility: "eligible" | "risky" | "ineligible";
  reasons: ProfileReason[];
  recommended: boolean;
}

const CODEC_ORDER: readonly CatalogCodec[] = ["h264", "hevc", "av1"];

function decodable(codec: CatalogCodec, caps: CodecCapabilities): boolean {
  if (codec === "h264") return caps.h264;
  if (codec === "hevc") return caps.hevc;
  return caps.av1;
}

/** Rungs in preference order — `position` is 1-indexed, best first. */
function orderedRungs(profile: LaunchProfileEvaluation): ProfileEvaluation[] {
  return [...profile.rungs].sort((a, b) => a.position - b.position);
}

/**
 * Builds the option space for one `GET /v1/me/profiles` response, already
 * intersected with the client's decode probe.
 */
export function buildOptionSpace(
  profiles: LaunchProfileEvaluation[],
  codecCaps: CodecCapabilities,
  recommendedId: string,
): OptionSpace {
  const autoEntries: OptionEntry[] = profiles.map((p) => {
    const topRung = orderedRungs(p)[0];
    return {
      codec: topRung?.codec ?? "h264",
      width: p.nominal.width,
      height: p.nominal.height,
      fps: p.nominal.fps,
      bitrateKbps: p.nominal.bitrate_kbps,
      profileId: p.id,
      eligibility: p.eligibility,
      reasons: p.reasons,
      recommended: p.id === recommendedId,
    };
  });

  const entriesByCodec = new Map<DraftCodec, OptionEntry[]>();
  entriesByCodec.set("auto", autoEntries);

  const codecs: DraftCodec[] = ["auto"];
  for (const codec of CODEC_ORDER) {
    if (!decodable(codec, codecCaps)) continue;
    // Dedup by (w,h,fps): several chains land on the same first C-rung. Credit
    // the NATURAL chain (profile's own nominal matches the rung) so profile_id
    // reflects what actually streams, not an unrelated higher chain — admin
    // observability reads profile_id at face value. Falls back to first-seen.
    const byKey = new Map<string, { entry: OptionEntry; natural: boolean }>();
    const order: string[] = [];
    for (const p of profiles) {
      const rung = orderedRungs(p).find((r) => r.codec === codec);
      if (!rung) continue;
      const key = `${rung.width}x${rung.height}x${rung.fps}`;
      const natural = p.nominal.height === rung.height && p.nominal.fps === rung.fps;
      const existing = byKey.get(key);
      if (existing && (existing.natural || !natural)) continue;
      if (!existing) order.push(key);
      byKey.set(key, {
        natural,
        entry: {
          codec,
          width: rung.width,
          height: rung.height,
          fps: rung.fps,
          bitrateKbps: rung.nominal_bitrate_kbps,
          profileId: p.id,
          eligibility: rung.eligibility,
          reasons: rung.reasons,
          recommended: false,
        },
      });
    }
    const entries: OptionEntry[] = order.map((k) => byKey.get(k)!.entry);
    if (entries.length === 0) continue;
    entriesByCodec.set(codec, entries);
    codecs.push(codec);
  }

  return { codecs, entriesByCodec };
}

/** Distinct fps values across every codec the panel offers, ascending — the
 * fps segment's fixed button set. */
export function fpsUniverse(space: OptionSpace): number[] {
  const all = new Set<number>();
  for (const entries of space.entriesByCodec.values()) {
    for (const e of entries) all.add(e.fps);
  }
  return Array.from(all).sort((a, b) => a - b);
}

/** fps values THIS codec actually has at least one entry for (regardless of
 * eligibility — an ineligible row still "has a profile", it's just
 * disabled). Anything in `fpsUniverse` but not here is a disabled button. */
export function availableFps(space: OptionSpace, codec: DraftCodec): number[] {
  const entries = space.entriesByCodec.get(codec) ?? [];
  return Array.from(new Set(entries.map((e) => e.fps))).sort((a, b) => a - b);
}

/** Distinct (height, representative width) pairs across every codec the
 * panel offers, descending by height — the resolution list's fixed row set. */
export function resolutionUniverse(space: OptionSpace): Array<{ height: number; width: number }> {
  const byHeight = new Map<number, number>();
  for (const entries of space.entriesByCodec.values()) {
    for (const e of entries) {
      if (!byHeight.has(e.height)) byHeight.set(e.height, e.width);
    }
  }
  return Array.from(byHeight.entries())
    .map(([height, width]) => ({ height, width }))
    .sort((a, b) => b.height - a.height);
}

/** The resolution list for one codec+fps — always the full
 * `resolutionUniverse`, each row marked available/selectable for THIS combo. */
export function availableResolutions(space: OptionSpace, codec: DraftCodec, fps: number): ResolutionRow[] {
  const entries = space.entriesByCodec.get(codec) ?? [];
  return resolutionUniverse(space).map(({ height, width }) => {
    const match = entries.find((e) => e.height === height && e.fps === fps);
    if (!match) {
      return { height, width, available: false, selectable: false, recommended: false };
    }
    return {
      height,
      width,
      available: true,
      selectable: match.eligibility !== "ineligible",
      eligibility: match.eligibility,
      reasons: match.reasons,
      recommended: match.recommended,
      bitrateKbps: match.bitrateKbps,
      profileId: match.profileId,
      entryCodec: match.codec,
    };
  });
}

/** Snaps an invalid draft to the nearest valid combo after a cascade change
 * (ports the mockup's `normalizeDraft`): fps snaps to the nearest available;
 * resolution snaps to the highest selectable row (risky stays selectable,
 * only "no rung" or ineligible forces a snap). */
export function normalizeDraft(space: OptionSpace, draft: LaunchDraft): LaunchDraft {
  const fpsOptions = availableFps(space, draft.codec);
  let fps = draft.fps;
  if (fpsOptions.length > 0 && !fpsOptions.includes(fps)) {
    fps = fpsOptions.reduce((a, b) => (Math.abs(b - fps) < Math.abs(a - fps) ? b : a));
  }

  const rows = availableResolutions(space, draft.codec, fps);
  const selectable = rows.filter((r) => r.selectable);
  let height = draft.height;
  if (!selectable.some((r) => r.height === height)) {
    height = selectable[0]?.height ?? rows[0]?.height ?? height;
  }

  return { codec: draft.codec, fps, height };
}

/** Resolves a draft to what committing it actually launches, or null when
 * the combo has no matching entry (should not happen for a normalized
 * draft — defensive for a stale/pre-normalize draft). */
export function resolveSelection(space: OptionSpace, draft: LaunchDraft): ResolvedSelection | null {
  const entries = space.entriesByCodec.get(draft.codec) ?? [];
  const match = entries.find((e) => e.height === draft.height && e.fps === draft.fps);
  if (!match) return null;
  return {
    profileId: match.profileId,
    codec: draft.codec === "auto" ? null : match.codec,
    entryCodec: match.codec,
    width: match.width,
    height: match.height,
    fps: match.fps,
    bitrateKbps: match.bitrateKbps,
    eligibility: match.eligibility,
    reasons: match.reasons,
    recommended: match.recommended,
  };
}

/** Initial committed draft on first load: "auto" at the recommended profile's
 * nominal, falling back through first non-ineligible auto row, first auto
 * row, then a static default for zero launch profiles. */
export function defaultDraft(space: OptionSpace, recommendedId: string): LaunchDraft {
  const auto = space.entriesByCodec.get("auto") ?? [];
  const pick =
    auto.find((e) => e.profileId === recommendedId && e.eligibility !== "ineligible") ??
    auto.find((e) => e.eligibility !== "ineligible") ??
    auto[0];
  if (!pick) return { codec: "auto", fps: 60, height: 1080 };
  return { codec: "auto", fps: pick.fps, height: pick.height };
}

/** Display label for a codec segment button. */
export function codecLabel(codec: DraftCodec): string {
  if (codec === "auto") return "Auto";
  if (codec === "h264") return "H.264";
  if (codec === "hevc") return "HEVC";
  return "AV1";
}

/** What choosing this codec buys (UX assessment §2.6). "Auto" gets only its
 * general case here; which codec auto lands on for the current draft is
 * composed at the call site. */
export function codecBasis(codec: DraftCodec): string {
  if (codec === "auto") return "Your host picks the codec, best first, and falls back to one this device can decode.";
  if (codec === "h264") return "H.264 plays on every device and needs the most bandwidth of the three.";
  if (codec === "hevc") return "HEVC gives you the same picture as H.264 for noticeably less bandwidth.";
  return "AV1 is the most efficient of the three, and the newest — decoding it can work this device harder.";
}

// Eligibility reasons, in the user's words (UX assessment §2.6). `GET /v1/me/profiles`
// sends reasons[] {code, message} per verdict — codes are the stable, append-only
// half (control-api.md), messages are operator-facing wire text. This table
// translates codes to user copy.
//
// `bandwidth_too_low` is emitted at both severities by eligibility.go (hard: under
// the profile minimum; soft: under the recommended headroom) with different meaning
// to a user, so the table carries both wordings and the caller passes the verdict.

/** Verdict grain a reason was attached to; picks the wording. */
export type ReasonSeverity = "risky" | "ineligible";

interface ReasonCopy {
  /** Wording for an `ineligible` verdict. */
  hard: string;
  /** Wording for a `risky` verdict; falls back to `hard`. */
  soft?: string;
}

const REASON_COPY: Record<string, ReasonCopy> = {
  bandwidth_too_low: {
    hard: "Your connection is slower than this quality needs. Pick a lower resolution or frame rate.",
    soft: "Your connection has little room to spare here, so the picture may wobble when the network dips. A lower resolution or frame rate holds steadier.",
  },
  rtt_too_high: {
    hard: "Your connection takes too long to reach the host for this quality, so the controls would feel laggy. Try a wired network, or pick a lower quality.",
  },
  decode_height_too_low: {
    hard: "This device can't decode a picture this large. Pick a lower resolution.",
  },
  codec_not_supported: {
    hard: "This browser can't decode the video format this option uses. Leave the codec on Auto, or open Quasar in a different browser.",
  },
  host_encoder_not_supported: {
    hard: "No host that can encode this quality is available. Pick a lower quality, or try again once a host frees up.",
  },
  display_refresh_unknown: {
    hard: "This option needs a high-refresh screen, and Quasar can't read your screen's refresh rate. A lower frame rate is the safe pick.",
  },
  display_refresh_too_low: {
    hard: "Your screen refreshes slower than this frame rate, so the extra frames never reach you. Pick a frame rate your screen can show.",
  },
  browser_playout_unsupported: {
    hard: "This option is built for the native client. In a browser it can stutter or fail to play.",
  },
  historical_client_performance_failed: {
    hard: "This device couldn't keep up with this option last time it ran. Pick a lower quality.",
  },
  probe_missing: {
    hard: "Quasar hasn't measured your network yet, so these are conservative estimates rather than a measurement.",
  },
  probe_stale: {
    hard: "Your last network measurement is out of date, so these are estimates rather than a fresh measurement.",
  },
};

/** Trim, capitalise, and end with a full stop — for the degraded path, where
 * the only thing left to say is what the server said. */
function asSentence(text: string): string {
  const t = text.trim();
  if (!t) return "";
  const head = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(head) ? head : `${head}.`;
}

/** One reason as a user-actionable sentence. Codes are append-only: an
 * unrecognised code degrades to the server's own message-as-sentence rather
 * than disappearing or printing the raw code. */
export function reasonSentence(reason: ProfileReason, severity: ReasonSeverity = "ineligible"): string {
  const copy = REASON_COPY[reason.code];
  if (copy) return severity === "risky" ? copy.soft ?? copy.hard : copy.hard;
  const fromServer = asSentence(reason.message ?? "");
  if (fromServer) return fromServer;
  return "Your host flagged this option without saying why. If it won't start, pick a lower quality.";
}

/** Every reason as a sentence, deduped by code — a profile's rungs routinely
 * repeat the same verdict. */
export function reasonSentences(
  reasons: readonly ProfileReason[] | undefined,
  severity: ReasonSeverity = "ineligible",
): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const r of reasons ?? []) {
    if (seen.has(r.code)) continue;
    seen.add(r.code);
    out.push(reasonSentence(r, severity));
  }
  return out;
}

/** Why nothing in this response is launchable: reasons across every profile
 * and rung (a chain's own `reasons` carries only its top rung's) plus
 * response-level `notes`, deduped by code. */
export function blockingReasons(data: ProfilesResponse): ProfileReason[] {
  const seen = new Set<string>();
  const out: ProfileReason[] = [];
  const take = (r: ProfileReason) => {
    if (seen.has(r.code)) return;
    seen.add(r.code);
    out.push(r);
  };
  for (const p of data.profiles) {
    for (const r of p.reasons ?? []) take(r);
    for (const rung of p.rungs ?? []) {
      for (const r of rung.reasons ?? []) take(r);
    }
  }
  for (const n of data.notes ?? []) take(n);
  return out;
}

/** catalog `hevc` -> wire `h265`; every other codec is unchanged. Only place
 * this mapping happens (spec B3/B4) — elsewhere stays catalog vocabulary. */
export function toWireCodec(codec: CatalogCodec): "h264" | "h265" | "av1" {
  return codec === "hevc" ? "h265" : codec;
}

// Moved from ProfilePicker.tsx (deleted, B5) — quickLaunch's eligibility gate
// (AppHomeNext.tsx) reads these.

/** The profile `recommended_id` points at, or null if phantom (stale id, no
 * match). Warning surface only, never authorization: quickLaunch can launch
 * with no profile_id and the server still resolves/rejects (CLAUDE.md
 * invariant #6). */
export function recommendedProfile(data: ProfilesResponse): LaunchProfileEvaluation | null {
  return data.profiles.find((p) => p.id === data.recommended_id) ?? null;
}

/** True only when the resolved recommendation is outright `eligible`. */
export function isRecommendationEligible(data: ProfilesResponse): boolean {
  return recommendedProfile(data)?.eligibility === "eligible";
}

// ── One call for the whole panel (home/DetailBand + home/LaunchOptions) ──────

export interface LaunchOptionsModel {
  space: OptionSpace;
  /** `ProfilesResponse.confidence` — whether the network was measured. */
  confidence: string;
  /**
   * #525: the launch profile this app pins, or null when it pins nothing (or
   * pins one this menu cannot offer, or the caller is an admin — the server
   * exempts them from the same check). A resolvable pin freezes the frame-rate
   * and resolution columns and is what Play sends. UX only; the gate is the
   * server's `409 profile overrides are disabled for this launch`.
   */
  pinnedProfileId: string | null;
  /** The draft to open on. */
  seed: LaunchDraft;
}

/**
 * Everything the launch panel needs from one `GET /v1/me/profiles?app_id=`
 * response, already intersected with the decode probe and with the app's own
 * profile policy resolved.
 */
export function optionsFor(input: {
  app: Pick<App, "profile_policy" | "default_profile_id">;
  data: ProfilesResponse;
  caps: CodecCapabilities;
  isAdmin: boolean;
}): LaunchOptionsModel {
  const { app, data, caps, isAdmin } = input;
  const space = buildOptionSpace(data.profiles, caps, data.recommended_id);

  // The pin must actually resolve before it disables anything: a forced app
  // with a null or ineligible `default_profile_id` would otherwise lock the
  // panel to a selection the server still refuses, with no way to change it.
  const wanted = app.profile_policy === "force" && !isAdmin ? app.default_profile_id ?? null : null;
  const resolves =
    wanted !== null &&
    (space.entriesByCodec.get("auto") ?? []).some(
      (e) => e.profileId === wanted && e.eligibility !== "ineligible",
    );
  const pinnedProfileId = resolves ? wanted : null;

  // A pinned app seeds from its own profile, not the per-device
  // recommendation: seeding the recommendation would default to a selection
  // the server always rejects.
  return {
    space,
    confidence: data.confidence,
    pinnedProfileId,
    seed: defaultDraft(space, pinnedProfileId ?? data.recommended_id),
  };
}
