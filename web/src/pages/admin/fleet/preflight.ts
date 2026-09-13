/**
 * The console's read of a target's preflight (control-api.md §"Self-update
 * hardening"): which checks block, what to say about a run that passed hosts
 * over, and who a fleet update will skip. Pure over the wire shapes; the
 * server decides, this only phrases.
 */

import type {
  PlatformApplyRun,
  PlatformPreflightCheck,
  PlatformReleaseTarget,
} from "../../../api/types";
import { eligibilityText, preflightCheckText, skipReasonPhrase } from "./releasesCopy";

/** A server predating the amendment sends no `preflight`; that reads as unknown. */
export function preflightChecks(target: PlatformReleaseTarget): PlatformPreflightCheck[] {
  return target.preflight?.checks ?? [];
}

/** The checks that failed, in the vocabulary's order. */
export function blockingChecks(target: PlatformReleaseTarget): PlatformPreflightCheck[] {
  return preflightChecks(target).filter((c) => c.status === "fail");
}

/** The checks nobody could evaluate: shown as a warning, never a stop. */
export function unknownChecks(target: PlatformReleaseTarget): PlatformPreflightCheck[] {
  return preflightChecks(target).filter((c) => c.status !== "pass" && c.status !== "fail");
}

/** One line for a holdout row: the first failing check's name, else the reason. */
export function holdoutText(target: PlatformReleaseTarget): string {
  const first = blockingChecks(target)[0];
  if (target.reason === "preflight_blocked" && first) return `Blocked: ${preflightCheckText(first.id)}`;
  return eligibilityText(target.reason ?? null);
}

/** Hosts a fleet update will pass over, with why — every ineligible host
 *  except one already on the release, which is done rather than skipped. */
export function willBeSkipped(targets: PlatformReleaseTarget[]): PlatformReleaseTarget[] {
  return targets.filter((t) => t.kind === "host" && !t.eligible && t.reason !== "up_to_date");
}

/** "Applied to the control plane and 2 of 3 hosts — 1 skipped: gpu-02 (offline)".
 *  Counts come from the run itself: attempts are what moved, skips what did not. */
export function partialSummary(run: PlatformApplyRun): string {
  const cpMoved = run.attempts.some((a) => a.target === "control_plane" && a.state === "succeeded");
  const hostsMoved = run.attempts.filter((a) => a.target === "host" && a.state === "succeeded").length;
  const behind = run.skipped.filter((s) => s.reason !== "up_to_date");
  const hostTotal = hostsMoved + behind.length;
  const who = behind.map((s) => `${s.node_name} (${skipReasonPhrase(s.reason)})`).join(", ");
  const head = cpMoved
    ? `Applied to the control plane and ${hostsMoved} of ${hostTotal} hosts`
    : `Applied to ${hostsMoved} of ${hostTotal} hosts`;
  return `${head} — ${behind.length} skipped: ${who}`;
}
