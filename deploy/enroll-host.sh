#!/bin/sh
# deploy/enroll-host.sh — join a machine to a Quasar control plane as a GPU host (#100).
#
#   curl -fsSL [-k --pinnedpubkey 'sha256//…'] https://<control-plane>/enroll-host.sh \
#     | QUASAR_ENROLLMENT='qenr1.…' QUASAR_REF=<ref> sh
#
# Admin → Fleet → Enroll host prints that line filled in. The control plane serves
# this file itself (the SPA build copies it into web/dist), so the script is by
# construction the one that matches the running deployment. With a self-signed
# control plane, curl trusts nothing but the pinned public key (`-k` alone would
# hand anyone on the path a root shell; `--pinnedpubkey` is what makes `-k`
# safe); with a real-CA certificate neither flag appears. The agent then pins the
# certificate fingerprint INSIDE the enrollment string (#12). The token travels in
# an environment variable, never a URL: it is single-use and expires in an hour,
# which is what makes a shell-history exposure bounded.
#
# What it does, in order (it prints each step; nothing is silent):
#   1. parses the string, refuses a ws:// (cleartext) control plane;
#   2. checks the host the way node-agent/src/readiness.rs will: Docker + Compose,
#      a DRM render node, /dev/uinput, unprivileged user namespaces (incl. the
#      Ubuntu 24.04+ AppArmor knob, #76), the NVIDIA container toolkit on NVIDIA;
#   3. pins the agent image to the release this script was fetched at;
#   4. writes $QUASAR_DIR/{docker-compose.yml,.env} — the .env is 0600 and is the
#      only place the enrollment string lands;
#   4b. on an AppArmor host, writes $QUASAR_DIR/apparmor/quasar-app and loads it
#      with apparmor_parser (#76) — the agent cannot: policy needs host root;
#   5. starts the agent and the updater (re-running updates them in place;
#      never a second agent);
#   6. tails its log until it is enrolled, or names the failure.
# It never edits the firewall: media reachability is reported by the agent's own
# readiness check in Admin → Fleet, with the exact rule for this host.
#
# Inputs (environment):
#   QUASAR_ENROLLMENT   required — the string from Admin → Fleet → Enroll host
#   QUASAR_REF          the git ref the control plane was built from (vX.Y.Z tag,
#                       or a commit); it selects the matching agent image tag
#   QUASAR_AGENT_IMAGE  explicit image reference (a digest pin) — overrides QUASAR_REF
#   QUASAR_UPDATER_IMAGE  same, for the updater; it follows QUASAR_REF otherwise
#   QUASAR_DIR          install directory, default /opt/quasar-agent
#   NODE_NAME           this host's stable fleet name, default: its hostname
#   QUASAR_HOME_ROOT    managed-home root, default /var/lib/quasar/homes
#   QUASAR_RENDER_NODE  render node to use, default: the detected one
#   QUASAR_ENROLL_DRY_RUN=1   print the plan; write and start nothing
#   QUASAR_ENROLL_APPARMOR_PERSIST=1  also install the AppArmor profile into
#                       /etc/apparmor.d so it survives a reboot; default is
#                       load-now plus the copy beside the compose files
#
# Sub-commands (argv): --print-compose, --print-nvidia-overlay,
# --print-apparmor-profile, --help.
#
# The compose text below is the node-agent service from deploy/docker-compose.yml
# with the local-stack coupling removed (depends_on, CONTROL_PLANE_URL,
# ENROLLMENT_TOKEN) and QUASAR_ENROLLMENT added, plus the quasar-updater service
# verbatim. control-plane's TestEnrollHostComposeMatchesBase fails the build if
# the agent service drifts.
set -eu

IMAGE_REPO="ghcr.io/accreleus/quasar/quasar-node-agent"
LOCAL_IMAGE="quasar-node-agent:latest"
UPDATER_REPO="ghcr.io/accreleus/quasar/quasar-updater"
LOCAL_UPDATER_IMAGE="quasar-updater:latest"
DIR="${QUASAR_DIR:-/opt/quasar-agent}"
ROOT="${QUASAR_ENROLL_ROOT:-}"          # test seam: fake /proc,/sys,/dev,/etc root
TAIL_SECS="${QUASAR_ENROLL_TAIL_SECS:-90}"
DRY="${QUASAR_ENROLL_DRY_RUN:-0}"
PROJECT="quasar-agent"

# ── rendering ────────────────────────────────────────────────────────────────
# Two styles. `plain` is the log form: what goes into bug reports, CI output and
# `| tee`; `tty` adds colour, ticks and lines that rewrite themselves while a
# step runs. Picked from the terminal unless QUASAR_ENROLL_STYLE says otherwise;
# NO_COLOR and TERM=dumb force plain. Every line of information is printed in
# both — tty only changes how, never what.
STYLE="${QUASAR_ENROLL_STYLE:-}"
if [ -z "$STYLE" ]; then
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ]; then STYLE=tty; else STYLE=plain; fi
fi
case "$STYLE" in tty|plain) ;; *) STYLE=plain ;; esac
UNICODE=0
case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in *[Uu][Tt][Ff]-8*|*[Uu][Tt][Ff]8*) UNICODE=1 ;; esac
if [ "$STYLE" = tty ]; then
  C_OK="$(printf '\033[32m')"; C_WARN="$(printf '\033[33m')"; C_ERR="$(printf '\033[31m')"
  C_DIM="$(printf '\033[2m')"; C_BOLD="$(printf '\033[1m')"; C_OFF="$(printf '\033[0m')"
  CLR="$(printf '\r\033[K')"
else
  C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_BOLD=""; C_OFF=""; CLR=""
fi
if [ "$UNICODE" = 1 ]; then
  G_OK="✔"; G_FAIL="✘"; G_WARN="!"; SPIN_FRAMES="⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏"
else
  G_OK="[ok]"; G_FAIL="[!!]"; G_WARN="[!]"; SPIN_FRAMES="- \\ | /"
fi

# A spinner is a background loop rewriting one line; it MUST be stopped before
# anything else prints, so every printer below calls spin_stop first.
SPIN_PID=""
spin_start() { # spin_start <label>
  [ "$STYLE" = tty ] || return 0
  spin_stop
  (
    set +e
    # shellcheck disable=SC2086
    set -- $SPIN_FRAMES
    while :; do
      printf '%s  %s %s' "$CLR" "$1" "$SPIN_LABEL"
      f="$1"; shift; set -- "$@" "$f"
      sleep 0.1
    done
  ) &
  SPIN_PID=$!
}
spin_stop() {
  [ -n "$SPIN_PID" ] || return 0
  { kill "$SPIN_PID" && wait "$SPIN_PID"; } 2>/dev/null || true
  SPIN_PID=""
  printf '%s' "$CLR"
}
spin() { SPIN_LABEL="$1"; spin_start; }

