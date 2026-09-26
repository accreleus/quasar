/**
 * "Must update before it can be managed" (design_handoff_v3 rh06 floor*.png,
 * `pageRhFloor` in assets/pages-rh06.js). The server decides `below_floor`
 * (control-api.md amendment 14 §"below_floor"); this module only reads it, and works
 * out what the update would move from the commits, the same test hostServices uses.
 */

import type {
  Host,
  PlatformApplyAttempt,
  PlatformRelease,
  PlatformReleaseTarget,
  PlatformReleaseView,
} from "../../../api/types";
import { commitsMatch } from "./releasesCopy";

export interface FloorState {
  /** `below`: the host is below the floor. `unknown`: an owned host that has not said
   *  which release it runs, so nothing is offered. */
  kind: "below" | "unknown";
  /** The update the host is offered: `available[0]`, and its target entry. */
  release: PlatformRelease | null;
  target: PlatformReleaseTarget | null;
  /** Which services that update replaces. */
  movesAgent: boolean;
  movesActor: boolean;
  /** The floor the installed release publishes, per component; null when this view
   *  lists no format-2 manifest for the installed control plane. */
  floor: { agent: string | null; actor: string | null };
  /** The host's last attempt, when it failed: the update did not finish. */
  failed: PlatformApplyAttempt | null;
}

interface ManifestFloorEntry {
  name?: unknown;
  version?: unknown;
}

/** The floor a release's format-2 manifest names. */
function manifestFloor(release: PlatformRelease | undefined): FloorState["floor"] {
  const manifest = release?.manifest as { format_version?: unknown; floor?: unknown } | null | undefined;
  const none = { agent: null, actor: null };
  if (!manifest || manifest.format_version !== 2 || !Array.isArray(manifest.floor)) return none;
  const find = (name: string) => {
    const hit = (manifest.floor as ManifestFloorEntry[]).find((f) => f.name === name);
    return typeof hit?.version === "string" ? hit.version : null;
  };
  return { agent: find("node-agent"), actor: find("recovery-actor") };
}

function namesActor(release: PlatformRelease | null): boolean {
  const manifest = release?.manifest as { components?: { name?: unknown }[] } | null | undefined;
  return !!manifest?.components?.some((c) => c.name === "recovery-actor");
}

/**
 * The host's floor state, or null when it is neither below the floor nor an owned host
 * with an unreported identity. `lastAttempt` is the host's newest attempt, if read.
 */
export function hostFloorState(
  host: Host,
  view: PlatformReleaseView | null | undefined,
  lastAttempt?: PlatformApplyAttempt | null,
): FloorState | null {
  // A partial view (an older server, a failed read) names no floor state.
  const identity = view?.installed?.hosts?.find((h) => h.host_id === host.id);
  if (!view || !identity) return null;
  const available = view.available ?? [];
  const release = available[0] ?? null;
  const target = view.targets?.find((t) => t.kind === "host" && t.host_id === host.id) ?? null;
  const installed = available.find((r) =>
    commitsMatch(r.source_commit, view.installed.control_plane?.source_commit),
  );

  if (identity.below_floor) {
    const agentBehind = !release || !commitsMatch(host.source_commit, release.source_commit);
    const actorBehind =
      host.install_mode === "owned" &&
      namesActor(release) &&
      !commitsMatch(host.recovery_actor_source_commit, release?.source_commit);
    return {
      kind: "below",
      release,
      target,
      // A release that names no actor moves the agent alone.
      movesAgent: agentBehind || !actorBehind,
      movesActor: actorBehind,
      floor: manifestFloor(installed),
      failed: lastAttempt?.state === "failed" ? lastAttempt : null,
    };
  }
  if (host.install_mode === "owned" && !identity.identity_known) {
    return {
      kind: "unknown",
      release,
      target,
      movesAgent: false,
      movesActor: false,
      floor: manifestFloor(installed),
      failed: null,
    };
  }
  return null;
}

/** "v0.5.0 and newer", naming each component when their floors differ. */
export function floorPhrase(floor: FloorState["floor"]): string | null {
  const v = (s: string) => (/^\d/.test(s) ? `v${s}` : s);
  if (floor.agent && floor.actor) {
    return floor.agent === floor.actor
      ? `${v(floor.agent)} and newer`
      : `node agents from ${v(floor.agent)} and recovery actors from ${v(floor.actor)}`;
  }
  const one = floor.agent ?? floor.actor;
  return one ? `${v(one)} and newer` : null;
}
