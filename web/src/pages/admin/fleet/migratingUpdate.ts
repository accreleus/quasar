/**
 * A migrating update on the control plane's own machine (#364; control-api.md §"Migrating
 * updates: the pre-update dump and the external-backup confirmation"): what the Update
 * dialog says about the database, and how the Releases page reads a control-plane
 * attempt that failed around a migration. Pure over the wire shapes; the server decides,
 * this only reads and phrases.
 *
 * Laid out to design_handoff_v3/screens/rh06 `update-*.png` and `restore-*.png`
 * (`rhUpdateModal`, `rhRefusedBanner` and `rhRestore` in assets/pages-rh06.js).
 */

import type {
  PlatformApplyAttempt,
  PlatformPreflightCheck,
  PlatformRelease,
  PlatformReleaseTarget,
  PlatformReleaseView,
} from "../../../api/types";
import { releaseForDigest } from "./RevertControls";
import { commitsMatch } from "./releasesCopy";

/** The preflight check that answers whether the pre-update dump fits (amendment 14). */
export const BACKUP_SPACE = "backup_space";

/** How the control plane's machine names itself in copy. */
export function machineName(view: PlatformReleaseView): string {
  return view.installed.control_plane.machine_node_name ?? "this machine";
}

/** A recovery actor created this control plane (machine_role is non-null exactly then,
 *  whether or not the actor is answering right now). */
export function ownedControlPlane(view: PlatformReleaseView): boolean {
  return view.installed.control_plane.machine_role != null;
}

export function backupSpaceCheck(target: PlatformReleaseTarget | undefined): PlatformPreflightCheck | undefined {
  return target?.preflight?.checks.find((c) => c.id === BACKUP_SPACE);
}

/** What the Update dialog says about the database before a migrating update moves an
 *  owned control plane. */
export type DatabasePlan =
  /** Not an owned control plane, or the release does not migrate: nothing to say. */
  | { kind: "none" }
  /** Quasar's own database: the dump is taken first. `detail` is the server's
   *  backup_space prose, rendered verbatim; null when no check was served. */
  | { kind: "dump"; space: "pass" | "unknown"; detail: string | null }
  /** Quasar's own database, and the dump does not fit: the update cannot start. */
  | { kind: "no_space"; detail: string }
  /** The operator's own database: their backup, confirmed. */
  | { kind: "external" }
  /** The actor has not said which database this is (database_mode null or unknown). */
  | { kind: "unreported" };

export function databasePlan(view: PlatformReleaseView, migrates: boolean): DatabasePlan {
  if (!migrates || !ownedControlPlane(view)) return { kind: "none" };
  const mode = view.installed.control_plane.database_mode;
  if (mode === "external") return { kind: "external" };
  if (mode !== "owned") return { kind: "unreported" };
  const check = backupSpaceCheck(view.targets.find((t) => t.kind === "control_plane"));
  if (check?.status === "fail") return { kind: "no_space", detail: check.detail };
  return {
    kind: "dump",
    space: check?.status === "pass" ? "pass" : "unknown",
    detail: check?.detail || null,
  };
}

/** The last non-empty line of an attempt's output. The contract guarantees that a
 *  failed migrating control-plane attempt's output ENDS WITH the one-line restore
 *  command, so this is that command whenever there is one; it is never composed here. */
export function lastOutputLine(output: string | null | undefined): string | null {
  const lines = (output ?? "").split("\n").map((l) => l.trim()).filter((l) => l !== "");
  return lines.length > 0 ? lines[lines.length - 1] : null;
}

/** Whether a line reads as the recovery actor's restore command rather than an ordinary
 *  last line of output. Only guards the copyable block against showing something that is
 *  not a command; the command itself is shown exactly as served. */
function looksLikeRestoreCommand(line: string | null): line is string {
  return line != null && /^docker\s/.test(line) && /\srestore(\s|$)/.test(line);
}

/** Failure reasons that mean the control plane was never replaced. Amendment 14 names
 *  five of them outright ("each meaning nothing changed"); the rest are refusals before
 *  anything was sent or pulled. */
const NOT_REPLACED = new Set<string>([
  "recipe_unsupported",
  "owner_conflict",
  "backup_failed",
  "backup_unconfirmed",
  "interrupted",
  "updater_absent",
  "busy",
  "invalid",
  "namespace_rejected",
  "digest_malformed",
  "pull_failed",
  "unsupported",
  "signature_missing",
  "signature_invalid",
]);

export type RestoreVariant =
  /** Quasar's own database, and the dump and its restore command were reported. */
  | "own"
  /** Quasar's own database (or not said), and the dump has not been reported yet. */
  | "unknown"
  /** The operator's own database: they restore their backup, then run the command. */
  | "external";

export interface FailedMigration {
  attempt: PlatformApplyAttempt;
  variant: RestoreVariant;
  /** The restore command, exactly as the output ends with it; null when not reported. */
  command: string | null;
  /** The release the attempt applied, when it is still listed (null for a developer apply). */
  release: PlatformRelease | undefined;
  /** The release the control plane was on before, when it can be named from its digest. */
  previous: PlatformRelease | undefined;
}

