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

/** Apply progress, as a phrase the row reads as its state. */
const ATTEMPT_STATE_TEXT: Record<string, string> = {
  queued: "Queued",
  waiting_sessions: "Waiting for sessions to end",
  pending: "Handed to the updater",
  pulling: "Pulling the image",
  recreating: "Recreating the agent",
  verifying: "Verifying",
  succeeded: "Updated",
  failed: "Update failed",
  cancelled: "Cancelled",
};

export function attemptStateText(state: string): string {
  return ATTEMPT_STATE_TEXT[state] ?? state;
}

/** A fleet run's state. There is no `partial`: a failed run stops at its first
 *  failed target and the per-target attempts carry the rest. */
const RUN_STATE_TEXT: Record<string, string> = {
  pending: "Queued.",
  running: "Updating.",
  succeeded: "Every target is on the new release.",
  failed: "Stopped at the first target that failed.",
  cancelled: "Cancelled; nothing further was started.",
};

export function runStateText(state: string): string {
  return RUN_STATE_TEXT[state] ?? state;
}

/** The closed failure vocabulary, shared verbatim with the wire, so this one
 *  mapping serves progress, history and an ack rejection. An identifier this
 *  build does not know renders verbatim. */
const FAILURE_TEXT: Record<string, string> = {
  updater_absent: "No updater is installed beside this host's stack.",
  busy: "An update was already in flight on this host.",
  invalid: "The update request was rejected as un-actionable.",
  namespace_rejected: "The image is outside this host's platform-image namespace.",
  digest_malformed: "The image digest was malformed.",
  pull_failed: "The image could not be pulled.",
  recreate_failed: "The container could not be recreated — this host's agent is stopped.",
  never_started: "The new container never started.",
  unhealthy: "The new container started but never became healthy.",
  updater_unreachable: "The updater could not be reached.",
  timeout: "The update did not finish in time.",
  unsupported: "This host's agent predates the update feature; update it another way.",
};

export function failureText(reason: string | null | undefined): string {
  if (!reason) return "";
  return FAILURE_TEXT[reason] ?? reason;
}

/** A digest, short enough to read and long enough to identify. */
export function shortDigest(digest: string | null | undefined): string {
  if (!digest) return "unknown";
  const hex = digest.startsWith("sha256:") ? digest.slice(7) : digest;
  return hex.slice(0, 12);
}

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

/** Display counterpart of the server's same-schema edge ordering. Unknown
 * build metadata never proves that the candidate is older. */
export function olderEdgeCandidate(
  view: { installed: { control_plane: { schema_version?: number; built_at?: string | null } } },
  release: PlatformRelease | undefined,
): boolean {
  const installed = view.installed.control_plane;
  return installed.built_at != null && installed.schema_version !== undefined &&
    release?.channel === "edge" && release.schema_version === installed.schema_version &&
    Date.parse(release.built_at) < Date.parse(installed.built_at);
}

/** A different release is an update candidate unless its same-schema edge
 *  build predates the installed one. `available` alone is not: a
 *  current instance still lists the release it is already running, so that the
 *  contract's `up_to_date` and `control_plane_not_first` reasons can be
 *  evaluated against it. */
export function hasUpdate(view: {
  available: PlatformRelease[];
  installed: { control_plane: { source_commit?: string | null; schema_version?: number; built_at?: string | null } };
}): boolean {
  const newest = view.available[0];
  if (!newest || olderEdgeCandidate(view, newest)) return false;
  const installed = view.installed.control_plane.source_commit;
  if (!installed) return true;
  return !commitsMatch(installed, newest.source_commit);
}

/** Whether applying this release runs a migration here: `schema_version` IS the
 *  highest migration a build embeds, so "above the installed control plane" and
 *  "migrates the database" are one fact. Only a migrating release makes the
 *  control-plane step wait for every session on the instance to end (#153); the
 *  rest carry them across the restart. An unknown installed schema reads as
 *  migrating, as it does server-side. Server twin:
 *  internal/platform.ReleaseRunsAMigration. */
export function releaseRunsAMigration(
  view: { installed: { control_plane: { schema_version?: number } } },
  release: PlatformRelease | undefined,
): boolean {
  const installed = view.installed.control_plane.schema_version;
  if (installed === undefined || !release) return true;
  return release.schema_version > installed;
}

/** An agent stamps 7-40 hex while a manifest carries the full 40, so "the same
 *  commit" is a prefix match. Server twin: internal/platform.commitsMatch. */
export function commitsMatch(a: string | null | undefined, b: string | null | undefined): boolean {
  if (!a || !b) return false;
  const [x, y] = a.length <= b.length ? [a, b] : [b, a];
  return y.toLowerCase().startsWith(x.toLowerCase());
}
