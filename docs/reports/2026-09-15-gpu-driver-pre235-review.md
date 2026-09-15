# GPU driver validation before #235

Assessment of `feature/rh-01-runtime-api` at `31ea524`; runtime candidate source `65a53b0`, image `quasar-node-agent:20260915-0811-rh01-234`. This is a pre-implementation review, not GPU certification or authorization to change host drivers. No #235 code or issue changes were made.

## Direction

Support NVIDIA, AMD and Intel through common capability and lifecycle contracts, with vendor-specific driver preparation where demonstrated necessary. Keep Intel/AMD userspace drivers in the versioned images. Retain the existing NVIDIA userspace provisioner for missing/mismatched graphics libraries. Do not generalize NVIDIA's installer/extraction mechanism to Intel without evidence of a need.

Separate three responsibilities:

1. Host kernel and firmware: detect missing/incompatible support and report corrective actions. A container image cannot replace the running host kernel.
2. Image userspace: ship the correct graphics and media drivers with their dependencies; validate the realized candidate artifact.
3. Runtime access: verify the selected device, permissions, mounts, driver loading and real encode/session behavior through owned disposable helpers.

The NVIDIA extractor runs as a child of the agent and writes userspace libraries into its mounted driver volume; it does not install the host kernel module. #235 migrates surrounding container helpers and inspections while preserving that behavior. See `node-agent/src/nvidia_volume.rs` module contract, `session/container.rs::probe_nvidia_lib32_path`, and `nvidia_volume.rs::probe_sibling_egl`.

## Reconciled Intel issue history