say() { spin_stop; printf '%s\n' "$*"; }
step() {
  spin_stop
  if [ "$STYLE" = tty ]; then printf '\n%s==> %s%s\n' "$C_BOLD" "$*" "$C_OFF"; else printf '\n==> %s\n' "$*"; fi
}
# ok/warn: one fact that went well / one that needs a look. Plain prints the
# bare text; tty prefixes a coloured glyph.
ok() {
  spin_stop
  if [ "$STYLE" = tty ]; then printf '  %s%s%s %s\n' "$C_OK" "$G_OK" "$C_OFF" "$*"; else printf '  %s\n' "$*"; fi
}
warn() {
  spin_stop
  if [ "$STYLE" = tty ]; then printf '  %s%s%s %s\n' "$C_WARN" "$G_WARN" "$C_OFF" "$*"; else printf '  WARN: %s\n' "$*"; fi
}
dim() { spin_stop; if [ "$STYLE" = tty ]; then printf '  %s%s%s\n' "$C_DIM" "$*" "$C_OFF"; else printf '  %s\n' "$*"; fi; }
usage_error() { spin_stop; printf '%senroll-host: %s%s\n' "$C_ERR" "$*" "$C_OFF" >&2; exit 2; }
host_error() {
  spin_stop
  if [ "$STYLE" = tty ]; then printf '  %s%s enroll-host: %s%s\n' "$C_ERR" "$G_FAIL" "$*" "$C_OFF" >&2; else printf 'enroll-host: %s\n' "$*" >&2; fi
  exit 1
}
ENV_TMP=""
# Always exits 0: under dash a failing last command in an EXIT trap replaces
# the script's own exit status.
cleanup() { spin_stop; if [ -n "$ENV_TMP" ]; then rm -f "$ENV_TMP"; fi; return 0; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM

# ── the compose text (kept in lockstep with deploy/docker-compose.yml) ───────
compose_yaml() {
cat <<'YAML'
# Generated by deploy/enroll-host.sh — the Quasar node-agent service alone.
# Mirrors the quasar-node-agent service in deploy/docker-compose.yml; edit the
# .env beside this file, not this file.
services:
  quasar-node-agent:
    image: ${QUASAR_AGENT_IMAGE:-${QUASAR_NODE_IMAGE:-quasar-node-agent:latest}}
    entrypoint: ["/usr/local/bin/quasar-node-agent-entrypoint"]
    network_mode: host
    cap_add: [NET_ADMIN, SYSLOG]
    init: true
    environment:
      QUASAR_ENROLLMENT: ${QUASAR_ENROLLMENT:?}
      NODE_NAME: ${NODE_NAME:-quasar-node-1}
      NODE_SECRET_PATH: /var/lib/quasar-agent/node-secret
      RUST_LOG: ${RUST_LOG:-info}
      XDG_RUNTIME_DIR: /run/quasar-agent
      QUASAR_ENCODER: ${QUASAR_ENCODER:-}
      QUASAR_RENDER_NODE: ${QUASAR_RENDER_NODE:-}
      QUASAR_HOME_ROOT: ${QUASAR_HOME_ROOT:-}
      QUASAR_HOMES_GC: ${QUASAR_HOMES_GC:-}
      QUASAR_HOMES_GC_RETENTION_HOURS: ${QUASAR_HOMES_GC_RETENTION_HOURS:-}
      QUASAR_HOMES_GC_DRY_RUN: ${QUASAR_HOMES_GC_DRY_RUN:-}
      QUASAR_HOME_TEMPLATES: ${QUASAR_HOME_TEMPLATES:-}
      QUASAR_TEMPLATE_WARMUP: ${QUASAR_TEMPLATE_WARMUP:-}
      QUASAR_TEMPLATE_ROOT: ${QUASAR_TEMPLATE_ROOT:-}
      QUASAR_TEMPLATE_CLONE_MODE: ${QUASAR_TEMPLATE_CLONE_MODE:-}
      QUASAR_TEMPLATE_ALLOW_CROSSFS: ${QUASAR_TEMPLATE_ALLOW_CROSSFS:-}
      QUASAR_TEMPLATE_SETTLE_SECS: ${QUASAR_TEMPLATE_SETTLE_SECS:-}
      QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS: ${QUASAR_TEMPLATE_WARMUP_TIMEOUT_SECS:-}
      QUASAR_TEMPLATE_MIN_FREE_BYTES: ${QUASAR_TEMPLATE_MIN_FREE_BYTES:-}
      QUASAR_ZEROCOPY: ${QUASAR_ZEROCOPY:-0}
      QUASAR_LATENCY_PROBE: ${QUASAR_LATENCY_PROBE:-0}
      QUASAR_CAPTURE_H264: ${QUASAR_CAPTURE_H264:-}
      LIBVA_TRACE: ${LIBVA_TRACE:-}
      GST_DEBUG: ${GST_DEBUG:-${VULKAN_GST_DEBUG:-}}
      QUASAR_TARGET_USAGE: ${QUASAR_TARGET_USAGE:-6}
      QUASAR_QUEUE_BUFFERS: ${QUASAR_QUEUE_BUFFERS:-3}
      QUASAR_SLICES: ${QUASAR_SLICES:-8}
      QUASAR_FEC_MODE: ${QUASAR_FEC_MODE:-}
      QUASAR_FEC_PERCENTAGE: ${QUASAR_FEC_PERCENTAGE:-0}
      QUASAR_FEC_ARM_LOSS_PCT: ${QUASAR_FEC_ARM_LOSS_PCT:-}
      QUASAR_FEC_WINDOW_S: ${QUASAR_FEC_WINDOW_S:-}
      QUASAR_FEC_ARM_WINDOWS: ${QUASAR_FEC_ARM_WINDOWS:-}
      QUASAR_FEC_DISARM_WINDOWS: ${QUASAR_FEC_DISARM_WINDOWS:-}
      QUASAR_FEC_MAX_FLAPS: ${QUASAR_FEC_MAX_FLAPS:-}
      QUASAR_INTRA_REFRESH: ${QUASAR_INTRA_REFRESH:-0}
      QUASAR_INTRA_REFRESH_PERIOD: ${QUASAR_INTRA_REFRESH_PERIOD:-0}
      QUASAR_VULKAN_H264: ${QUASAR_VULKAN_H264:-}
      QUASAR_VULKAN_HEVC: ${QUASAR_VULKAN_HEVC:-}
      QUASAR_VULKAN_AV1: ${QUASAR_VULKAN_AV1:-}
      WOLF_VULKAN_RING: ${WOLF_VULKAN_RING:-}
      QUASAR_TRACE_RTP_TS: ${QUASAR_TRACE_RTP_TS:-}
      QUASAR_TRACE_RTP_MARKER: ${QUASAR_TRACE_RTP_MARKER:-}
      QUASAR_TRACE_ENC_PTS: ${QUASAR_TRACE_ENC_PTS:-}
      QUASAR_ABR: ${QUASAR_ABR:-1}
      QUASAR_ABR_MODE: ${QUASAR_ABR_MODE:-smooth}
      QUASAR_ABR_FLOOR_KBPS: ${QUASAR_ABR_FLOOR_KBPS:-}
      QUASAR_ABR_FLOOR_RATIO: ${QUASAR_ABR_FLOOR_RATIO:-0.3}
      QUASAR_ABR_EWMA_ALPHA: ${QUASAR_ABR_EWMA_ALPHA:-}
      QUASAR_ABR_DEADBAND: ${QUASAR_ABR_DEADBAND:-}
      QUASAR_ABR_MAX_UP_STEP: ${QUASAR_ABR_MAX_UP_STEP:-}
      QUASAR_ABR_MIN_INTERVAL_MS: ${QUASAR_ABR_MIN_INTERVAL_MS:-}
      QUASAR_ABR_MAX_DOWN_STEP: ${QUASAR_ABR_MAX_DOWN_STEP:-}
      QUASAR_ABR_DOWN_DWELL_MS: ${QUASAR_ABR_DOWN_DWELL_MS:-}
      QUASAR_ABR_CLIFF_GUARD_FRAC: ${QUASAR_ABR_CLIFF_GUARD_FRAC:-}
      MALLOC_ARENA_MAX: ${MALLOC_ARENA_MAX:-}
      MALLOC_TRIM_THRESHOLD_: ${MALLOC_TRIM_THRESHOLD_:-}
      MALLOC_MMAP_THRESHOLD_: ${MALLOC_MMAP_THRESHOLD_:-}
      QUASAR_MALLOC_TRIM: ${QUASAR_MALLOC_TRIM:-}
      QUASAR_AUDIO_DISABLED: ${QUASAR_AUDIO_DISABLED:-}
      QUASAR_AUDIO_NO_CLOCK: ${QUASAR_AUDIO_NO_CLOCK:-}
      QUASAR_AUDIO_REQUIRED: ${QUASAR_AUDIO_REQUIRED:-}
      QUASAR_INPUT_TRACE: ${QUASAR_INPUT_TRACE:-}
      QUASAR_INPUT_CHANNEL_MODE: ${QUASAR_INPUT_CHANNEL_MODE:-}
      QUASAR_INPUT_BATCH_MS: ${QUASAR_INPUT_BATCH_MS:-}
      QUASAR_INPUT_CONTROLLER_NUDGE: ${QUASAR_INPUT_CONTROLLER_NUDGE:-}
      LIBGL_ALWAYS_SOFTWARE: ${LIBGL_ALWAYS_SOFTWARE-}
      MESA_LOADER_DRIVER_OVERRIDE: ${MESA_LOADER_DRIVER_OVERRIDE-}
      QUASAR_PULSE_IMAGE: ${QUASAR_PULSE_IMAGE:-${QUASAR_AGENT_IMAGE:-${QUASAR_NODE_IMAGE:-quasar-node-agent:latest}}}
      QUASAR_APP_SHM_SIZE: ${QUASAR_APP_SHM_SIZE:-1g}
      QUASAR_APP_STOP_TIMEOUT_SECS: ${QUASAR_APP_STOP_TIMEOUT_SECS:-10}
      QUASAR_CONTAINER_NETWORK: ${QUASAR_CONTAINER_NETWORK:-none}
      QUASAR_APP_PUID: ${QUASAR_APP_PUID:-}
      QUASAR_APP_PGID: ${QUASAR_APP_PGID:-}
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /run/quasar-agent:/run/quasar-agent
      - /dev/input:/dev/input
      - ${QUASAR_HOME_ROOT:-/tmp/quasar-homes-unset}:${QUASAR_HOME_ROOT:-/tmp/quasar-homes-unset}
      - ${QUASAR_TEMPLATE_ROOT:-/var/lib/quasar/templates}:${QUASAR_TEMPLATE_ROOT:-/var/lib/quasar/templates}
      - /etc/os-release:/host/etc/os-release:ro
      - /dev:/host/dev:ro
      - /sys/kernel/security:/host/sys/kernel/security:ro
      - quasar-agent-data:/var/lib/quasar-agent
      - quasar-updater-run:/run/quasar-updater
    devices:
      - /dev/dri
      - /dev/uinput
      - /dev/kmsg:/dev/kmsg:r
    device_cgroup_rules:
      - 'c 13:* rmw'
    restart: unless-stopped
  quasar-updater:
    image: ${QUASAR_UPDATER_IMAGE:-quasar-updater:latest}
    environment:
      QUASAR_UPDATER_ALLOWED_NAMESPACES: ${QUASAR_UPDATER_ALLOWED_NAMESPACES:-}
      QUASAR_UPDATER_WAIT_TIMEOUT_S: ${QUASAR_UPDATER_WAIT_TIMEOUT_S:-}
    volumes:
      - ${QUASAR_DOCKER_SOCKET:-/var/run/docker.sock}:/var/run/docker.sock
      - ${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}:${QUASAR_STACK_DIR:-/var/lib/quasar/stack-dir-unset}
      - quasar-updater-run:/run/quasar-updater
    security_opt: ["label=disable"]
    restart: unless-stopped
volumes:
  quasar-agent-data:
  quasar-updater-run:
YAML
}

# The NVIDIA overlay, verbatim from deploy/docker-compose.nvidia.yml (comments
# stripped) — same drift test.
nvidia_yaml() {
cat <<'YAML'
# Generated by deploy/enroll-host.sh — NVIDIA overlay, from deploy/docker-compose.nvidia.yml.
services:
  quasar-node-agent:
    image: ${QUASAR_AGENT_IMAGE:-${QUASAR_NODE_IMAGE:-quasar-node-agent:latest}}
    gpus: all
    environment:
      NVIDIA_DRIVER_CAPABILITIES: all
      QUASAR_PULSE_IMAGE: ${QUASAR_PULSE_IMAGE:-${QUASAR_AGENT_IMAGE:-${QUASAR_NODE_IMAGE:-quasar-node-agent:latest}}}
      QUASAR_GPU_NVIDIA: "1"
      QUASAR_CUDA_DEVICE: ${QUASAR_CUDA_DEVICE:-0}
      QUASAR_RENDER_NODE: ${QUASAR_RENDER_NODE:-/dev/dri/renderD128}
      QUASAR_NVIDIA_DRIVER_VOLUME: ${QUASAR_NVIDIA_DRIVER_VOLUME:-1}
      QUASAR_CUDA_RUNTIME: ${QUASAR_CUDA_RUNTIME:-1}
      LD_LIBRARY_PATH: /opt/quasar/nvidia-driver/lib64:/opt/quasar/nvidia-driver/cuda/lib64${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}
      LIBVA_MESSAGING_LEVEL: "0"
    volumes:
      - quasar-nvidia-driver:/opt/quasar/nvidia-driver
volumes:
  quasar-nvidia-driver:
YAML
}

# The AppArmor profile for APP containers, verbatim from deploy/apparmor/quasar-app
# (deploy/test-enroll-host.sh fails if the two drift). Written and loaded only on a
# host that enforces AppArmor; see the load step below.
apparmor_profile() {
cat <<'PROFILE'
# quasar-app — AppArmor profile for Quasar app containers (#76).
#
# Load it on the host, as root:
#     sudo apparmor_parser -r -W deploy/apparmor/quasar-app
# The node agent then picks it up by itself. It never loads this: loading policy needs
# root on the host, and the agent has neither that nor any business with it.
# deploy/enroll-host.sh writes and loads this file on an AppArmor host.
#
# Why it exists. Docker confines every container with `docker-default`, which contains
# `deny mount,`. Steam's pressure-vessel and Flatpak's bwrap set up their sandbox by
# creating a user namespace and mounting inside it, so under docker-default `unshare -U
# true` succeeds while `unshare -Urm true` dies with "cannot change root filesystem
# propagation: Permission denied", and Steam reports "Steam now requires user namespaces
# to be enabled". The agent's first answer was `--security-opt apparmor=unconfined`, which
# drops every protection docker-default carries, not just the mount deny. This profile is
# docker-default with the mount family allowed and the escape routes that opens closed
# again.
#
# Everything outside the marked delta below is docker-default verbatim, from
# github.com/moby/profiles/apparmor `baseTemplate` (read 2026-09-04), including the
# `abi <abi/3.0>` pin. That pin is load-bearing, not decoration: under AppArmor ABI 4.0+
# `network,` no longer covers `network unix`, and an app container with AF_UNIX denied
# loses its Wayland socket and its PulseAudio socket. Upstream comment and rationale:
# https://gitlab.com/apparmor/apparmor/-/issues/561
#
# The delta:
#   + mount, umount, pivot_root, userns   the sandbox bootstrap
#   + deny mount on the kernel-control filesystems and on overlay
#   + deny on the escape files by basename, so a fresh mount cannot launder them
#
# Known limit: a process may still mount a fresh proc or sysfs somewhere and reach
# writable knobs under it that the path-based denies only cover at their usual paths. The
# named escape files are covered at any path (`/**/…`); the rest of /proc/sys and /sys is
# not. Closing that needs mount-point-scoped rules that bwrap's flag combinations do not
# survive — see the mount block.
#
# The host sysctl prereq is unchanged and still required:
# `kernel.apparmor_restrict_unprivileged_userns=0` (deploy/README.md). The `userns` rule
# below is what a host would need to keep that restriction on instead; that combination is
# not validated, and an abi-3.0 profile may not mediate userns at all, so do not rely on
# it.

abi <abi/3.0>,

#include <tunables/global>

profile quasar-app flags=(attach_disconnected,mediate_deleted) {
  #include <abstractions/base>

  network,
  # Disallow AF_ALG (Linux kernel crypto API); see https://copy.fail/
  deny network alg,
  # Disallow AF_VSOCK to prevent host/guest communication.
  deny network vsock,
  # docker-default is equally blanket here, and an AppArmor capability rule takes no
  # target, so there is no narrower form to write: `capability sys_admin` is either on or
  # off, and bwrap needs it inside its user namespace. The real bound is the kernel's —
  # the agent launches app containers with `--cap-drop ALL` plus a named user-switch
  # subset (node-agent/src/session/container.rs), and the capabilities bwrap holds inside
  # its own user namespace carry no authority over the host.
  capability,
  file,
  umount,

  # Host (privileged) processes may send signals to container processes.
  signal (receive) peer=unconfined,
  # runc may send signals to container processes (for "docker stop").
  signal (receive) peer=runc,
  # crun may send signals to container processes (for "docker stop" when used with crun).
  signal (receive) peer=crun,
  # Container processes may send signals amongst themselves.
  signal (send,receive) peer="quasar-app",

  deny @{PROC}/* w,   # deny write for all files directly in /proc (not in a subdir)
  # deny write to files not in /proc/<number>/** or /proc/sys/**
  deny @{PROC}/{[^1-9/],[^1-9/][^0-9/],[^1-9s/][^0-9y/][^0-9s/],[^1-9/][^0-9/][^0-9/][^0-9/]*}/** w,
  deny @{PROC}/sys/[^k]** w,  # deny /proc/sys except /proc/sys/k* (effectively /proc/sys/kernel)
  deny @{PROC}/sys/kernel/{?,??,[^s][^h][^m]**} w,  # deny everything except shm* in /proc/sys/kernel/
  deny @{PROC}/sysrq-trigger rwklx,
  deny @{PROC}/kcore rwklx,

  # ── delta: the sandbox bootstrap ───────────────────────────────────────────
  # Allowed as verbs rather than enumerated by flags. `mount options=(…)` matches the
  # flag set exactly, and bwrap/pressure-vessel derive each remount's flags from the
  # source mount, so any enumeration passes on one host filesystem and denies on the
  # next. Ubuntu ships the same judgement: its own /etc/apparmor.d/bwrap-userns-restrict
  # grants bare `mount, umount, pivot_root, userns` to /usr/bin/bwrap. What that opens is
  # closed below instead.
  mount,
  pivot_root,
  # AppArmor 4 (Ubuntu 24.04+) only; older parsers reject the rule and the loader in
  # deploy/enroll-host.sh retries without this one line. Keep it alone on its line.
  userns,

  # Mounting any of these is a container escape, and none is mounted by a Steam-class
  # sandbox. proc and sysfs are deliberately absent from the list — Flatpak's bwrap mounts
  # a fresh /proc, which is what the per-app `systempaths_unconfined` knob exists for.
  #
  # overlay is denied on evidence, and is the first line to relax if a live denial names
  # it: pressure-vessel builds its container from bind mounts and a tmpfs and copies the
  # runtime rather than layering it (PV_RUNTIME_FLAGS_COPY_RUNTIME), and bwrap's
  # --overlay/--tmp-overlay/--overlay-src options appear only inside the bubblewrap
  # subproject it vendors, never in a pressure-vessel call site (steam-runtime-tools, read
  # 2026-09-04). An unprivileged overlay mount is the CVE-2023-0386 vector, so absent a
  # user it stays denied.
  #
  # fusectl is /sys/fs/fuse/connections, not FUSE itself: the xdg document portal mounts
  # fstype `fuse`, which is not in this list and stays allowed.
  deny mount fstype=securityfs,
  deny mount fstype=debugfs,
  deny mount fstype=tracefs,
  deny mount fstype=cgroup,
  deny mount fstype=cgroup2,
  deny mount fstype=bpf,
  deny mount fstype=configfs,
  deny mount fstype=pstore,
  deny mount fstype=efivarfs,
  deny mount fstype=overlay,
  deny mount fstype=fusectl,
  deny mount fstype=binfmt_misc,
  deny mount fstype=hugetlbfs,

  # By basename, at any path: with `mount,` allowed, the @{PROC} and /sys denies can be
  # sidestepped by mounting proc or sysfs elsewhere and using the new path. These are the
  # escape files that matters for, and no app image ships a file so named.
  deny /**/sysrq-trigger rwklx,
  deny /**/kcore rwklx,
  deny /**/uevent_helper rwklx,
  deny /**/core_pattern rwklx,
  deny /**/release_agent rwklx,
  # ── end of the delta ───────────────────────────────────────────────────────

  deny /sys/[^f]*/** wklx,
  deny /sys/f[^s]*/** wklx,
  deny /sys/fs/[^c]*/** wklx,
  deny /sys/fs/c[^g]*/** wklx,
  deny /sys/fs/cg[^r]*/** wklx,
  deny /sys/firmware/** rwklx,
  deny /sys/devices/virtual/powercap/** rwklx,
  deny /sys/kernel/security/** rwklx,

  # allow processes within the container to trace each other,
  # provided all other LSM and yama setting allow it.
  ptrace (trace,tracedby,read,readby) peer="quasar-app",
}
PROFILE
}

usage() {
  sed -n '2,47p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || true
}

# ── sub-commands ─────────────────────────────────────────────────────────────
case "${1:-}" in
  --print-compose)        compose_yaml; exit 0 ;;
  --print-nvidia-overlay) nvidia_yaml; exit 0 ;;
  --print-apparmor-profile) apparmor_profile; exit 0 ;;
  --help|-h)              usage; exit 0 ;;
  "") ;;
  *) usage_error "unknown argument '$1' (try --help)" ;;
esac

# ── 1. the enrollment string ─────────────────────────────────────────────────
blob="${QUASAR_ENROLLMENT:-}"
[ -n "$blob" ] || usage_error "QUASAR_ENROLLMENT is not set. Mint one in Admin → Fleet → Enroll host and run the command it prints:
  curl -fsSL … https://<control-plane>/enroll-host.sh | QUASAR_ENROLLMENT='qenr1.…' sh"

case "$blob" in
  qenr1.*) ;;
  *) usage_error "QUASAR_ENROLLMENT is not an enrollment string (expected it to start with 'qenr1.')" ;;
esac
rest="${blob#qenr1.}"
case "$rest" in *.*) ;; *) usage_error "QUASAR_ENROLLMENT is not an enrollment string (too few fields)" ;; esac
fp="${rest%%.*}"; rest="${rest#*.}"
case "$rest" in *.*) ;; *) usage_error "QUASAR_ENROLLMENT is not an enrollment string (too few fields)" ;; esac
url_b64="${rest%%.*}"; token="${rest#*.}"
[ -n "$url_b64" ] || usage_error "the enrollment string carries no control-plane address"
[ -n "$token" ] || usage_error "the enrollment string carries no token"
if [ -n "$fp" ] && ! printf '%s' "$fp" | grep -Eq '^([0-9A-F]{2}:){31}[0-9A-F]{2}$'; then
  usage_error "the enrollment string's fingerprint is not an uppercase colon-separated SHA-256 — was it pasted whole?"
fi

pad=$(( (4 - ${#url_b64} % 4) % 4 ))
padding=""
while [ "$pad" -gt 0 ]; do padding="$padding="; pad=$((pad - 1)); done
url="$(printf '%s%s' "$url_b64" "$padding" | tr -- '-_' '+/' | base64 -d 2>/dev/null)" ||
  usage_error "the control-plane address inside the enrollment string does not decode — was it pasted whole?"
case "$url" in
  wss://*) ;;
  ws://*) usage_error "the enrollment string points at $url — a cleartext ws:// control plane. The enrollment token and this host's node secret would cross the network unencrypted. Mint the string from the control plane's https:// page instead." ;;
  *) usage_error "the enrollment string's control-plane address is not a wss:// URL" ;;
esac
cp_host="${url#wss://}"; cp_host="${cp_host%%/*}"

step "Control plane"
say "  address:     $url"
if [ -n "$fp" ]; then
  say "  certificate: pinned to $fp"
  say "               (compare with the fingerprint= line in the control plane's startup log)"
else
  say "  certificate: public CA — verified normally, nothing pinned"
fi
say "  token:       single-use, read from QUASAR_ENROLLMENT (not shown)"

# ── privileges ───────────────────────────────────────────────────────────────
# Never prompt from inside a pipe: every privileged command runs `sudo -n`, and a
# host whose sudo wants a password is refused up front with the way out.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || host_error "not root and no sudo: this writes $DIR and talks to the Docker daemon. Run it from a root shell (su -, then paste the command)."
  if ! sudo -n true 2>/dev/null; then
    host_error "sudo asks $(id -un) for a password on this host, and this script never prompts. Either allow passwordless sudo for this user (NOPASSWD in sudoers), or open a root shell first (sudo -i) and paste the command there."
  fi
  SUDO="sudo -n"
fi

# ── 2. host preflights (the readiness checks, before anything is written) ────
step "Host preflights"

spin "checking docker"
command -v docker >/dev/null 2>&1 || host_error "docker is not installed. Install Docker Engine first (https://docs.docker.com/engine/install/) — this script does not install it."
if [ "$DRY" != 1 ]; then
  $SUDO docker info >/dev/null 2>&1 || host_error "the Docker daemon is not reachable (is it running, and can $(id -un) use it?)"
fi
docker compose version >/dev/null 2>&1 || host_error "the Docker Compose v2 plugin is missing ('docker compose version' fails). Install docker-compose-plugin."
ok "docker + compose: ok"

distro=""
if [ -r "$ROOT/etc/os-release" ]; then
  distro="$(sed -n 's/^ID=//p' "$ROOT/etc/os-release" | tr -d '"')"
fi

# GPU: sysfs is the kernel's own view; the same source readiness.rs reads.
gpu=""; render_node=""
for vendor_file in "$ROOT"/sys/class/drm/renderD*/device/vendor; do
  [ -r "$vendor_file" ] || continue
  vendor="$(tr -d '[:space:]' < "$vendor_file")"
  node_dir="${vendor_file%/device/vendor}"
  candidate="/dev/dri/${node_dir##*/}"
  case "$vendor" in
    0x10de) gpu=nvidia; render_node="$candidate"; break ;;
    0x1002) [ -z "$gpu" ] && { gpu=amd; render_node="$candidate"; } ;;
    0x8086) [ -z "$gpu" ] && { gpu=intel; render_node="$candidate"; } ;;
  esac
