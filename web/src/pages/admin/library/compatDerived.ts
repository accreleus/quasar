// What the Compatibility tab and its picker derive from the catalogue
// (control-api.md §Compatibility tools): pure, so both read one rule.

import type { CompatTool, CompatToolAvailable } from "../../../api/admin";

type HostCounts = CompatTool["hosts"];

export type InstallState = "installing" | "failed" | "installed";

export function hostTotal(h: NonNullable<HostCounts>): number {
  return h.installed + h.installing + h.waiting + h.failed + h.unsupported;
}

/** One state for a tool across the fleet. A host still catching up (installing,
 *  removing, or waiting for an offline host) reads as installing; a failure only
 *  shows once nothing is still in flight. */
export function installState(h: HostCounts): InstallState {
  if (!h) return "installed";
  if (h.installing > 0 || h.waiting > 0) return "installing";
  if (h.failed > 0 || h.unsupported > 0) return "failed";
  return "installed";
}

/** The installed-tools table: GE tools desired on every host. Valve's builds
 *  are never rows (they need no install). */
export function installedTools(items: CompatTool[]): CompatTool[] {
  return items.filter((t) => t.family === "ge" && t.enabled);
}

/** The default / per-game selects: Valve's builds, and the installed tools that
 *  are done installing and not in conflict. The current default always stays
 *  listed so the select never shows a value it cannot name. */
export function selectableInstalled(items: CompatTool[]): CompatTool[] {
  return installedTools(items).filter(
    (t) => t.is_default || (installState(t.hosts) === "installed" && !t.pin_conflict && !t.name_conflict),
  );
}

export function valveBuilds(items: CompatTool[]): CompatTool[] {
  return items.filter((t) => t.family === "valve" && t.enabled);
}

export type PickerState =
  | { kind: "installable" }
  | { kind: "installing" | "failed" | "installed" }
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

export const GITHUB_UNAVAILABLE: Record<string, string> = {
  unreachable: "github.com could not be reached.",
  rate_limited: "github.com is limiting requests right now.",
  upstream_error: "github.com answered with an error.",
  response_invalid: "github.com sent an answer Quasar could not read.",
};
