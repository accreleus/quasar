/**
 * Amendment 18 — console access on owned hosts (control-api.md §Console mode,
 * agent-api.md `capacity.console_capabilities.access`). Pure helpers so the
 * "has access" and "reads as on" rules live in one place, not re-derived in
 * the component.
 */

import type { ConsoleAccess } from "../../../../api/types";
import { failureText } from "../releasesCopy";

/** "Has access": `state` is `on`, or `restored` with `target` false (a failed
 *  attempt to turn it OFF, so the host kept the access it had). Every other
 *  state, including an unrecognised one, means no access. */
export function hasAccess(access: ConsoleAccess | undefined | null): boolean {
  if (!access) return false;
  if (access.state === "on") return true;
  return access.state === "restored" && access.target === false;
}

/** Console mode reads as on only when the admin's wish (`enabled`) and what
 *  the host has agree, and never while a replacement is `applying`. */
export function readsAsOn(enabled: boolean | undefined, access: ConsoleAccess | undefined | null): boolean {
  return Boolean(enabled) && hasAccess(access);
}

/** "the host's live sessions" / "1 live session" / "2 live sessions" — a noun
 *  phrase, not a full sentence, so callers can say "ends {phrase}" or "the
 *  {phrase} on this host ends". `null` (the count could not be read) and `0`
 *  both fall back to the generic phrasing, same rule as ApplyConfirmModal's. */
export function liveSessionsNoun(liveSessions: number | null): string {
  if (liveSessions == null || liveSessions === 0) return "host's live sessions";
  return `${liveSessions} live session${liveSessions === 1 ? "" : "s"}`;
}

/** A restored attempt's `reason` is an identifier from the same closed
 *  `release_state` failure vocabulary the fleet-apply surfaces use
 *  (`agent-api.md` §`capacity`) — shared verbatim, not a second mapping. */
export function accessReasonText(reason: string | null | undefined): string {
  return failureText(reason);
}
