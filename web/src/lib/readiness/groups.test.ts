import { describe, expect, it } from "vitest";
import type { ReadinessCheck } from "../../api/types";
import { groupChecks, KNOWN_CHECK_IDS, READINESS_GROUPS } from "./groups";

function c(id: string, status = "pass", summary = id): ReadinessCheck {
  return { id, status, summary, remediation: "" } as ReadinessCheck;
}

// Every `const ID: &str = "…"` in node-agent/src/readiness.rs. A check added or
// renamed there must be placed here, or it lands in "Other" unnoticed.
const AGENT_CHECK_IDS = [
  "xid_visibility",
  "nvidia_egl_vendor_json",
  "nvidia_eglcore_library",
  "nvidia_lib32_gl",
  "render_node",
  "uinput",
  "user_namespaces",
  "app_apparmor_profile",
  "host_render_node",
  "dri_node_app_access",
  "driver_volume_version",
  "encoder_codecs",
  "media_reachability",
];

describe("readiness groups (#102)", () => {
  it("places every agent check id in exactly one named group", () => {
    expect([...KNOWN_CHECK_IDS].sort()).toEqual([...AGENT_CHECK_IDS].sort());
    const seen = new Map<string, number>();
    for (const g of READINESS_GROUPS) for (const id of g.ids) seen.set(id, (seen.get(id) ?? 0) + 1);
    expect([...seen.values()].every((n) => n === 1)).toBe(true);
  });

  it("keeps the four NVIDIA-related checks together, driver_volume_version included", () => {
    const nvidia = READINESS_GROUPS.find((g) => g.key === "nvidia");
    expect(nvidia?.ids).toEqual(["nvidia_egl_vendor_json", "nvidia_eglcore_library", "nvidia_lib32_gl", "driver_volume_version"]);
  });

  it("demotes skipped checks to not-applicable and omits a group with nothing left to show", () => {
    const { groups, notApplicable } = groupChecks([
      c("nvidia_egl_vendor_json", "skip", "no NVIDIA GPU detected on this host"),
      c("nvidia_eglcore_library", "skip"),
      c("nvidia_lib32_gl", "skip"),
      c("driver_volume_version", "skip"),
      c("render_node"),
      c("uinput"),
    ]);
    expect(groups.map((g) => g.key)).toEqual(["gpu", "input"]);
    expect(notApplicable.map((x) => x.id)).toEqual([
      "nvidia_egl_vendor_json",
      "nvidia_eglcore_library",
      "nvidia_lib32_gl",
      "driver_volume_version",
    ]);
  });

  it("orders groups by area and, within a group, fail then warn then provisioning then pass", () => {
    const { groups } = groupChecks([
      c("media_reachability", "warn"),
      c("encoder_codecs"),
      c("xid_visibility", "provisioning"),
      c("render_node", "fail"),
      c("uinput"),
      c("nvidia_lib32_gl"),
    ]);
    expect(groups.map((g) => g.key)).toEqual(["gpu", "nvidia", "input", "network"]);
    expect(groups[0].checks.map((x) => x.id)).toEqual(["render_node", "xid_visibility", "encoder_codecs"]);
  });

  it("keeps a check with an unknown id under Other rather than dropping it", () => {
    const { groups } = groupChecks([c("render_node"), c("brand_new_probe", "fail")]);
    const other = groups.find((g) => g.key === "other");
    expect(other?.label).toBe("Other");
    expect(other?.checks.map((x) => x.id)).toEqual(["brand_new_probe"]);
  });

  it("treats an unknown status as advisory: after failures, before passes, never not-applicable", () => {
    const { groups, notApplicable } = groupChecks([c("render_node"), c("host_render_node", "mystery"), c("dri_node_app_access", "fail")]);
    expect(notApplicable).toEqual([]);
    expect(groups[0].checks.map((x) => x.id)).toEqual(["dri_node_app_access", "host_render_node", "render_node"]);
  });
});
