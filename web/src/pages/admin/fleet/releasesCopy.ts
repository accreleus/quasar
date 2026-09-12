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
  preflight_blocked: "A preflight check failed on this target; the check below names the fix.",
  attempt_in_flight: "An update is already in flight on this target.",
  run_active: "A fleet update is already running.",
};

/** The closed preflight check vocabulary (amendment 9), as short labels. An
 *  unknown id renders verbatim. */
const PREFLIGHT_CHECK_TEXT: Record<string, string> = {
  updater_socket: "updater reachable",
  updater_stack_dir: "updater sees the stack directory",
  updater_overlays: "compose files match the updater's",
  image_resolvable: "release images resolve at the registry",
  agent_connected: "agent connected",
  health_addr_bindable: "agent health port free",
};

export function preflightCheckText(id: string): string {
  return PREFLIGHT_CHECK_TEXT[id] ?? id;
}

/** A skip reason as the short phrase a sentence takes ("gpu-02 (offline)");
 *  REASON_TEXT above is the same vocabulary as a full sentence. */
const SKIP_PHRASE: Record<string, string> = {
  host_offline: "offline",
  preflight_blocked: "a preflight check failed",
  install_mode_source: "built from source",
  updater_absent: "no updater",
  attempt_in_flight: "another update in flight",
};

export function skipReasonPhrase(reason: string): string {
  return SKIP_PHRASE[reason] ?? reason;
}

/** Which button, or no button at all, produced an attempt. */
const ATTEMPT_KIND_TEXT: Record<string, string> = {
  apply: "Apply",
  revert: "Revert",
  auto_revert: "Reverted automatically",
};

export function attemptKindText(kind: string): string {
  return ATTEMPT_KIND_TEXT[kind] ?? kind;
}

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

/** A fleet run's state. A failed run stops at its first failed target and the
 *  per-target attempts carry the rest; a partial one failed nothing but left a
 *  host behind (amendment 9). */
const RUN_STATE_TEXT: Record<string, string> = {
  pending: "Queued.",
  running: "Updating.",
  succeeded: "Every target is on the new release.",
  succeeded_partial: "Applied, but at least one host was skipped and is still on the old release.",
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
  signature_missing: "This host requires a signed release and this one is not signed.",
  signature_invalid: "The release's signature did not verify against this host's trusted keys.",
};

export function failureText(reason: string | null | undefined): string {
  if (!reason) return "";
  return FAILURE_TEXT[reason] ?? reason;
}

/** What a failure left running on the host. Keyed on the same closed
 *  vocabulary: a failure past the health wait IS restored by the updater
 *  itself (ADR 0004), so one fixed "nothing was rolled back" line was false for
 *  half of them (#201). "" for a reason this build does not know — no sentence
 *  beats a guess about what a host is running. */
const UNTOUCHED = "Nothing was applied: this host is still running the build it had.";

const RESTORE_ATTEMPTED =
  "The new container did not come up. The updater puts the previous build back itself when " +
  "that happens; the apply history shows an automatic revert when it worked.";

const AFTER_FAILURE_TEXT: Record<string, string> = {
  updater_absent: UNTOUCHED,
  busy: UNTOUCHED,
  invalid: UNTOUCHED,
  namespace_rejected: UNTOUCHED,
  digest_malformed: UNTOUCHED,
  updater_unreachable: UNTOUCHED,
  unsupported: UNTOUCHED,
  signature_missing: UNTOUCHED,
  signature_invalid: UNTOUCHED,
  // Nothing was recreated, so the old container is still the running one.
  pull_failed:
    "The image never arrived, so nothing was recreated: this host is still running the build it had.",
  recreate_failed: RESTORE_ATTEMPTED,
  never_started: RESTORE_ATTEMPTED,
  unhealthy: RESTORE_ATTEMPTED,
  // Both builds are unaccounted for: the apply expired with no verdict, which
  // is what the attempt's own output explains.
  timeout:
    "This host's agent has not reported back, so what it is running now cannot be read from " +
    "here — check the host itself.",
};

export function hostAfterFailureText(reason: string | null | undefined): string {
  if (!reason) return "";
  return AFTER_FAILURE_TEXT[reason] ?? "";
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

/** An agent stamps 7-40 hex while a manifest carries the full 40, so "the same
 *  commit" is a prefix match. Server twin: internal/platform.commitsMatch. */
export function commitsMatch(a: string | null | undefined, b: string | null | undefined): boolean {
  if (!a || !b) return false;
  const [x, y] = a.length <= b.length ? [a, b] : [b, a];
  return y.toLowerCase().startsWith(x.toLowerCase());
}