done
[ -n "$gpu" ] || host_error "no DRM render node with a known GPU vendor under /sys/class/drm (renderD*). The agent needs a GPU with a loaded kernel driver; check 'ls /dev/dri' and the driver install."
ok "gpu: $gpu ($render_node)"

if [ "$gpu" = nvidia ]; then
  if ! command -v nvidia-ctk >/dev/null 2>&1 && ! command -v nvidia-container-cli >/dev/null 2>&1; then
    host_error "NVIDIA GPU, but the NVIDIA Container Toolkit is not installed (no nvidia-ctk / nvidia-container-cli). The agent's compose uses 'gpus: all'. Install it: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html"
  fi
  if [ ! -e "$ROOT/etc/cdi/nvidia.yaml" ] && [ ! -e "$ROOT/var/run/cdi/nvidia.yaml" ]; then
    warn "no CDI spec at /etc/cdi/nvidia.yaml — if the agent's readiness reports a CDI problem, generate one:"
    say "        sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
  fi
  ok "nvidia container toolkit: ok"
fi

[ -e "$ROOT/dev/uinput" ] || host_error "/dev/uinput is missing — virtual keyboard, mouse and gamepad injection cannot work. Load the module:
  sudo modprobe uinput && echo uinput | sudo tee /etc/modules-load.d/uinput.conf"