function controlPlaneDigest(digests: { name: string; digest: string | null }[]): string | null {
  return digests.find((d) => d.name === "control-plane")?.digest ?? null;
}

/**
 * A failed control-plane attempt that was not restored automatically because it
 * migrated: the attempt the restore card and the history's "not restored" line are
 * about. Null for anything else.
 *
 * The migration itself is read from the evidence the attempt carries: a pre-update dump
 * (only a migrating step takes one), a restore command at the end of its output, or a
 * schema step between the release it left and the one it applied when both are listed.
 * `migrates` on the release cannot say it once the new build serves the page, because it
 * is relative to the installed control plane.
 */
export function failedMigration(
  attempt: PlatformApplyAttempt,
  view: PlatformReleaseView,
): FailedMigration | null {
  if (attempt.target !== "control_plane" || attempt.state !== "failed") return null;
  if (attempt.reason && NOT_REPLACED.has(attempt.reason)) return null;
  if (!ownedControlPlane(view)) return null;

  const release = attempt.release_id
    ? view.available.find((r) => r.id === attempt.release_id)
    : undefined;
  const prevDigest = controlPlaneDigest(attempt.previous_digests);
  const previous = prevDigest ? releaseForDigest(view.available, prevDigest) : undefined;
  const line = lastOutputLine(attempt.output);
  const command = looksLikeRestoreCommand(line) ? line : null;
  const dump = attempt.pre_update_dump ?? null;
  const schemaStep =
    release != null && previous != null && release.schema_version > previous.schema_version;
  if (dump == null && command == null && !schemaStep) return null;

  const external = view.installed.control_plane.database_mode === "external";
  const variant: RestoreVariant = external ? "external" : dump != null && command != null ? "own" : "unknown";
  return { attempt, variant, command, release, previous };
}

/** When a pre-update dump was taken, from its name (`20260925T140200Z-schema-88`, the
 *  recovery actor's `dump_dir.rs` format), as an ISO instant; null for any other name. */
export function dumpTakenAt(name: string | null | undefined): string | null {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z-schema-\d+(-[0-9a-f]{8})?$/.exec(name ?? "");
  if (!m) return null;
  const iso = `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`;
  return Number.isNaN(new Date(iso).getTime()) ? null : iso;
}

/** The attempts list is newest first; the most recent control-plane attempt, if any. */
export function latestControlPlaneAttempt(
  attempts: PlatformApplyAttempt[],
): PlatformApplyAttempt | undefined {
  return attempts.find((a) => a.target === "control_plane");
}

/**
 * The restore card, when it is useful: the newest control-plane attempt failed a
 * migration, and this page is served by the build that attempt moved to. After the
 * operator runs `restore`, the previous control plane serves the page again and the same
 * attempt is still the newest one, so the card must go.
 *
 * With the attempt's release listed, "served by it" is the installed commit matching the
 * release's. A developer apply (no release) or a release no longer listed cannot be told
 * apart from the build it replaced, so the card then shows for as long as that attempt is
 * the newest control-plane attempt.
 */
export function restoreCardFor(
  attempts: PlatformApplyAttempt[],
  view: PlatformReleaseView,
): FailedMigration | null {
  const latest = latestControlPlaneAttempt(attempts);
  if (!latest) return null;
  const failed = failedMigration(latest, view);
  if (!failed) return null;
  if (failed.release) {
    return commitsMatch(view.installed.control_plane.source_commit, failed.release.source_commit)
      ? failed
      : null;
  }
  return failed;
}

/** The history's line for a failed migration, as the mock's Apply history draws it. */
export function failedMigrationStatus(failed: FailedMigration): string {
  const tail =
    failed.variant === "external"
      ? "your own database"
      : failed.variant === "unknown"
        ? "dump not reported yet"
        : "dump kept";
  return `Failed · not restored · ${tail}`;
}

/** The refusal banner (`update-refused`): the newest control-plane attempt of a release
 *  apply failed backup_failed and the instance is still behind that release. */
export function refusedDump(
  attempts: PlatformApplyAttempt[],
  view: PlatformReleaseView,
): { attempt: PlatformApplyAttempt; release: PlatformRelease | undefined; actorMoved: boolean } | null {
  const latest = latestControlPlaneAttempt(attempts);
  if (!latest || latest.state !== "failed" || latest.reason !== "backup_failed") return null;
  if (latest.kind !== "apply") return null;
  const release = latest.release_id
    ? view.available.find((r) => r.id === latest.release_id)
    : undefined;
  if (release && commitsMatch(view.installed.control_plane.source_commit, release.source_commit)) {
    return null;
  }
  return {
    attempt: latest,
    release,
    actorMoved: latest.requested_digests.some((c) => c.name === "recovery-actor"),
  };
}
