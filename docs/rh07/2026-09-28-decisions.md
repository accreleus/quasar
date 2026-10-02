# RH-07 decisions: engines, engine modes and least privilege

Agreed with the owner on 2026-09-28, in a question-by-question review of RH-07's scope.
This record is the product authority for RH-07. Where the original tracker text in #220
and #221 differs, this record wins. The survey of where Quasar assumes Docker today is
`2026-09-28-research-engine-assumptions.md`. Terms are defined in `CONTEXT.md`, "Engines
and privilege".

## Guiding principle

**P1: least privilege.** Quasar holds the container engine, so it has the potential to be
exploited. A compromised Quasar must not mean a compromised machine. Every container asks
for the least access that does its job, in every engine mode. A capability that only some
hosts can grant is optional, and says why it is missing. It is never obtained by
escalating privilege, and there is no silent privileged fallback.

## Decisions

| ID | Decision |
|---|---|
| D1 | **Four engine modes, one recipe.** Docker and Podman, each rootful and rootless, are built from one least-privilege container recipe. Rootless is the design target; rootful is the same recipe with fewer limits. Rootful installs also give up what rootless cannot have. |
| D2 | **Whole machine.** A rootless install runs everything on the machine (control plane, Postgres, recovery actor, node agent, session containers) under one engine and one mode. |
| D3 | **No default engine yet.** The quick start and the enrollment command detect the engine and its mode and meet the operator where they are. Once the rootless profiles pass, the quick start recommends rootless where the host supports it. |
| D4 | **Unraid stays on rootful Docker.** Unraid has neither Podman nor rootless Docker. Nothing gets worse for it. |
| D5 | **Required engine profiles.** On Fedora, with AMD and with NVIDIA: Docker rootful, Docker rootless and Podman rootless must pass. Podman rootful is claimed if it passes. Ubuntu 24.04 is experimental until someone runs it. Every other combination is unsupported and says so. |
| D6 | **Release gate.** RH-07 gates the first `main` release. The first stable release of owned installs also withdraws amendment 16's edge default (`settings.OwnedInstallReleaseChannel`), with release notes telling edge installs how to switch. |
| D7 | **Quasar user.** A rootless install runs under a dedicated, unprivileged `quasar` account, never the operator's own login. |
| D8 | **Host preparation.** One idempotent root step, which can be re-run safely and says what each change does: kernel settings, udev rules, the NVIDIA CDI specification, subordinate UID/GID ranges and lingering. It runs as root once. Quasar never runs as root. It writes only under `/etc`, so it works on image-based (atomic) Fedora. |
| D9 | **Input the Linux way.** Access to `/dev/uinput` and to the input devices Quasar creates is granted by a udev group rule, the model `steam-devices` uses for Steam Input. The agent passes host-created device nodes into session containers instead of calling `mknod` inside them. |
| D10 | **NVIDIA through CDI.** CDI is the one GPU-injection mechanism in every mode. `--gpus` / DeviceRequests remain only as a fallback for rootful Docker without CDI. |
| D11 | **Almost nothing is lost rootless.** Firewall-rule inspection is replaced by an active reachability probe, which is better evidence. GPU fault (Xid) visibility needs an optional host setting (`kernel.dmesg_restrict=0`) and otherwise reports why it is skipped. DDC monitor control works through the i2c group rule. |
| D12 | **Console mode is required, in all four modes.** RH-06's owned installs lost it (a regression). An early ticket restores it on rootful owned installs; rootless console mode follows. Display, sound and i2c come through device group rules; taking control of the display should not need `SYS_ADMIN` when no other process holds it, and this must be proven on hardware. |
| D13 | **Console audio through PipeWire.** Console mode's local audio goes to the host's PipeWire when one exists, and to ALSA only when none does. Moving each session's PulseAudio sidecar to PipeWire is a separate issue, scheduled after RH-07. |
| D14 | **Homes belong to the Quasar user.** Session containers map their app user onto the Quasar user (Podman `keep-id`, or the Docker equivalent), so home files on the host are owned by `quasar`, not by a high subordinate ID. Backups and shared storage behave normally. |
| D15 | **Podman installs with Quadlet.** A Quadlet systemd unit is the documented Podman install. A `podman run` seed is for trying it out. Stack managers are out of scope for Podman. |
| D16 | **Moving to rootless is a fresh install** followed by #380's dump restore. No in-place conversion. |
| D17 | **Rootful socket honesty.** The docs say plainly that on a rootful engine, the engine socket is equivalent to root. A request filter in front of the socket is a follow-up issue, not RH-07. |
| D18 | **Delivery.** Work integrates on `initiative/resilient-host-architecture` and is promoted to develop with owner approval. A new RH-07 specification issue and tickets replace #220 and #221, which close as superseded once the specification is published. |
| D19 | **Done means an acceptance map.** For each required profile, on AMD and NVIDIA: a real Steam session with input and audio, an update applied from the console, and a reboot after which everything comes back. Plus console mode on one host per engine. Bench runs are recorded. |
| D20 | **Contracts.** Additive frozen-contract changes (readiness checks, host facts) stand if an independent Opus review says APPROVED, as RH-06's did; anything else goes back to the owner. |
| D21 | **Mockups first.** The owner approves mockups (the quick start's engine choice, console mode's settings on owned installs) before that UI is built. |
| D22 | **One seed-interface amendment.** ADR 0007 may be amended once for Podman's restart behaviour and for the engine socket path, under the D20 rule. Docker users see no change. |
| D23 | **Lab.** The owner provides a Bazzite or uCore (Fedora Atomic) VM with a rootless environment, and login details. It is the must-pass Fedora profile: SELinux enforcing and a read-only `/usr`. Nothing on the hypervisor changes without asking. |

## Consequences drawn from the research

These follow from the decisions above and the engine survey, and shape the tickets:

- **The runtime's strict read-back of what it created must allow engine equivalents.**
  It currently rejects any runtime other than runc, any user-namespace mode and any
  device permission other than `rwm`, which would refuse every launch on crun or Podman.
  It must compare what Quasar asked for with what the engine reports it did, per engine.
- **Engine socket discovery gains Podman's paths** (`$XDG_RUNTIME_DIR/podman/podman.sock`,
  `/run/podman/podman.sock`) and `CONTAINER_HOST`, and records which engine and mode it
  found as host facts.
- **Restart and health semantics differ on Podman.** Restart policies need
  `podman-restart.service` (or Quadlet), and healthchecks need a systemd user session.
  Installs and replacements wait on health, so this is on the critical path.
- **SELinux:** every container that mounts the engine socket needs `label=disable`,
  including the node agent, which lacks it today.
- **Self-identification** must handle Podman's container layout, not only Docker's.
- **"No GPU" detection** must not depend on Docker's error wording.
- **Rootless networking:** the node agent keeps host networking for WebRTC. The control
  plane's published ports go through rootless port forwarding, which can hide the client
  address and applies the host firewall. Ports below 1024 need
  `net.ipv4.ip_unprivileged_port_start`.

## Follow-up issues outside RH-07

- A request filter in front of a rootful engine socket (D17).
- Moving each session's audio from PulseAudio to PipeWire (D13).
