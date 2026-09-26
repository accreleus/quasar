/**
 * The RH-06 host warnings (design_handoff_v3 fleet-rh06-v3.html, `pageRhWarning`;
 * screenshots rh06/seed-missing, conflict, conflict-error): a seed the recovery actor
 * cannot find, and a container in the way it never acts on. One module so the host
 * page's notes and the host row's chip cannot disagree.
 *
 * Semantics: agent-api.md §register "Owned installs" (`seed_version`), control-api.md
 * amendment 14 §"Preflight" (`owner_conflict`, which a host reports as its readiness
 * check of the same id).
 */

import type { Host, ReadinessCheck } from "../../../api/types";
import { isOwned } from "./hostServices";

export const OWNER_CONFLICT = "owner_conflict";

/**
 * The recovery actor answered and saw no running seed. `seed_version` is opaque: any
 * value, `unknown` included, is a seed that exists (ADR 0007), so only its absence
 * while the actor answers means "not found". An offline host's report is old news.
 */
export function seedMissing(host: Host): boolean {
  return (
    isOwned(host) &&
    host.updater_present === true &&
    host.status !== "offline" &&
    (host.seed_version == null || host.seed_version === "")
  );
}

/** The failing `owner_conflict` readiness check, when the host reports one. */
export function ownerConflict(host: Host): ReadinessCheck | null {
  if (!isOwned(host)) return null;
  return (
    (host.readiness ?? []).find((c) => c.id === OWNER_CONFLICT && c.status === "fail") ?? null
  );
}

export type HostFlag = { label: string; title: string };

/** The one attention chip a host row carries for these, the conflict first. */
export function hostFlag(host: Host): HostFlag | null {
  if (ownerConflict(host)) {
    return {
      label: "owner conflict",
      title: "Another owner’s container is in the way: Quasar will not update this machine",
    };
  }
  if (seedMissing(host)) {
    return {
      label: "no seed",
      title: "No seed found: a deleted recovery actor would not be re-created",
    };
  }
  return null;
}
