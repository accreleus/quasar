/**
 * The one place an eligibility reason or a fault kind becomes a sentence.
 *
 * The server sends stable identifiers and never the wording (control-api.md
 * §"Platform releases"), so this file is where the wording improves. An
 * identifier this build does not know is rendered VERBATIM rather than dropped:
 * amendment 2 appends `attempt_in_flight` and `run_active`, and a row that
 * vanished would be worse than one labelled with a raw identifier.
 */

import type { EligibilityReason, PlatformRelease } from "../../../api/types";

const REASON_TEXT: Record<string, string> = {
  no_release: "Nothing newer has been detected on this channel.",
  identity_unknown: "This target has not reported what it is running.",
  up_to_date: "Already on the newest release.",
  install_mode_source: "Built from source on the host — update it with git, not from here.",
  updater_absent: "No updater is installed beside this host's stack.",
  host_offline: "The host's agent is not connected.",
  release_above_control_plane: "Waiting on the control plane: this release carries a newer schema.",
  control_plane_not_first: "Waiting on the control plane, which moves first.",
  attempt_in_flight: "An update is already in flight on this target.",
  run_active: "A fleet update is already running.",
};

export function eligibilityText(reason: EligibilityReason | string | null): string {
  if (!reason) return "Ready to update.";
  return REASON_TEXT[reason] ?? reason;
}

const FAULT_TEXT: Record<string, string> = {
  agent_ahead_of_control_plane: "Agent ahead of the control plane",
  identity_unknown: "Build identity unknown",
  manifest_invalid: "Release manifest invalid",
};

export function faultText(kind: string): string {
  return FAULT_TEXT[kind] ?? kind;
}

/** A release's display name: its version on stable, a short commit on edge,
 *  which publishes no version by design. */
export function releaseLabel(release: PlatformRelease): string {
  return release.version || shortCommit(release.source_commit);
}

export function shortCommit(commit: string | null | undefined): string {
  if (!commit) return "unknown";
  return commit.slice(0, 12);
}

/** True when the newest listed release is not what the control plane is on —
 *  which is what "there is an update" means. `available` alone is not: a
 *  current instance still lists the release it is already running, so that the
 *  contract's `up_to_date` and `control_plane_not_first` reasons can be
 *  evaluated against it. */
export function hasUpdate(view: {
  available: PlatformRelease[];
  installed: { control_plane: { source_commit?: string | null } };
}): boolean {
  const newest = view.available[0];
  if (!newest) return false;
  const installed = view.installed.control_plane.source_commit;
  if (!installed) return true;
  return !commitsMatch(installed, newest.source_commit);
}

/** An agent stamps 7-40 hex while a manifest carries the full 40, so "the same
 *  commit" is a prefix match. Server twin: internal/platform.commitsMatch. */
export function commitsMatch(a: string | null | undefined, b: string | null | undefined): boolean {
  if (!a || !b) return false;
  const [x, y] = a.length <= b.length ? [a, b] : [b, a];
  return y.toLowerCase().startsWith(x.toLowerCase());
}
