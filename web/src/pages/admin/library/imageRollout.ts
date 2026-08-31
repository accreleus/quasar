// Pure per-image version/rollout summaries for the Images tab and image
// detail page (handoff §A.13/§A.14). No React, no fetch — inputs are exactly
// what GET /v1/admin/images and GET /v1/hosts already return.
//
// The wire's per-host state is absent/pulling/building/ready/failed — there is
// no "stale" value. The mock's per-host "stale" is derived here, display-only:
// a host reporting "ready" whose own `version` trails the image's
// `installed_version` (the version every host is meant to converge to) is
// stale, and is excluded from the ready count.

import type { CatalogImage, Host, ImageUpdatePolicy } from "../../../api/types";
import { dominantInFlightState, HOST_STATE_COPY } from "./imageStatus";

export interface ImgVersion {
  version: string;
  sub: string;
  tone?: "info" | "warning";
}

/** §A.13: `{version}` plus a sub of Downloading…/Building… (the same labels
 *  the per-host chips use — HOST_STATE_COPY, not a generic "installing") /
 *  not installed / running {installed} / up to date. In-flight beats
 *  everything else — a real install/update already running must not also
 *  read as "update available". */
export function imgVersion(img: CatalogImage): ImgVersion {
  const inFlight = dominantInFlightState(img);
  if (inFlight) return { version: img.version, sub: HOST_STATE_COPY[inFlight].label, tone: "info" };
  if (!img.installed) return { version: img.version, sub: "not installed" };
  if (img.update_available) {
    return { version: img.version, sub: `running ${img.installed_version ?? img.version}`, tone: "warning" };
  }
  return { version: img.version, sub: "up to date" };
}

export type HostImageState = "ready" | "stale" | "pulling" | "building" | "failed" | "absent";

/** One host's state for one image, with "stale" derived on top of the wire
 *  enum (see module doc). Shared by `imgRollout` (fleet-wide summary) and the
 *  image detail page's Per host table (one row per host). */
export function hostImageState(img: CatalogImage, hostId: string): HostImageState {
  const state = (img.hosts ?? []).find((h) => h.host_id === hostId);
  if (!state || state.state === "absent") return "absent";
  if (state.state === "ready") {
    const stale =
      img.installed_version != null && state.version != null && state.version !== img.installed_version;
    return stale ? "stale" : "ready";
  }
  return state.state;
}

export interface ImgRollout {
  ready: number;
  total: number;
  tone?: "success" | "warning";
  /** Per-host exceptions ("node-2 stale", "node-3 pulling"), plus a trailing
   *  "not on …" entry for hosts the image reported nothing for. Empty when
   *  every host is ready and current. */
  exceptions: string[];
  /** "ready on every host" when `exceptions` is empty, else `exceptions`
   *  joined with " · ". Callers needing the mock's dash state for "no host has
   *  ever reported this image" check `img.hosts` themselves — see ImagesTab. */
  note: string;
}

export function imgRollout(img: CatalogImage, hosts: Host[]): ImgRollout {
  const total = hosts.length;
  const exceptions: string[] = [];
  const missing: string[] = [];
  let ready = 0;

  for (const host of hosts) {
    const state = hostImageState(img, host.id);
    if (state === "absent") {
      missing.push(host.node_name);
    } else if (state === "ready") {
      ready++;
    } else {
      exceptions.push(`${host.node_name} ${state}`);
    }
  }

  if (missing.length > 0) {
    exceptions.push(missing.length > 2 ? `not on ${missing.length} hosts` : `not on ${missing.join(", ")}`);
  }

  return {
    ready,
    total,
    tone: exceptions.length > 0 ? "warning" : "success",
    exceptions,
    note: exceptions.length === 0 ? "ready on every host" : exceptions.join(" · "),
  };
}

/** Instance-wide image update policy copy — shared by the Images tab's
 *  policy card and the image detail rail's "Update policy" link. */
export const POLICY_COPY: Record<ImageUpdatePolicy, { label: string; desc: string }> = {
  manual: { label: "Manual", desc: "Sync only refreshes the catalog. Nothing installs or re-adopts on its own." },
  notify: { label: "Notify", desc: "Same as manual, plus the update badge is surfaced more prominently. Update now applies it." },
  auto: { label: "Auto", desc: "After a sync, every installed, unpinned image with a newer catalog version re-adopts and re-ensures automatically. Pinned images are left alone." },
};
