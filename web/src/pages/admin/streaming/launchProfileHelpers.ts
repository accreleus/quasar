// Pure helpers for the Launch profiles tab (v3 handoff §A.16).
// A launch profile is an ORDERED CHAIN of stream profiles (rungs), best first —
// see docs/design/plans/2026-07-28-phase4-profile-restructure-respec.md §1.

import type { LaunchProfileUsedBy, StreamProfile } from "../../../api/types";

/**
 * Client-side half of "delete is disabled while the launch profile is
 * referenced" (control-api.md: refuse-if-referenced by an app, the global
 * policy, or a user preference). UX affordance ONLY — the server 409s
 * regardless (CLAUDE.md invariant #6).
 */
export function isLaunchProfileInUse(usedBy: LaunchProfileUsedBy | undefined): boolean {
  if (!usedBy) return false;
  return usedBy.apps.length > 0 || usedBy.global_default || usedBy.user_preferences > 0;
}

/**
 * Display "floor": the last rung iff h264 (the mock's `.rung.locked`,
 * handoff §A.16). A non-last h264 rung is not locked — h264_floor_not_last is
 * a warning, not a rejection (respec §2.7), so it must stay movable/removable.
 */
export function isFloorRung(rungs: StreamProfile[], index: number): boolean {
  return index === rungs.length - 1 && rungs[index]?.codec === "h264";
}

/** "35" not "35.0": trims a whole-Mb/s value to an integer, keeps one decimal
 *  only when the kbps figure needs it. */
export function formatMbps(kbps: number): string {
  const mbps = Math.round((kbps / 1000) * 10) / 10;
  return Number.isInteger(mbps) ? String(mbps) : mbps.toFixed(1);
}
