/**
 * An owned GPU host's service inventory (the RH-06 "Services on this machine"
 * mock), derived from the host body's amendment-14 fields. One module so the
 * host page's card and the host row's Services column cannot disagree.
 *
 * Semantics of the fields: control-api.md "Owned hosts on the host body and the
 * release view"; agent-api.md §register, "Owned installs".
 */

import type { Host, PlatformReleaseFault } from "../../../api/types";
import { shortCommit } from "./hostIdentity";
import { commitsMatch } from "./releasesCopy";

export type ServiceKey = "seed" | "recovery_actor" | "database" | "control_plane" | "node_agent";

export type ServiceState =
  | { kind: "running" }
  | { kind: "unknown" }
  /** The last report, from before the host went offline. */
  | { kind: "as_of"; at: string }
  /** The service does not run on this machine at all. */
  | { kind: "absent"; text: string }
  /** The recovery actor answered and found no such container. */
  | { kind: "not_found" };

export interface ServiceRow {
  key: ServiceKey;
  name: string;
  description: string;
  /** "v0.5.2", or null when not reported. */
  version: string | null;
  versionNote: string | null;
  owner: string | null;
  state: ServiceState;
}

export type InventoryReport =
  /** `updater_present: true`, and the agent is connected. */
  | "reported"
  /** `updater_present` null or absent: the agent has not said. */
  | "not_reported"
  /** `updater_present: false`: the recovery actor did not answer. */
  | "not_answering"
  /** `updater_present: true` on an offline host; the rows are its last report. */
  | "offline";

export interface HostServices {
  report: InventoryReport;
  /** When the rows were reported: the agent's last `register`. */
  reportedAt: string | null;
  rows: ServiceRow[];
}

export function isOwned(host: Host): boolean {
  return host.install_mode === "owned";
}

/**
 * The agent is on another build than the control plane, and it is not the
 * `agent_ahead_of_control_plane` fault. ADR 0002 never puts an agent above the
 * control plane, so the other case is behind. Commit equality is the release
 * view's own test (`commitsMatch`); no version ordering is re-derived here.
 */
export function agentOlderThanControlPlane(
  host: Host,
  controlPlaneCommit: string | null,
  faults: PlatformReleaseFault[],
): boolean {
  if (!host.source_commit || !controlPlaneCommit) return false;
  if (commitsMatch(host.source_commit, controlPlaneCommit)) return false;
  return !faults.some((f) => f.kind === "agent_ahead_of_control_plane" && f.host_id === host.id);
}

/** A version reads "v0.5.2"; a non-numeric one ("dev") is shown as sent. */
export function versionLabel(version: string | null | undefined): string | null {
  if (!version) return null;
  return /^\d/.test(version) ? `v${version}` : version;
}

/** Null for a host that is not owned: its page renders as it always has. */
export function hostServices(host: Host, opts: { agentOlder: boolean }): HostServices | null {
  if (!isOwned(host)) return null;

  // `updater_present` is whether the actor answered (agent-api.md §register,
  // "Owned installs"), not `recovery_actor_version`: a branch build reports
  // "dev", which the control plane stores NULL.
  const answered = host.updater_present === true;
  const offline = host.status === "offline";
  const report: InventoryReport = answered
    ? offline
      ? "offline"
      : "reported"
    : host.updater_present === false
      ? "not_answering"
      : "not_reported";
  const reportedAt = host.last_registered_at;
  const lastReport = (): ServiceState =>
    reportedAt ? { kind: "as_of", at: reportedAt } : { kind: "unknown" };
  const liveOrLast = (): ServiceState => (offline ? lastReport() : { kind: "running" });

  const actorVersion = versionLabel(host.recovery_actor_version);
  const seedVersion = versionLabel(host.seed_version);
  const actorCommit = host.recovery_actor_source_commit
    ? ` · commit ${shortCommit(host.recovery_actor_source_commit)}`
    : "";

  const rows: ServiceRow[] = [
    {
      key: "seed",
      name: "Seed",
      description: "Makes sure the recovery actor exists. Never updated by Quasar.",
      // `seed_version` is what the recovery actor saw; absent while it answers means it
      // found no seed. Nothing on the wire says how the seed was started, so one owner
      // label covers a manager's stack and a `docker run` (mock open question 4).
      version: answered ? seedVersion : null,
      versionNote: null,
      owner: answered && seedVersion ? "External manager" : null,
      state: !answered ? { kind: "unknown" } : seedVersion ? liveOrLast() : { kind: "not_found" },
    },
    {
      key: "recovery_actor",
      name: "Recovery actor",
      description: "Creates, updates and recovers the services on this machine, and itself.",
      version: answered ? actorVersion : null,
      versionNote: answered && !actorVersion ? `version not reported${actorCommit}` : null,
      owner: answered ? "Quasar" : null,
      state: answered ? liveOrLast() : { kind: "unknown" },
    },
    {
      key: "database",
      name: "Database",
      description: "No database runs on a GPU host.",
      version: null,
      versionNote: null,
      owner: null,
      state: { kind: "absent", text: "none on this machine" },
    },
    {
      key: "control_plane",
      name: "Control plane",
      description: "Runs on another machine.",
      version: null,
      versionNote: null,
      owner: null,
      state: { kind: "absent", text: "not on this machine" },
    },
    {
      key: "node_agent",
      name: "Node agent",
      description: "Runs this machine’s GPUs and sessions.",
      // The inventory is the recovery actor's report: until it answers, the
      // agent's row waits with the rest, as the mock's "not reported yet" draws it.
      version: answered ? versionLabel(host.agent_version) : null,
      versionNote:
        answered && opts.agentOlder ? "older than the control plane · update from Releases" : null,
      owner: answered ? "Quasar" : null,
      state: answered ? liveOrLast() : { kind: "unknown" },
    },
  ];

  return { report, reportedAt, rows };
}
