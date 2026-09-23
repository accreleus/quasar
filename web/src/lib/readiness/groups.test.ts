import { describe, expect, it } from "vitest";
import type { ReadinessCheck } from "../../api/types";
import { baseCheckId, groupChecks, KNOWN_CHECK_IDS, READINESS_GROUPS } from "./groups";

function c(id: string, status = "pass", summary = id): ReadinessCheck {
  return { id, status, summary, remediation: "" } as ReadinessCheck;
}

// Every `const ID: &str = "…"` in node-agent/src/readiness.rs,
// readiness/platform_update.rs, node-agent/src/host_probe.rs, and node-agent/src/diagnostic.rs. A check added
// or renamed there must be placed here, or it lands in "Other" unnoticed.
const AGENT_CHECK_IDS = [
  "updater_socket",
  "updater_stack_dir",
  "updater_overlays",
  "health_addr_bindable",
  // Host probes (host_probe.rs).
  "media_probe",
  "application_gpu_probe",
  "input_probe",
  "audio_probe",
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
  "nvidia_vulkan_av1_compatibility",
  // #254: the container runtime (readiness/runtime_facts.rs).
  "runtime_endpoint",
  "runtime_api_version",
  "runtime_capabilities",
  "runtime_cdi",
  // #256: the agent's own safety state (node-agent/src/diagnostic.rs).
  "startup_cleanup",
  "policy_journal",
  // #253: storage (readiness/storage.rs).
  "homes_root_writable",
  "homes_free_space",
  "template_free_space",
  "image_free_space",
  // #261: host container mounts and NVIDIA driver mount.
  "host_container_mounts",
  "nvidia_driver_mount",
];

