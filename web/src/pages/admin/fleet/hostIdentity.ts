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
    default:
      return "No agent has reported how this host was installed.";
  }
}

export function updaterLabel(present: Host["updater_present"]): string {
  if (present === true) return "Present";
  if (present === false) return "None";
  return "Unknown";
}

export function updaterHint(present: Host["updater_present"]): string {
  if (present === true) return "An updater sits beside this host's agent.";
  if (present === false)
    return "The agent looked and found no updater on this stack — a release cannot be applied here.";
  return "No agent has reported whether an updater is present.";
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
