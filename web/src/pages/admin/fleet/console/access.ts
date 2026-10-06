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

/**
 * RH07-15 (#407): a restored (failed) attempt's `summary` is built server-side
 * as the console preflight's own detail text, in front of the generic
 * put-back sentence (`node-agent/src/release/console.rs` `derive`,
 * `node-agent/src/session/console_preflight.rs` `run_with` — not a contract
 * field, so this stays a text match, not a parsed one). Two of that
 * preflight's outcomes get bespoke copy per the approved mock
 * (`design_handoff_v3/screens/rh07/README.md` "d"): a display another
 * process holds, and a host never prepared with `--console`. Every other
 * restored attempt (a plain replacement failure with no preflight detail)
 * reads as "generic" and keeps the existing reason-mapped copy.
 */
export type ConsoleFailureCategory = "held" | "unprepared" | "generic";

const UNPREPARED_PHRASE = "host not prepared for console mode";
const HELD_PHRASE = "holds the display";

export function consoleFailureCategory(access: ConsoleAccess): ConsoleFailureCategory {
  if (access.state !== "restored") return "generic";
  const summary = access.summary ?? "";
  if (summary.includes(UNPREPARED_PHRASE)) return "unprepared";
  if (summary.includes(HELD_PHRASE)) return "held";
  return "generic";
}

/**
 * The copyable "run this as root" fix for the "unprepared" restored state:
 * install the console device rules from a checkout of the repository and keep
 * login prompts off tty8 (docs: Install, Device rules). The same on every
 * engine and mode; nothing is downloaded.
 */
export const CONSOLE_RULES_COMMAND =
  "sudo cp deploy/udev/71-quasar-console.rules /etc/udev/rules.d/ && " +
  "sudo udevadm control --reload && sudo udevadm trigger && " +
  "sudo systemctl mask getty@tty8.service autovt@tty8.service";
