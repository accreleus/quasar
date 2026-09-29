/**
 * Amendment 18 — console access on owned hosts (control-api.md §Console mode,
 * agent-api.md `capacity.console_capabilities.access`). Pure helpers so the
 * "has access" and "reads as on" rules live in one place, not re-derived in
 * the component.
 */

import type { ConsoleAccess, ConsoleCapabilities, Host } from "../../../../api/types";
import { failureText } from "../releasesCopy";

type AudioSink = ConsoleCapabilities["audio_sinks"][number];

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
 * The copyable "run this as root" command for the "unprepared" restored
 * state. Engine/mode-aware when the host has reported its engine facts
 * (amendment 17, `Host.engine` / `Host.engine_mode`); otherwise the generic
 * form the mock shows, since there is nothing truthful to fill in for a host
 * that has never registered them.
 */
export function prepareHostConsoleCommand(host: Pick<Host, "engine" | "engine_mode"> | null | undefined): string {
  const mode = host?.engine_mode;
  if (!mode) return "sudo sh prepare-host.sh --console …";
  const engine = host?.engine ? ` --engine ${host.engine}` : "";
  return `sudo sh prepare-host.sh --mode ${mode}${engine} --console`;
}

/**
 * Whether the "Local audio output" selector's reported sinks are the host's
 * PipeWire (ids `pipewire:<node>` / `pipewire:default`, RH07-15 §3) or ALSA
 * `hw:*` sinks — the two never mix, since the agent reports one family or the
 * other for a given host. `"none"` when no sinks were reported at all (an
 * older agent, or a host that has not probed audio), which keeps today's
 * generic help text.
 */
export type ConsoleAudioBackend = "pipewire" | "alsa" | "none";

export function consoleAudioBackend(sinks: AudioSink[] | undefined | null): ConsoleAudioBackend {
  if (!sinks || sinks.length === 0) return "none";
  return sinks.some((s) => s.id.startsWith("pipewire:")) ? "pipewire" : "alsa";
}
