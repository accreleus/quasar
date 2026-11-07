// What the Compatibility tab and its picker derive from the catalogue
// (control-api.md §Compatibility tools): pure, so both read one rule.

import type { CompatTool, CompatToolAvailable } from "../../../api/admin";

type HostCounts = CompatTool["hosts"];

export function hostTotal(h: NonNullable<HostCounts>): number {
  return h.installed + h.installing + h.waiting + h.failed + h.unsupported;
}

export type InstallState = "installing" | "failed" | "waiting" | "installed";

/** One state for a tool across the fleet. Only a host actually installing or
 *  removing is "in flight" (it blocks Remove). Hosts that are offline (waiting),
 *  on an agent too old for tools (unsupported) or failed never block: a tool on
 *  some hosts reads installed, with the rest as notes. It reads failed or waiting
 *  only when no host holds it. */
export function installState(h: HostCounts): InstallState {
  if (!h) return "installed";
  if (h.installing > 0) return "installing";
  if (h.installed > 0) return "installed";
  if (h.failed > 0) return "failed";
  return "waiting";
}

/** The hosts that are neither installed nor installing, as short notes. */
export function hostNotes(h: NonNullable<HostCounts>): string[] {
  return [
    h.waiting > 0 && `${h.waiting} waiting`,
    h.failed > 0 && `${h.failed} failed`,
    h.unsupported > 0 && `${h.unsupported} unsupported`,
  ].filter((n): n is string => n !== false);
}

/** The installed-tools table: GE tools desired on every host. Valve's builds
 *  are never rows (they need no install). */
export function installedTools(items: CompatTool[]): CompatTool[] {
  return items.filter((t) => t.family === "ge" && t.enabled);
}

/** The default / per-game selects: Valve's builds and every enabled installed
 *  tool the contract allows (control-api.md §Usable: enabled, no conflict mark),
 *  whichever hosts still lack it. The current default always stays listed so the
 *  select never shows a value it cannot name. */
export function selectableInstalled(items: CompatTool[]): CompatTool[] {
  return installedTools(items).filter((t) => t.is_default || (!t.pin_conflict && !t.name_conflict));
}

export function valveBuilds(items: CompatTool[]): CompatTool[] {
  return items.filter((t) => t.family === "valve" && t.enabled);
}

export type PickerState =
  | { kind: "installable" }
  | { kind: InstallState }
  | { kind: "pin_conflict" | "name_conflict" };

/** What the picker shows beside one available release. A pin conflict can never
 *  be enabled; a name conflict only blocks a tool that is still enabled (the
 *  exit is disable, then enable). */
export function pickerState(a: CompatToolAvailable): PickerState {
  if (a.pin_conflict) return { kind: "pin_conflict" };
  if (a.enabled && a.name_conflict) return { kind: "name_conflict" };
  if (a.enabled) return { kind: installState(a.hosts) };
  return { kind: "installable" };
}

/** Why a Remove was refused, from the row's own counts (the server's message is
 *  a log line, not copy). */
export function refusalCopy(t: Pick<CompatTool, "mappings" | "is_default">, fallback: string): string {
  const parts: string[] = [];
  if (t.mappings > 0) {
    parts.push(`${t.mappings} game${t.mappings === 1 ? " uses" : "s use"} this tool. Change their tool first.`);
  }
  if (t.is_default) parts.push("It's the default tool. Pick another default first.");
  return `Not removed: ${parts.length > 0 ? parts.join(" ") : fallback}`;
}

export const GITHUB_UNAVAILABLE: Record<string, string> = {
  unreachable: "github.com could not be reached.",
  rate_limited: "github.com is limiting requests right now.",
  upstream_error: "github.com answered with an error.",
  response_invalid: "github.com sent an answer Quasar could not read.",
};
