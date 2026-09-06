# First-install integration and ticket audit

The operator authorized landing the tested deployment work into `develop` after
confirming clean Vulkan AV1 streaming on Unraid with manually installed NVIDIA
610.57.04. The production deployment and synthetic encoder evidence are in
[the live report](2026-09-06-first-install-live.md) and
[the AV1 comparison](2026-09-06-av1-vulkan-driver-comparison.md).

Current `develop` was merged into `fix/131-first-install-recovery` without
conflicts, retaining the newer release/apply and protocol-submodule changes.
No new frozen-contract changes were authored.

## Corrections found by the landing checks

- CI's `TestEnrollHostComposeMatchesBase` caught the additional-host enrollment
  script still mounting the optional `/sys/kernel/security` child. Its embedded
  Compose now uses `/sys/kernel:/host/sys/kernel:ro`, matching the canonical
  Compose and public-site generator. The parity test remains unchanged.
- The existing template publication concurrency test could finish the writer
  before the reader thread was scheduled. It now waits for the reader's first
  successful observation before publishing. Assertions against missing or torn
  metadata remain intact; no runtime template behavior changed.

## Ticket disposition

| Ticket | Implemented and evidenced | Remaining acceptance / disposition |
|---|---|---|
| #129 | Kernel provisioning lock with abandoned-marker recovery test; provisioning retry; background readiness refresh; shared setup/admin readiness rendering; documented provisioning | Keep open for a controlled live interrupted graphics download, progress visible in both UIs, and network/digest/disk failure remediation checks. Tower's successful NVRTC installation is not that test. |
| #130 | Structured Docker identity-file discovery, rejection of filesystem IDs, named-volume injection, retry and readiness/launch refusal for unresolved required mounts; regression tests | Keep open: the originally requested explicit host-path override and its override-specific remediation are not implemented. Do not describe that acceptance criterion as satisfied. |
| #131 | Repository-derived Compose, credential handling, image/runtime defaults, ownership preparation, recovery/readiness protections; two live Unraid deployment blockers fixed | Keep open for first-time setup to persist the administrator's detected browser origin, omission of an unset origin override from generated environment, accurate signaling errors, and diagnostic evidence sufficiency. |
| #143 | Evidence-scoped AV1 exclusion before codec advertising and Vulkan/NVENC resolution; shared readiness/setup explanation; eligible HEVC/H.264 negotiation; public documentation | Included in this delivery at the operator's request. Final GPU/image validation and merge evidence are attached to the PR before closeout. |

The operator explicitly agreed that setup should use the detected browser URL to
configure access. Implement that through the authenticated administrator's setup
action, honoring explicitly pinned policy. Do not silently trust arbitrary
request headers or reinterpret an explicitly empty policy override.

## Evidence boundaries

The existing candidate image contracts passed (control 23/23, agent 139/139,
updater 15/15); Tower startup, automatic NVRTC provisioning/restart, direct and
proxied media transport, H.264 visual output and operator-confirmed HEVC/AV1
are recorded separately. No replacement of a stale provisioned graphics volume,
full audio/input certification, AMD/Intel acceptance, or published-release
fresh-install acceptance is claimed. Merging the source does not redeploy Tower
or publish a stable release.

The original implementation report is historical: its missing-harness verify
failure was subsequently resolved. The landing check results are recorded in the
PR and issue updates rather than treating that old report as current gate status.

The public-site audit updates installation, prerequisites, first-run setup,
reverse-proxy access, host readiness, encoder tuning, troubleshooting, upgrades
and environment references. It removes the disproven blanket driver-volume
Vulkan restriction and unsafe NVENC recommendation, distinguishes host kernel
drivers from provisioned userspace, retains the manual origin workaround, and
states that `develop` changes require a compatible published release before an
existing installation receives them. The AV1 exclusion is host-wide for Vulkan
and NVENC because the current codec advertisement is host-wide; mixed-GPU hosts
are treated conservatively. Unknown versions are not claimed validated.

## AV1 compatibility candidate validation

The agent image `quasar-node-agent:20260906-1053-av1-compat-143`, built through
`deploy/build-images.sh` from `c20193d67fae`, passed its runtime contract:
**139 passed, zero failed**. Later changes to agent source add tests only.

Isolated `probe-encoder` containers exercised the shared production resolver,
encoder builder and bitstream chain at 1280×720, 30 fps, for two seconds:

| Host kernel driver | Identity visible to probe | Requested path | Result |
|---|---|---|---|
| 610.57.04 | Actual RTX 5090 / 610.57.04 | Vulkan AV1 | Passed with `vulkanav1enc`, VulkanImage input and AV1 output |
| 610.57.04 | Simulated 595.99.02 version file | Vulkan AV1 | Rejected before encoder construction, with compatibility guidance |
| 610.57.04 | Simulated 595.99.02 version file | NVENC AV1 | Rejected before encoder construction, with the same guidance |
| 610.57.04 | Simulated 595.99.02 version file | Vulkan H.264 | Passed with `vulkanh264enc` |
| 610.57.04 | Simulated 595.99.02 version file | Vulkan HEVC | Passed with `vulkanh265enc`, profile `main` |

[Raw probe reports](2026-09-06-av1-evidence/compatibility-probes.txt) include the
requested and effective encoders. The simulated cases bind a version-file fixture
only into temporary probe containers; the host driver remained 610.57.04.
These validate exclusion behavior, not output on an actual 595 kernel. Actual
595 corruption and operator-confirmed clean 610 streaming are documented in the
separate comparison report. The production stack and its running session were
left in place.

The first isolated probe omitted the normal agent's driver-volume activation
environment and resolved AV1 to NVENC. That result was discarded as Vulkan
validation. Candidate and working Vulkan plugin checksums were identical. The
corrected probes reproduced `nvidia_volume::process_env()` (EGL vendor/platform
paths, GBM backend and additive Vulkan ICD discovery) with a fresh GStreamer
registry, and explicitly checked the effective encoder. No toolchain defect was
established.

The control-plane profile endpoint now considers reported codecs from candidate
hosts, using existing app-image, GPU-binding and derived-home gates. Its existing
`host_encoder_not_supported` reason makes an unavailable AV1 rung ineligible;
the Auto preview selects a permitted, decodable rung. A fresh database-backed
HTTP test covers AV1 exclusion, HEVC eligibility, restoration after a host
re-report, and legacy unreported capabilities. The menu is advisory across a
mixed-capability fleet; the placed host's launch checks remain authoritative.
