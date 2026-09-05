/**
 * Readiness checks arrive as a flat, agent-ordered list (`Host.readiness`).
 * This is the console's view of it (#102): checks grouped by the area an
 * operator would fix, skipped ones set aside as not applicable to the host —
 * the contract defines `skip` as exactly that, never "could not tell".
 *
 * The map is keyed on the agent's stable check ids (node-agent/src/readiness.rs);
 * groups.test.ts pins it against that list so a new or renamed check cannot
 * land in "Other" unnoticed. Other still exists so an unknown id is shown,
 * never dropped.
 */
import type { ReadinessCheck } from "../../api/types";

export interface ReadinessGroupDef {
  key: string;
  label: string;
  ids: readonly string[];
}

export const READINESS_GROUPS: readonly ReadinessGroupDef[] = [
  {
    key: "gpu",
    label: "GPU & display",
    ids: ["render_node", "host_render_node", "dri_node_app_access", "xid_visibility", "encoder_codecs"],
  },
  {
    key: "nvidia",
    label: "NVIDIA driver",
    ids: ["nvidia_egl_vendor_json", "nvidia_eglcore_library", "nvidia_lib32_gl", "driver_volume_version"],
  },
  { key: "input", label: "Input & sandbox", ids: ["uinput", "user_namespaces", "app_apparmor_profile"] },
  { key: "network", label: "Network", ids: ["media_reachability"] },
];

export const OTHER_GROUP: ReadinessGroupDef = { key: "other", label: "Other", ids: [] };

export const KNOWN_CHECK_IDS: readonly string[] = READINESS_GROUPS.flatMap((g) => g.ids);

/** fail → warn → provisioning/unknown → pass. Unknown statuses are advisory:
 *  shown with the actionable ones, never hidden as not applicable. */
function rank(status: string): number {
  switch (status) {
    case "fail":
      return 0;
    case "warn":
      return 1;
    case "pass":
      return 3;
    default:
      return 2;
  }
}

export interface ReadinessGroup {
  key: string;
  label: string;
  checks: ReadinessCheck[];
}

export interface GroupedReadiness {
  /** Groups with at least one applicable check, in area order; Other last. */
  groups: ReadinessGroup[];
  /** `skip` checks, in the agent's order. */
  notApplicable: ReadinessCheck[];
}

export function groupChecks(checks: readonly ReadinessCheck[]): GroupedReadiness {
  const notApplicable = checks.filter((c) => c.status === "skip");
  const applicable = checks.filter((c) => c.status !== "skip");
  const groupOf = new Map<string, ReadinessGroupDef>();
  for (const g of READINESS_GROUPS) for (const id of g.ids) groupOf.set(id, g);

  const buckets = new Map<string, ReadinessCheck[]>();
  for (const c of applicable) {
    const g = groupOf.get(c.id) ?? OTHER_GROUP;
    const list = buckets.get(g.key) ?? [];
    list.push(c);
    buckets.set(g.key, list);
  }

  const groups: ReadinessGroup[] = [];
  for (const g of [...READINESS_GROUPS, OTHER_GROUP]) {
    const list = buckets.get(g.key);
    if (!list || list.length === 0) continue;
    const order = new Map(g.ids.map((id, i) => [id, i]));
    const sorted = [...list].sort((a, b) => {
      const r = rank(a.status) - rank(b.status);
      if (r !== 0) return r;
      return (order.get(a.id) ?? Number.MAX_SAFE_INTEGER) - (order.get(b.id) ?? Number.MAX_SAFE_INTEGER);
    });
    groups.push({ key: g.key, label: g.label, checks: sorted });
  }
  return { groups, notApplicable };
}
