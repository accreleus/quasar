/**
 * The host drawer's and host detail's "Build" facts: which agent build a host
 * is running and how it got there (`Host.source_commit` / `built_at` /
 * `install_mode` / `updater_present`, openapi.yaml).
 *
 * One module so the two surfaces cannot word the same fact differently — and
 * so the null-vs-false distinction is made in exactly one place: `null` is "no
 * amendment-aware agent has registered", `false` is "an agent looked and found
 * none", and a UI that renders both as "No" loses the difference an operator
 * needs to act on.
 */

import type { Host } from "../../../api/types";

/** Enough of a commit to identify it at a glance; the caller carries the full
 *  value in a `title`, exactly as the id cells do. */
export const SHORT_COMMIT_LENGTH = 12;

export function shortCommit(commit: string | null | undefined): string {
  return commit ? commit.slice(0, SHORT_COMMIT_LENGTH) : "";
}

export function installModeLabel(mode: Host["install_mode"]): string {
  switch (mode) {
    case "registry":
      return "Registry";
    case "source":
      return "Built from source";
    case "owned":
      return "Owned by Quasar";
    default:
      return "Unknown";
  }
}

/** A source-built host can be told about a platform release but never given
 *  one, so the mode earns a hint rather than a bare word. */
export function installModeHint(mode: Host["install_mode"]): string | undefined {
  switch (mode) {
    case "registry":
      return "This host runs published platform images.";
    case "source":
      return "This host's images were built on it; a platform release can be shown but not applied.";
    case "owned":
      return "This host's services are created and replaced by its recovery actor.";
    default:
      return "No agent has reported how this host was installed.";
  }
}

/** On an owned host `updater_present` answers "did its recovery actor answer
 *  on the agent socket" (amendment 14), so the words follow the mode. */
export function updaterLabel(
  present: Host["updater_present"],
  mode: Host["install_mode"] = null,
): string {
  if (mode === "owned" && present === true) return "Recovery actor";
  if (mode === "owned" && present === false) return "Not answering";
  if (present === true) return "Present";
  if (present === false) return "None";
  return "Unknown";
}

export function updaterHint(
  present: Host["updater_present"],
  mode: Host["install_mode"] = null,
): string {
  if (mode === "owned" && present === true)
    return "This host's recovery actor answered the agent.";
  if (mode === "owned" && present === false)
    return "This host's recovery actor did not answer the agent — a release cannot be applied here.";
  if (present === true) return "This host's recovery actor answered the agent.";
  if (present === false)
    return "This host has no recovery actor: it was not installed with the seed, so a release cannot be applied here.";
  return "No agent has reported whether a recovery actor is present.";
}

/** True only when all four identity fields are known. The eligibility model
 *  turns on this, and a partially-identified host is not a known one. */
export function identityKnown(host: Host): boolean {
  return (
    host.source_commit != null &&
    host.built_at != null &&
    host.install_mode != null &&
    host.updater_present != null
  );
}