ok "/dev/uinput: ok"

# Unprivileged user namespaces, in the order readiness.rs decides them.
knob="$ROOT/proc/sys/kernel/unprivileged_userns_clone"
if [ -r "$knob" ] && [ "$(tr -d '[:space:]' < "$knob")" = 0 ]; then
  host_error "unprivileged user namespaces are disabled by kernel.unprivileged_userns_clone — sandboxed app launchers (bwrap, Steam's container runtime) cannot start. Fix, then re-run:
  sudo sysctl -w kernel.unprivileged_userns_clone=1 && echo kernel.unprivileged_userns_clone=1 | sudo tee /etc/sysctl.d/99-quasar-userns.conf"
fi
knob="$ROOT/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
if [ -r "$knob" ] && [ "$(tr -d '[:space:]' < "$knob")" = 1 ]; then
  host_error "the host restricts unprivileged user namespaces through AppArmor (kernel.apparmor_restrict_unprivileged_userns=1, the Ubuntu 24.04+ default) — Steam's container runtime cannot start and every Steam-class app would exit before producing video (#76). Fix, then re-run:
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 && echo kernel.apparmor_restrict_unprivileged_userns=0 | sudo tee /etc/sysctl.d/99-quasar-userns.conf"
fi
knob="$ROOT/proc/sys/user/max_user_namespaces"
if [ -r "$knob" ] && [ "$(tr -d '[:space:]' < "$knob")" = 0 ]; then
  case "$distro" in
    debian|ubuntu) hint="sudo sysctl -w user.max_user_namespaces=15000 kernel.unprivileged_userns_clone=1 && printf 'user.max_user_namespaces=15000\\nkernel.unprivileged_userns_clone=1\\n' | sudo tee /etc/sysctl.d/99-quasar-userns.conf" ;;
    *) hint="sudo sysctl -w user.max_user_namespaces=15000 && echo user.max_user_namespaces=15000 | sudo tee /etc/sysctl.d/99-quasar-userns.conf" ;;
  esac
  host_error "unprivileged user namespaces are disabled (user.max_user_namespaces=0) — sandboxed app launchers cannot start. Fix, then re-run:
  $hint"
