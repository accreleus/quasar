// UI-P5: pure derivation for the app editor's launchable allow-list, testable
// in isolation. Not enforcement — the server rejects a disallowed profile_id
// at POST /v1/sessions (CLAUDE.md invariant #6); this only decides what to send.

import type { ProfilePolicyMode } from "../../../api/types";

/**
 * The always-launchable, untickable profile: the app default, only under
 * `prefer`. Under `inherit` the server pins nothing either — the two must
 * agree or the UI shows a tick the launch path does not honour.
 */
export function pinnedLaunchProfileId(
  policy: ProfilePolicyMode,
  defaultProfileId: string,
): string {
  return policy === "prefer" ? defaultProfileId : "";
}

/** Whether the allow-list control applies at all. `force` pins the profile. */
export function allowListApplies(policy: ProfilePolicyMode): boolean {
  return policy === "inherit" || policy === "prefer";
}

/**
 * What to send as `launchable_profile_ids`. `[]` = the contract's
 * "unrestricted" (sent, not omitted, so switching back clears a stored list).
 * The pinned default is included: unticking everything but it must still
 * produce a non-empty array, or the save means the opposite of the intent.
 */
export function effectiveLaunchableIds(
  policy: ProfilePolicyMode,
  defaultProfileId: string,
  restrict: boolean,
  ticked: string[],
): string[] {
  if (!allowListApplies(policy) || !restrict) return [];
  const pinned = pinnedLaunchProfileId(policy, defaultProfileId);
  return Array.from(new Set(pinned ? [pinned, ...ticked] : ticked));
}
