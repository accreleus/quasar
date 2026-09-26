/**
 * The control plane's own machine on an owned install (the RH-06 "This machine" block of
 * Releases ▸ Installed, `rhInstalled` in design_handoff_v3 assets/pages-rh06.js), derived
 * from PlatformIdentity's machine fields. Semantics: control-api.md amendment 14, "The
 * control plane's own machine".
 */

import type { PlatformHostIdentity, PlatformIdentity } from "../../../api/types";
import { versionLabel } from "./hostServices";
import { shortCommit } from "./hostIdentity";

/** An identity whose machine answered, and when it was read (ms). */
export interface MachineReport {
  identity: PlatformIdentity;
  at: number;
}

export interface MachineRow {
  key: "seed" | "recovery_actor" | "database" | "control_plane" | "node_agent";
  label: string;
  /** The version or mode; null draws a dash. */
  value: string | null;
  /** A version string (drawn in the numeric face) rather than words. */
  numeric: boolean;
  /** "Quasar · running", "External manager · as of 13:48", "not reported yet". */
  hint: string;
  /** The hint is the whole cell (no value): "none on this machine". */
  hintOnly?: boolean;
}

export type MachineState =
  | "reported"
  /** Owned, but its recovery actor has not answered yet in this page's life. */
  | "not_reported"
  /** It answered earlier in this page's life and does not now; rows are that report. */
  | "not_answering";

export type ThisMachine =
  /** Not an owned install, or an older server: nothing to draw. */
  | { kind: "none" }
  | {
      kind: MachineState;
      /** "attic-server · Control-only host"; null when the server names no shape. */
      title: string | null;
      /** When the last report was read, for `not_answering`. */
      since: number | null;
      rows: MachineRow[];
      external: boolean;
    };

export function isOwnedMachine(identity: PlatformIdentity | undefined | null): boolean {
  return identity?.install_mode === "owned";
}

function shapeLabel(role: string | null | undefined): string | null {
  switch (role) {
    case "combined":
      return "Combined host";
    case "control_only":
      return "Control-only host";
    case null:
    case undefined:
      return null;
    default:
      return "shape unknown";
  }
}

/**
 * `current` is what the control plane says now; `last` the most recent owned report seen
 * in this page's life; `hosts` the release view's hosts, for a combined machine's agent.
 * `asOf` renders a report time ("13:48").
 */
export function thisMachine(
  current: PlatformIdentity,
  last: MachineReport | null,
  hosts: PlatformHostIdentity[],
  asOf: (at: number) => string,
): ThisMachine {
  const role = current.machine_role ?? null;
  const shape = shapeLabel(role);
  const title =
    shape && current.machine_node_name ? `${current.machine_node_name} · ${shape}` : shape;
  const agent = agentRow(current, hosts);
  let state: MachineState;
  let report: PlatformIdentity | null;
  let since: number | null = null;
  let hint: string;
  if (isOwnedMachine(current)) {
    [state, report, hint] = ["reported", current, "running"];
  } else if (last && isOwnedMachine(last.identity)) {
    // A transient failure to reach the actor also reads null (control-api.md).
    [state, report, hint, since] = ["not_answering", last.identity, `as of ${asOf(last.at)}`, last.at];
  } else if (role) {
    [state, report, hint] = ["not_reported", null, ""];
  } else {
    return { kind: "none" };
  }
  const rows = report ? reportedRows(report, hint) : unreportedRows();
  rows.push(controlPlaneRow(current));
  if (agent) rows.push(agent);
  return {
    kind: state,
    title,
    since,
    rows,
    external: report?.database_mode === "external",
  };
}

function reportedRows(report: PlatformIdentity, state: string): MachineRow[] {
  const external = report.database_mode === "external";
  const seed = versionLabel(report.seed_version);
  const actor =
    versionLabel(report.recovery_actor_version) ??
    (report.recovery_actor_source_commit
      ? `commit ${shortCommit(report.recovery_actor_source_commit)}`
      : null);
  const database =
    report.database_mode === "owned" ? "Quasar’s own" : external ? "Your own" : null;
  // The page was served to an authenticated admin, and authentication reads the
  // database: it answered, whoever owns it.
  const dbState = state === "running" && external ? "reachable" : state;
  return [
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
  ];
}

function unreportedRows(): MachineRow[] {
  return (
    [
      ["seed", "Seed"],
      ["recovery_actor", "Recovery actor"],
      ["database", "Database"],
    ] as const
  ).map(([key, label]) => ({ key, label, value: null, numeric: false, hint: "not reported yet" }));
}

function controlPlaneRow(live: PlatformIdentity): MachineRow {
  return {
    key: "control_plane",
    label: "Control plane",
    value: versionLabel(live.version) ?? live.version,
    numeric: true,
    // It is serving this page.
    hint: "Quasar · running",
  };
}

function agentRow(current: PlatformIdentity, hosts: PlatformHostIdentity[]): MachineRow | null {
  if (current.machine_role === "control_only") {
    return {
      key: "node_agent",
      label: "Node agent",
      value: null,
      numeric: false,
      hint: "none on this machine",
      hintOnly: true,
    };
  }
  if (current.machine_role !== "combined") return null;
  // Only on combined, and only by node name (control-api.md).
  const own = hosts.find((h) => h.node_name === current.machine_node_name);
  if (!own) {
    return {
      key: "node_agent",
      label: "Node agent",
      value: null,
      numeric: false,
      hint: "not enrolled yet",
    };
  }
  const version = versionLabel(own.agent_version);
  return {
    key: "node_agent",
    label: "Node agent",
    value: version,
    numeric: version != null,
    hint: `Quasar · ${own.status === "online" ? "running" : own.status}`,
  };
}