fi
ok "user namespaces: ok"

# ── 3. the agent image, pinned to the ref this script came from ──────────────
step "Agent image"
# QUASAR_REF -> a published tag on one repo. Two artifacts follow the same rule,
# so it is written once: a host whose agent came from a release tag and whose
# updater came from somewhere else is a state nobody could explain.
IMAGE_FROM=""
ref_to_image() { # ref_to_image <repo> <ref> ; sets IMAGE_FROM
  case "$2" in
    "") IMAGE_FROM="no QUASAR_REF given"; printf '' ;;
    v[0-9]*.[0-9]*.[0-9]*) IMAGE_FROM="release $2"; printf '%s:%s' "$1" "${2#v}" ;;
    *)
      if printf '%s' "$2" | grep -Eq '^[0-9a-f]{40}$'; then
        IMAGE_FROM="commit $(printf '%.7s' "$2")"; printf '%s:sha-%.7s' "$1" "$2"
      else
        IMAGE_FROM="branch $2"; printf '%s:%s' "$1" "$(printf '%s' "$2" | tr '/' '-')"
      fi ;;
  esac
}

image="${QUASAR_AGENT_IMAGE:-}"
image_from="${image:+QUASAR_AGENT_IMAGE}"
if [ -z "$image" ]; then
  image="$(ref_to_image "$IMAGE_REPO" "${QUASAR_REF:-}")"
  image_from="$IMAGE_FROM"
  [ -n "$image" ] || image_from="no QUASAR_REF or QUASAR_AGENT_IMAGE given"
