/**
 * The control plane's own machine on an owned install (the RH-06 "This machine" block of
 * Releases ▸ Installed, `rhInstalled` in design_handoff_v3 assets/pages-rh06.js), derived
 * from PlatformIdentity's machine fields. Semantics: control-api.md amendment 14, "The
 * control plane's own machine".
 */

import type { PlatformIdentity } from "../../../api/types";
import { versionLabel } from "./hostServices";
import { shortCommit } from "./hostIdentity";

/** An identity whose machine answered, and when it was read (ms). */
export interface MachineReport {
  identity: PlatformIdentity;
  at: number;
}

export interface MachineRow {
  key: "seed" | "recovery_actor" | "database" | "control_plane";
  label: string;
  /** The version or mode; null draws a dash. */
  value: string | null;
  /** A version string (drawn in the numeric face) rather than words. */
  numeric: boolean;
  /** "Quasar · running", "External manager · as of 13:48", "not found". */
  hint: string;
}

export type ThisMachine =
  /** Not an owned install, or never reported in this session: nothing to draw. */
  | { kind: "none" }
  | { kind: "reported"; rows: MachineRow[]; external: boolean }
  /** It answered earlier in this session and does not now; rows are that report. */
  | { kind: "not_answering"; since: number; rows: MachineRow[]; external: boolean };

export function isOwnedMachine(identity: PlatformIdentity | undefined | null): boolean {
  return identity?.install_mode === "owned";
}

/**
 * `current` is what the control plane says now; `last` the most recent owned report seen
 * in this page's life. `asOf` renders a report time ("13:48").
 */
export function thisMachine(
  current: PlatformIdentity,
  last: MachineReport | null,
  asOf: (at: number) => string,
): ThisMachine {
  if (isOwnedMachine(current)) {
    return { kind: "reported", ...rows(current, "running") };
  }
  // A transient failure to reach the actor also reads null; only a machine that did
  // answer before is known to be owned.
  if (last && isOwnedMachine(last.identity)) {
    return {
      kind: "not_answering",
      since: last.at,
      ...rows(last.identity, `as of ${asOf(last.at)}`, current),
    };
  }
  return { kind: "none" };
}

function rows(
  report: PlatformIdentity,
  state: string,
  live: PlatformIdentity = report,
): { rows: MachineRow[]; external: boolean } {
  const external = report.database_mode === "external";
  const seed = versionLabel(report.seed_version);
  const actor =
    versionLabel(report.recovery_actor_version) ??
    (report.recovery_actor_source_commit
      ? `commit ${shortCommit(report.recovery_actor_source_commit)}`
      : null);
  const database =
    report.database_mode === "owned"
      ? "Quasar’s own"
      : report.database_mode === "external"
        ? "Your own"
        : null;
  // The page was served to an authenticated admin, and authentication reads the
  // database: it answered, whoever owns it.
  const dbState = state === "running" && external ? "reachable" : state;
  return {
    external,
    rows: [
      {
        key: "seed",
        label: "Seed",
        value: seed,
        numeric: seed != null,
        // The actor reports a seed only when one runs; absent reads "no seed found".
        hint: seed ? `External manager · ${state}` : "not found",
      },
      {
        key: "recovery_actor",
        label: "Recovery actor",
        value: actor,
        numeric: actor != null,
        hint: `Quasar · ${state}`,
      },
      {
        key: "database",
        label: "Database",
        value: database,
        numeric: false,
        hint: database ? `${external ? "You" : "Quasar"} · ${dbState}` : "unknown",
      },
      {
        key: "control_plane",
        label: "Control plane",
        value: versionLabel(live.version) ?? live.version,
        numeric: true,
        // It is serving this page.
        hint: "Quasar · running",
      },
    ],
  };
}