[#126](https://github.com/accreleus/quasar/issues/126) identifies the reporter's processor as **i3-12100**. Intel identifies that processor's integrated graphics as **UHD 730** ([processor list](https://www.intel.com/content/www/us/en/ark/products/series/217840/12th-generation-intel-core-i3-processors.html)).

The issue includes successive, partly superseded diagnoses. Inventory detection was fixed first; the full Intel VA driver and Vulkan Video opt-in followed and shipped in v0.3.0. The latest comment closes #126 after the maintainer relayed the original reporter's successful Intel iGPU test on 2026-09-13. This is a real reported result, but the closing note does not record exact codec, encoder, kernel, image digest or a full acceptance trace.

[#149](https://github.com/accreleus/quasar/issues/149) remains open. Its later comment corrects the body: driver packaging and ANV opt-in were implemented, and the older blanket generation-based Vulkan encode explanation was incorrect for the pinned Mesa. Reconcile this issue before commissioning duplicate fixes. Do not infer that the i3 result certifies Arc A-series or Battlemage, or that B580 working under Wolf proves Quasar support.

## Artifact and upstream findings

The running candidate contains:

| Package | Installed version |
|---|---|
| intel-media-driver | 26.1.5-1.fc43 |
| intel-gmmlib | 22.10.0-2.fc43 |
| libva | 2.22.0-6.fc43 |
| mesa-vulkan-drivers-freeworld | 25.3.6-1.fc43 |
| mesa-va-drivers-freeworld | 25.3.6-1.fc43 |

`deploy/Dockerfile.vulkan` explicitly installs RPM Fusion's full `intel-media-driver`; `deploy/image-contract.json` asserts it and `ANV_DEBUG=video-encode`. The candidate's iHD shared-library dependencies resolve. None of these facts proves Intel encode without Intel hardware. Intel's default remains VA (`session/settings.rs::encoder_default_for_vendor`); the encoder candidate list includes both ordinary and low-power VA elements.

Intel's [26.1.5 release](https://github.com/intel/media-driver/releases/tag/intel-media-26.1.5) explicitly includes Alder Lake, DG2/Alchemist and BMG/Battlemage, and lists GmmLib 22.10.0 and Libva 2.23.0. The candidate's older libva is a compatibility item to investigate through package provenance and actual driver initialization/encode, not a confirmed failure or reason to change dependencies speculatively.

Intel's [driver documentation](https://github.com/intel/media-driver#known-issues-and-limitations) describes HuC firmware requirements for low-power bitrate control. Firmware loading is a host responsibility; a populated container driver directory does not prove it is working.

Intel's [Xe hardware table](https://dgpu-docs.intel.com/overview/supported-hardware/xe-driver-gpus.html) lists B570/B580 as Xe-driver devices, with initial kernel 6.11 support and Ubuntu full-support guidance at 6.12. Intel cautions that distro backports matter. Treat these as investigation guidance, not an unconditional Quasar/Unraid minimum or a claim that every B-series SKU has the same requirements. Alder Lake and Arc A-series appear in the [i915 table](https://dgpu-docs.intel.com/overview/supported-hardware/i915-driver-gpus.html).

Inference: no evidence currently justifies an Intel download/extract driver volume. The existing userspace strategy covers the relevant driver families; the missing evidence concerns actual host support, media functionality and application graphics on each class.

## Validation matrix before and after migration

| Target | Available evidence | Required next validation |
|---|---|---|
| AMD integrated GPU on local development host | #234 exact candidate: GPU contract, VA H.264 video, app-originated browser audio, bounded teardown; operator heard audio | Preserve as baseline; repeat affected helpers and one real session after #235. This is not all AMD hardware/codec certification. |
| NVIDIA on gpu-test | Host reached after the operator corrected its private address; baseline Vulkan pattern/audio and teardown passed | Validate migrated helper behavior and fresh driver-volume provisioning on the reviewed candidate. |
| Intel i3-12100 / UHD 730 | #126 reported working, v0.3.0 contains fixes | Capture exact image/kernel/driver/encoder/codec and playable-session evidence for the runtime migration. |
| Intel Arc A-series / Alchemist | Upstream media-driver support, no inspected Quasar session evidence | Real Arc device: driver initialization, advertised versus working codecs, render/input/audio and cleanup. |
| Intel Arc B570/B580 / Battlemage | Upstream userspace support plus Xe host requirements, no inspected Quasar session evidence | Same checks on Xe, including firmware state and userspace compatibility. |

For each target record PCI ID, selected render node, loaded kernel driver, firmware state where relevant, image ID and package versions. Run VA/Vulkan probes with a fresh temporary GStreamer registry. Distinguish codec registration from a bounded actual encode, then from a visible/interactive browser session. Use 64-bit and 32-bit application graphics checks where required; Intel/AMD application Mesa libraries and NVIDIA library injection must work in the application image as well as the agent.

NVIDIA additionally needs successful existing-volume use, fresh provisioning into a uniquely owned disposable volume, stale/mismatched manifest rejection, lib32 discovery and sibling EGL tests. Negative tests must simulate missing/corrupt inputs only in owned fixtures, never damage the installed host driver or production volume. Preserve host-injected driver precedence, identity, cleanup obligations and bounded diagnostic results.

For Intel, validate H.264 first through the existing VA default; test HEVC/AV1 only where the device and selected stack expose them. Exercise Vulkan separately when investigating that path. Do not classify a missing Vulkan queue as proof that VA hardware encode is impossible.

## Scope and next step

Use #235 for the owned runtime migration and vendor-neutral preservation tests. Track any demonstrated missing readiness check or packaging defect against its appropriate existing issue; do not turn this ticket into speculative driver installation or change frozen contracts.

Operator-approved scope: implement and validate AMD/NVIDIA now, preserving Intel behavior and preparing an external test procedure. Intel hardware access is not a blocker. Reconcile #149's stale description with #126's final confirmation; untested Intel classes remain explicitly unverified. Baseline diagnostics are in `.diagnostics/rh01-235-precheck/` and `.diagnostics/rh01-235/nvidia-baseline/`. The bounded NVIDIA baseline session ran after host access was corrected. No #235 candidate deployment, fresh provisioning or host driver change is claimed by this review.