fi

if [ "$DRY" = 1 ]; then
  say "  would use: ${image:-$LOCAL_IMAGE (local build, if present)} ($image_from)"
else
  [ -n "$image" ] && spin "pulling $image"
  if [ -n "$image" ] && $SUDO docker pull "$image" >/dev/null 2>&1; then
    ok "pulled $image (from $image_from)"
  elif $SUDO docker image inspect "$LOCAL_IMAGE" >/dev/null 2>&1; then
    if [ -n "$image" ]; then
      ok "image: $LOCAL_IMAGE (local build; $image is not published)"
    else
      ok "image: $LOCAL_IMAGE (local build)"
    fi
    image="$LOCAL_IMAGE"
  else
    host_error "no agent image: ${image:+could not pull $image (from $image_from), and }$LOCAL_IMAGE is not on this host. For a release, re-run with QUASAR_REF=<the release tag> or QUASAR_AGENT_IMAGE=<digest reference from the release body>; for a source build, build $LOCAL_IMAGE here with deploy/build-images.sh first."
  fi
fi
[ -n "$image" ] || image="$LOCAL_IMAGE"

# The updater image. Same resolution as the agent, but a MISSING one is a warning
# and not a refusal: the agent is what enrollment exists to deliver, and a host
# without an updater simply reports updater_present=false and cannot be handed a
# platform release. Refusing the whole enrollment over it would be worse.
updater_image="${QUASAR_UPDATER_IMAGE:-}"
updater_from="${updater_image:+QUASAR_UPDATER_IMAGE}"
if [ -z "$updater_image" ]; then
  updater_image="$(ref_to_image "$UPDATER_REPO" "${QUASAR_REF:-}")"
  updater_from="$IMAGE_FROM"