describe("readiness groups (#102)", () => {
  it("places every agent check id in exactly one named group", () => {
    expect([...KNOWN_CHECK_IDS].sort()).toEqual([...AGENT_CHECK_IDS].sort());
    const seen = new Map<string, number>();
    for (const g of READINESS_GROUPS) for (const id of g.ids) seen.set(id, (seen.get(id) ?? 0) + 1);
    expect([...seen.values()].every((n) => n === 1)).toBe(true);
  });

  it("keeps NVIDIA-related checks together, driver_volume_version included", () => {
    const nvidia = READINESS_GROUPS.find((g) => g.key === "nvidia");
    expect(nvidia?.ids).toEqual(["nvidia_egl_vendor_json", "nvidia_eglcore_library", "nvidia_lib32_gl", "driver_volume_version", "nvidia_vulkan_av1_compatibility", "nvidia_driver_mount"]);
  });

  // #254: the runtime is the most basic fault, so it is the first group.
  it("puts the container runtime checks first, endpoint before what it negotiated", () => {
    expect(READINESS_GROUPS[0].key).toBe("runtime");
    expect(READINESS_GROUPS[0].label).toBe("Container runtime");
    expect(READINESS_GROUPS[0].ids).toEqual(["startup_cleanup", "policy_journal", "runtime_endpoint", "runtime_api_version", "runtime_capabilities", "runtime_cdi", "host_container_mounts"]);
  });

  // #256: diagnostic mode's safety check explains the refusal, so it leads the runtime group.
  it("shows the startup-cleanup safety check under Container runtime, never Other", () => {
    const { groups } = groupChecks([c("runtime_endpoint", "fail"), c("startup_cleanup", "fail")]);
    expect(groups.map((g) => g.key)).toEqual(["runtime"]);
    expect(groups[0].checks.map((check) => check.id)).toContain("startup_cleanup");
  });

  it("shows a policy-journal fault beside runtime safety checks, never Other", () => {
    const { groups } = groupChecks([c("policy_journal", "fail"), c("runtime_endpoint", "pass")]);
    expect(groups.map((g) => g.key)).toEqual(["runtime"]);
    expect(groups[0].checks[0].id).toBe("policy_journal");
  });

  // #261: host_container_mounts and nvidia_driver_mount.
  it("places host_container_mounts in the runtime group", () => {
    const { groups } = groupChecks([c("host_container_mounts"), c("render_node")]);
    const runtime = groups.find((g) => g.key === "runtime");
    expect(runtime?.checks.map((x) => x.id)).toContain("host_container_mounts");
  });

  it("places nvidia_driver_mount in the nvidia group", () => {
    const { groups } = groupChecks([c("nvidia_driver_mount"), c("render_node")]);
    const nvidia = groups.find((g) => g.key === "nvidia");
    expect(nvidia?.checks.map((x) => x.id)).toContain("nvidia_driver_mount");
  });

  // #253: the storage checks sit together, homes first — the two that can block later.
  it("groups the storage checks under Storage, homes first", () => {
    const storage = READINESS_GROUPS.find((g) => g.key === "storage");
    expect(storage?.label).toBe("Storage");
    expect(storage?.ids).toEqual(["homes_root_writable", "homes_free_space", "template_free_space", "image_free_space"]);
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

  it("places unknown status after fail and warn but before pass, never in not-applicable", () => {
    const { groups, notApplicable } = groupChecks([
      c("render_node", "pass"),
      c("application_gpu_probe", "unknown"),
      c("dri_node_app_access", "fail"),
      c("xid_visibility", "warn"),
    ]);
    expect(notApplicable).toEqual([]);
    expect(groups[0].checks.map((x) => x.id)).toEqual([
      "dri_node_app_access",
      "xid_visibility",
      "application_gpu_probe",
      "render_node",
    ]);
  });

  it("sorts an unsupported check with the passes, never as not-applicable (#311)", () => {
    const { groups, notApplicable } = groupChecks([
      c("media_probe_gpu1_av1", "unsupported"),
      c("render_node", "pass"),
      c("xid_visibility", "unknown"),
      c("dri_node_app_access", "fail"),
    ]);
    expect(notApplicable).toEqual([]);
    expect(groups[0].checks.map((x) => x.id)).toEqual([
      "dri_node_app_access",
      "xid_visibility",
      "media_probe_gpu1_av1",
      "render_node",
    ]);
  });

  // Per-GPU host-probe ids land in their base id's group.
  it("places per-GPU media_probe checks in the gpu group alongside their base id", () => {
    const { groups } = groupChecks([c("media_probe_gpu0"), c("media_probe_gpu1"), c("render_node")]);
    const gpu = groups.find((g) => g.key === "gpu");
    expect(gpu?.checks.map((x) => x.id)).toEqual(["media_probe_gpu0", "media_probe_gpu1", "render_node"]);
    const other = groups.find((g) => g.key === "other");
    expect(other).toBeUndefined();
  });

  it("folds per-GPU codec probe checks (media_probe_gpu<N>_<codec>) under the media probe in the gpu group", () => {
    const { groups } = groupChecks([
      c("media_probe_gpu0"),
      c("media_probe_gpu0_h265"),
      c("media_probe_gpu1_av1", "fail"),
      c("render_node"),
    ]);
    const gpu = groups.find((g) => g.key === "gpu");
    expect(gpu?.checks.map((x) => x.id)).toEqual([
      "media_probe_gpu1_av1",
      "media_probe_gpu0",
      "media_probe_gpu0_h265",
      "render_node",
    ]);
    expect(groups.find((g) => g.key === "other")).toBeUndefined();
  });

  it("places input_probe in the input group", () => {
    const { groups } = groupChecks([c("input_probe"), c("uinput")]);
    const input = groups.find((g) => g.key === "input");
    expect(input?.checks.map((x) => x.id)).toEqual(["input_probe", "uinput"]);
  });

  it("places an unknown id ending in _gpu<N> in the other group, not its would-be base", () => {
    const { groups } = groupChecks([c("mystery_gpu3"), c("render_node")]);
    const gpu = groups.find((g) => g.key === "gpu");
    expect(gpu?.checks.map((x) => x.id)).toEqual(["render_node"]);
    const other = groups.find((g) => g.key === "other");
    expect(other?.checks.map((x) => x.id)).toEqual(["mystery_gpu3"]);
  });

  it("baseCheckId strips the _gpu<N> suffix and leaves others unchanged", () => {
    expect(baseCheckId("media_probe_gpu0")).toBe("media_probe");
    expect(baseCheckId("media_probe_gpu12")).toBe("media_probe");
    expect(baseCheckId("application_gpu_probe_gpu1")).toBe("application_gpu_probe");
    expect(baseCheckId("media_probe_gpu0_h265")).toBe("media_probe");
    expect(baseCheckId("media_probe_gpu3_av1")).toBe("media_probe");
    expect(baseCheckId("media_probe_gpu0_vp9")).toBe("media_probe_gpu0_vp9");
    expect(baseCheckId("render_node")).toBe("render_node");
    expect(baseCheckId("input_probe")).toBe("input_probe");
  });

  it("places per-GPU application_gpu_probe checks in the gpu group and sorts after media_probe_gpu<N>", () => {
    const { groups } = groupChecks([c("media_probe_gpu0"), c("application_gpu_probe_gpu0"), c("render_node")]);
    const gpu = groups.find((g) => g.key === "gpu");
    expect(gpu?.checks.map((x) => x.id)).toEqual(["media_probe_gpu0", "application_gpu_probe_gpu0", "render_node"]);
  });

  it("places audio_probe in the audio group", () => {
    const { groups } = groupChecks([c("audio_probe"), c("uinput")]);
    const audio = groups.find((g) => g.key === "audio");
    expect(audio?.label).toBe("Audio");
    expect(audio?.checks.map((x) => x.id)).toEqual(["audio_probe"]);
  });

  it("places the audio group after input and before storage in READINESS_GROUPS order", () => {
    const inputIdx = READINESS_GROUPS.findIndex((g) => g.key === "input");
    const audioIdx = READINESS_GROUPS.findIndex((g) => g.key === "audio");
    const storageIdx = READINESS_GROUPS.findIndex((g) => g.key === "storage");
    expect(inputIdx < audioIdx && audioIdx < storageIdx).toBe(true);
  });
});
