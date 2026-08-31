// Pure formatting and display helpers for the UsersTab.
// No React imports — all functions are side-effect-free and testable in isolation.

import { ApiError } from "../../../api/client";
import { ACTIVE_SESSION_STATES } from "../../../api/sessionStates";
import { durationBetween } from "../../../lib/format/duration";

/** Re-export, not a second copy: the set has one definition (api/sessionStates)
 *  because a session the drawer paints as active must be one the rail counts. */
export const ACTIVE_STATES = ACTIVE_SESSION_STATES;

/** Format a session duration from ISO timestamps (or "now" if still running).
 *  The implementation is `lib/format/duration`'s, shared with the v3 console so
 *  one session's age does not read two ways on two screens. */
export const fmtDuration = durationBetween;

/** Generate a deterministic gradient for a username's avatar. */
export function avatarGradient(username: string): string {
  const palettes = [
    ["#6A45F5", "#3a1f9e"],
    ["#00C0FF", "#0b5a8f"],
    ["#5B6BFF", "#2a2f8c"],
    ["#8A5CF7", "#b084ff"],
    ["#2E8BFF", "#123a7a"],
    ["#FF5470", "#7a2540"],
    ["#6A45F5", "#00C0FF"],
  ];
  let hash = 0;
  for (let i = 0; i < username.length; i++)
    hash = (hash * 31 + username.charCodeAt(i)) & 0xffffffff;
  const [a, b] = palettes[Math.abs(hash) % palettes.length];
  return `linear-gradient(150deg,${a},${b})`;
}

/** Map a session state to a CSS color variable for the dot in the drawer. */
export function sessionStateDot(state: string): string {
  if (state === "running") return "var(--success)";
  if (ACTIVE_STATES.has(state)) return "var(--warning)";
  if (state === "failed") return "var(--danger)";
  return "var(--text-4)";
}

/** Friendly message for server refusal codes. */
export function friendlyError(e: unknown): string {
  if (!(e instanceof ApiError)) return "Unexpected error";
  const msg = e.message.toLowerCase();
  if (msg.includes("last_admin") || msg.includes("last admin"))
    return "Cannot demote or delete the last admin account.";
  if (msg.includes("self") || msg.includes("yourself"))
    return "You cannot perform this action on your own account.";
  if (msg.includes("active_session") || msg.includes("active session"))
    return "User has active sessions — stop them first.";
  return e.message || "Server refused the request.";
}