fi
if [ "$DRY" = 1 ]; then
  say "  would use: ${updater_image:-$LOCAL_UPDATER_IMAGE (local build, if present)} (updater)"
elif [ -n "$updater_image" ] && $SUDO docker pull "$updater_image" >/dev/null 2>&1; then
  ok "pulled $updater_image (updater, from $updater_from)"
elif $SUDO docker image inspect "$LOCAL_UPDATER_IMAGE" >/dev/null 2>&1; then
  updater_image="$LOCAL_UPDATER_IMAGE"
  ok "updater image: $LOCAL_UPDATER_IMAGE (local build)"
else
  updater_image=""
  warn "no updater image on this host, so this host cannot be handed a platform release from the console (its agent will report updater_present=false). Everything else works. To add it later: QUASAR_UPDATER_IMAGE=<reference> in $DIR/.env, then 'docker compose --project-directory $DIR up -d quasar-updater'."
fi

# ── 4. files ─────────────────────────────────────────────────────────────────
node_name="${NODE_NAME:-$(hostname -s 2>/dev/null || hostname)}"
home_root="${QUASAR_HOME_ROOT:-/var/lib/quasar/homes}"
render_node="${QUASAR_RENDER_NODE:-$render_node}"
compose_files="docker-compose.yml"
[ "$gpu" = nvidia ] && compose_files="docker-compose.yml:docker-compose.nvidia.yml"

step "Install"
say "  directory:   $DIR"
say "  node name:   $node_name"
say "  home root:   $home_root"
say "  render node: $render_node"
say "  compose:     $compose_files"

if [ "$DRY" = 1 ]; then
  say ""
  say "dry run: nothing written, nothing started."
  exit 0
fi

$SUDO install -d -m 0755 "$DIR" "$home_root"
compose_yaml | $SUDO tee "$DIR/docker-compose.yml" >/dev/null
if [ "$gpu" = nvidia ]; then
  nvidia_yaml | $SUDO tee "$DIR/docker-compose.nvidia.yml" >/dev/null
else
  $SUDO rm -f "$DIR/docker-compose.nvidia.yml"
fi

# The .env is the ONLY place the enrollment string lands, and it is written by
# this shell (printf is a builtin: nothing below puts the token in an argv).
# Re-runs keep every operator-added line and replace only the managed keys.
managed='^(QUASAR_ENROLLMENT|NODE_NAME|QUASAR_AGENT_IMAGE|QUASAR_UPDATER_IMAGE|QUASAR_STACK_DIR|QUASAR_HOME_ROOT|QUASAR_RENDER_NODE|COMPOSE_FILE|COMPOSE_PROJECT_NAME)='
umask 077
env_tmp="$(mktemp)"
ENV_TMP="$env_tmp"
if $SUDO test -f "$DIR/.env"; then
  $SUDO cat "$DIR/.env" | grep -Ev "$managed" > "$env_tmp" || true
  dim "updating existing $DIR/.env (operator lines kept)"
else
  printf '# Written by deploy/enroll-host.sh. Add agent knobs (docs/configuration.md) below.\n' > "$env_tmp"
fi
{
  printf 'COMPOSE_PROJECT_NAME=%s\n' "$PROJECT"
  printf 'COMPOSE_FILE=%s\n' "$compose_files"
  printf 'NODE_NAME=%s\n' "$node_name"
  printf 'QUASAR_AGENT_IMAGE=%s\n' "$image"
  [ -n "$updater_image" ] && printf 'QUASAR_UPDATER_IMAGE=%s\n' "$updater_image"
  # The stack directory at its HOST path: the updater rebuilds its compose
  # invocation from its own container labels, which record host paths, and
  # refuses to serve when the same absolute path is not visible inside it.
  printf 'QUASAR_STACK_DIR=%s\n' "$DIR"
  printf 'QUASAR_HOME_ROOT=%s\n' "$home_root"
  printf 'QUASAR_RENDER_NODE=%s\n' "$render_node"
  printf 'QUASAR_ENROLLMENT=%s\n' "$blob"
} >> "$env_tmp"
$SUDO install -m 0600 "$env_tmp" "$DIR/.env"
rm -f "$env_tmp"; ENV_TMP=""
if [ "$gpu" = nvidia ]; then
  ok "wrote $DIR/docker-compose.yml + docker-compose.nvidia.yml and .env (0600)"
else
  ok "wrote $DIR/docker-compose.yml and .env (0600)"
fi

# ── 4b. the app-container AppArmor profile (AppArmor hosts only) ─────────────
# Without `quasar-app` loaded the agent falls back to launching app containers
# `apparmor=unconfined` (#76) — sessions work, confinement does not. Loading policy
# needs root ON THE HOST, which is why it happens here and never in the agent.
# The file lives beside the compose by default; QUASAR_ENROLL_APPARMOR_PERSIST=1
# also installs it in /etc/apparmor.d so it survives a reboot (an unloaded profile
# is a warning on the host's readiness card, never a broken session, so persisting
# is the operator's call, not ours).
knob="$ROOT/sys/module/apparmor/parameters/enabled"
if [ -r "$knob" ] && [ "$(tr -d '[:space:]' < "$knob")" = Y ]; then
  $SUDO install -d -m 0755 "$DIR/apparmor"
  apparmor_profile | $SUDO tee "$DIR/apparmor/quasar-app" >/dev/null
  ok "wrote $DIR/apparmor/quasar-app (app-container profile)"
  if [ "${QUASAR_ENROLL_APPARMOR_PERSIST:-0}" = 1 ]; then
    $SUDO install -d -m 0755 "$ROOT/etc/apparmor.d"
    apparmor_profile | $SUDO tee "$ROOT/etc/apparmor.d/quasar-app" >/dev/null
    $SUDO chmod 0644 "$ROOT/etc/apparmor.d/quasar-app"
    ok "persisted /etc/apparmor.d/quasar-app (reloaded at every boot)"
  fi
  if command -v apparmor_parser >/dev/null 2>&1; then
    aa_err="$(mktemp)"
    if $SUDO apparmor_parser -r -W "$DIR/apparmor/quasar-app" 2>"$aa_err"; then
      ok "loaded the quasar-app AppArmor profile"
    elif grep -q userns "$aa_err" 2>/dev/null; then
      # AppArmor 3 parsers (Ubuntu 22.04, Debian 12) reject the `userns` rule, which
      # only matters on the AppArmor 4 kernels that mediate userns creation at all.
      aa_tmp="$(mktemp)"
      sed '/^[[:space:]]*userns,$/d' "$DIR/apparmor/quasar-app" > "$aa_tmp"
      if $SUDO apparmor_parser -r -W "$aa_tmp" 2>/dev/null; then
        ok "loaded the quasar-app AppArmor profile (without the AppArmor 4 'userns' rule)"
      else
        warn "could not load the quasar-app AppArmor profile; app containers will run apparmor-unconfined. Load it by hand:"
        say "        sudo apparmor_parser -r -W $DIR/apparmor/quasar-app"
      fi
      rm -f "$aa_tmp"
    else
      warn "could not load the quasar-app AppArmor profile; app containers will run apparmor-unconfined. Load it by hand:"
      say "        sudo apparmor_parser -r -W $DIR/apparmor/quasar-app"
    fi
    rm -f "$aa_err"
  else
    warn "apparmor_parser is not installed, so the quasar-app profile was not loaded; app containers will run apparmor-unconfined. Install the apparmor tools, then:"
    say "        sudo apparmor_parser -r -W $DIR/apparmor/quasar-app"
  fi
