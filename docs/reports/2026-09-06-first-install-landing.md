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