fi

# ── 5. start (or update) the agent, and the updater beside it ────────────────
step "Starting the node agent"
# RFC 3339 with the Z: without it docker parses the stamp in the daemon's local
# zone and `logs --since` reaches back into a previous run's lines.
started_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
compose() {
  # shellcheck disable=SC2086
  $SUDO docker compose --project-directory "$DIR" --project-name "$PROJECT" \
    $(printf '%s' "$compose_files" | tr ':' '\n' | sed "s#^#-f $DIR/#" | tr '\n' ' ') "$@"
}
# tty: Compose's own progress block is noise when it works and evidence when
# it does not — captured, shown only on failure. plain: passed through as-is.
if [ "$STYLE" = tty ]; then
  spin "starting the node agent"
  if ! up_out="$(compose up -d quasar-node-agent 2>&1)"; then
    spin_stop
    printf '%s\n' "$up_out" >&2
    host_error "docker compose up failed (output above)"
  fi
else
  compose up -d quasar-node-agent
fi
ok "started (compose project '$PROJECT'; re-running this script updates it in place)"

# The updater, only when an image was obtained. Never fatal and never --wait: it
# declares no healthcheck, and a host with a live agent and no updater is a
# working host that cannot be handed a release.
if [ -n "$updater_image" ]; then
  if compose up -d quasar-updater >/dev/null 2>&1; then
    ok "updater started ($updater_image)"
  else
    warn "the updater did not start; this host cannot be handed a platform release until it does. 'docker compose --project-directory $DIR logs quasar-updater' says why."
  fi
fi

summary_paths() {
  [ "$STYLE" = tty ] || return 0
  dim "files:  $DIR/docker-compose.yml, $DIR/.env"
  dim "logs:   docker compose --project-directory $DIR logs -f quasar-node-agent"
  dim "update: re-run this command; it never adds a second agent"
}
summary() {
  summary_paths
  say ""
  say "Firewall was not touched. If sessions launch but video never arrives, the"
  say "host's readiness in Admin → Fleet names the UDP range and the exact rule."
}

# ── 6. wait for enrollment ───────────────────────────────────────────────────
step "Waiting for enrollment (up to ${TAIL_SECS}s)"
elapsed=0
verdict=""
tick=0
# tty: the wait is one line that keeps moving — a frame every half second, the
# log read every two. plain: the log read every two seconds, nothing printed.
wait_beat() {
  if [ "$STYLE" = tty ]; then
    for _ in 1 2 3 4; do
      # shellcheck disable=SC2086
      set -- $SPIN_FRAMES
      eval "frame=\${$((tick % 10 + 1))}"
      printf '%s  %s waiting for the agent to enroll… %ss' "$CLR" "$frame" "$elapsed"
      tick=$((tick + 1))
      sleep 0.5
    done
  else
    sleep 2
  fi
  elapsed=$((elapsed + 2))
}
while :; do
  logs="$(compose logs --no-log-prefix --since "$started_at" quasar-node-agent 2>/dev/null || true)"
  if printf '%s' "$logs" | grep -q 'enrolled as host'; then
    verdict=enrolled; break
  elif printf '%s' "$logs" | grep -q 'reconnected as host'; then
    verdict=reconnected; break
  elif printf '%s' "$logs" | grep -q 'auth_failed'; then
    verdict=auth_failed; break
  elif printf '%s' "$logs" | grep -q 'cp-tls-pin-mismatch'; then
    verdict=pin_mismatch; break
  elif printf '%s' "$logs" | grep -q 'boot-enrollment-unconfigured'; then
    verdict=unconfigured; break
  elif printf '%s' "$logs" | grep -q 'CONTROL_PLANE_URL is required'; then
    # An agent that never learned QUASAR_ENROLLMENT: the local image predates #12.
    verdict=stale_image; break
  fi
  state="$(compose ps --format '{{.State}}' quasar-node-agent 2>/dev/null || true)"
  case "$state" in exited*|dead*) verdict=exited; break ;; esac
  [ "$elapsed" -lt "$TAIL_SECS" ] || { verdict=timeout; break; }
  wait_beat
done
[ "$STYLE" = tty ] && printf '%s' "$CLR"

case "$verdict" in
  enrolled)
    ok "enrolled: this host is now '$node_name' in Admin → Fleet."
    summary
    exit 0 ;;
  reconnected)
    ok "already enrolled: the agent reconnected as '$node_name' with its saved node secret."
    summary_paths
    exit 0 ;;
  auth_failed)
    host_error "the control plane refused the credential (auth_failed). The token was expired, already used, minted for a different node name, or '$node_name' already has a live agent. Mint a fresh string in Admin → Fleet → Enroll host and re-run." ;;
  pin_mismatch)
    host_error "the certificate the control plane presented does not match the pin in the enrollment string (cp-tls-pin-mismatch). Mint a fresh string from the control plane's own page, or set CONTROL_PLANE_FINGERPRINT in $DIR/.env to the fingerprint= line from its startup log." ;;
  unconfigured)
    host_error "the agent started without an enrollment (boot-enrollment-unconfigured) — $DIR/.env did not reach it. Check 'docker compose --project-directory $DIR config'." ;;
  stale_image)
    host_error "the agent image on this host ($image) predates the enrollment string: it asks for CONTROL_PLANE_URL instead of reading QUASAR_ENROLLMENT. Give it a current image — QUASAR_AGENT_IMAGE=<published reference>, or build/copy quasar-node-agent:latest from the control plane's tree — then re-run this command." ;;
  exited)
    host_error "the agent container exited. Its log: docker compose --project-directory $DIR logs quasar-node-agent" ;;
  *)
    printf 'enroll-host: not enrolled after %ss — still connecting. Watch it with:\n  docker compose --project-directory %s logs -f quasar-node-agent\n' "$TAIL_SECS" "$DIR" >&2
    exit 3 ;;
esac
