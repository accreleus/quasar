#!/bin/sh
# deploy/enroll-host.sh — add this machine to a Quasar control plane as a GPU host.
# What it does and its inputs: help_text below, which `--help` prints (piped too).
set -eu

help_text() {
cat <<'HELP'
deploy/enroll-host.sh — add this machine to a Quasar control plane as a GPU host.

  curl -fsSL [-k --pinnedpubkey 'sha256//…'] https://<control-plane>/enroll-host.sh \
    | QUASAR_ENROLLMENT='qenr1.…' [QUASAR_NODE_NAME=<name>] sh

Admin → Fleet → Add host prints that line filled in. The control plane serves this
file with the two images it installs written in below (QUASAR_ENROLL_SEED_IMAGE and
QUASAR_ENROLL_AGENT_IMAGE on the control plane), the same two its Dockge / Arcane
stack names. With a self-signed control plane, curl trusts nothing but the pinned
public key (`-k` alone would hand anyone on the path a root shell; `--pinnedpubkey`
is what makes `-k` safe). The two go TOGETHER: `--pinnedpubkey` on its own still
fails a self-signed certificate with `SSL certificate problem: self-signed
certificate (18)`, because curl validates the chain before it looks at the pin.
With a real-CA certificate neither appears. The agent then pins the certificate
fingerprint carried INSIDE the enrollment string. The string travels in an
environment variable, never a URL or an argv: it is single-use and expires, which
is what makes a shell-history exposure bounded.

What it does, in order (it prints each step; nothing is silent):
  1. parses the string, refuses a ws:// (cleartext) control plane;
  2. checks the host the way the agent's readiness will: Docker, a DRM render node,
     /dev/uinput, unprivileged user namespaces (incl. the Ubuntu 24.04+ AppArmor
     knob), the NVIDIA container toolkit on NVIDIA, and loads the app-container
     AppArmor profile. A failed check stops here, before anything is started, and
     prints its fix; at a terminal it offers to apply it, QUASAR_ENROLL_FIX=1
     applies it without asking;
  3. pulls the seed and node-agent images, by digest;
  4. starts the seed (container quasar-seed): it creates the recovery actor, which
     creates the node agent with the enrollment string, node name and home root;
  5. waits until the agent has enrolled, or names the failure. A refused string
     leaves nothing behind on a machine this run installed.
It writes no compose file, no .env and no install directory. On a machine that is
already installed it starts nothing and changes nothing; it reports the agent. It
never edits the firewall.

Inputs (environment):
  QUASAR_ENROLLMENT     required: the string from Admin → Fleet → Add host
  QUASAR_NODE_NAME      this host's fleet name (the command carries it when it is
                        bound to one), default: the machine's hostname
  QUASAR_HOME_ROOT      managed-home root, default /var/lib/quasar/homes
  QUASAR_TEMPLATE_ROOT  home-template root, default `templates` beside the home root
  QUASAR_SEED_IMAGE     the seed image, repository@sha256:… (default: served below)
  QUASAR_AGENT_IMAGE    the node-agent image, repository@sha256:… (default: served)
  QUASAR_ENROLL_FIX=1   apply a failed check's fix without asking; =0 never ask
  QUASAR_ENROLL_APPARMOR_PERSIST=1  also install the AppArmor profile in
                        /etc/apparmor.d so it survives a reboot (QUASAR_ENROLL_FIX=1
                        does too)
  QUASAR_ENROLL_DRY_RUN=1   check the host and print the plan and each fix; apply,
                        pull and start nothing
  QUASAR_RESET_IDENTITY=1   first remove this machine's GPU-host install (the seed,
                        the recovery actor, the node agent and their volumes, the
                        agent's saved identity included; homes are kept), so it
                        enrolls from scratch. For a machine whose install keeps an
                        enrollment string that no longer works.

Sub-commands (argv): --fix, --fix-only, --reset-identity, --print-apparmor-profile,
--help.
  `… | sh -s -- --fix` is QUASAR_ENROLL_FIX=1.
  --fix-only (or QUASAR_ENROLL_FIX_ONLY=1) prepares the host and stops: every check,
  each failed one's fix applied, the AppArmor profile loaded and persisted. It needs no
  enrollment string and pulls and starts nothing: for a host that takes the Dockge /
  Arcane stack instead of this command.
HELP
}

# Written by the control plane that serves this script (internal/enrollscript), each
# line replaced whole; empty in the repository. testdata/enroll-host/pins.json pins it.
PINNED_SEED_IMAGE=''
PINNED_AGENT_IMAGE=''

ROOT="${QUASAR_ENROLL_ROOT:-}"          # test seam: fake /proc,/sys,/dev,/etc root
TAIL_SECS="${QUASAR_ENROLL_TAIL_SECS:-180}"
DRY="${QUASAR_ENROLL_DRY_RUN:-0}"
FIX="${QUASAR_ENROLL_FIX:-}"
RESET_IDENTITY="${QUASAR_RESET_IDENTITY:-0}"
FIX_ONLY="${QUASAR_ENROLL_FIX_ONLY:-0}"
PERSIST_AA="${QUASAR_ENROLL_APPARMOR_PERSIST:-0}"

# Fixed by the recovery actor's profile and recipes (quasar_runtime::owned_install,
# quasar-recovery recipe names); the seed's are docs/configuration.md "Seed".
SEED=quasar-seed
ACTOR=quasar-recovery
AGENT=quasar-node-agent
MACHINE_VOLUME=quasar-machine
# The agent socket's volume. The seed's actor profile mounts it, so the engine creates
# it unlabelled before the actor can label anything.
SOCKET_VOLUME=quasar-recovery-agent
AGENT_HEALTH=http://127.0.0.1:9091/health

# ── rendering ────────────────────────────────────────────────────────────────
# Two styles. `plain` is the log form: what goes into bug reports, CI output and
# `| tee`; `tty` adds colour, ticks and lines that rewrite themselves while a
# step runs. Picked from the terminal unless QUASAR_ENROLL_STYLE says otherwise;
# NO_COLOR and TERM=dumb force plain. Every line of information is printed in
# both — tty only changes how, never what.
STYLE="${QUASAR_ENROLL_STYLE:-}"
if [ -z "$STYLE" ]; then
  if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != dumb ]; then STYLE="tty"; else STYLE="plain"; fi
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
  G_OK="✔"; G_FAIL="✘"; G_WARN="!"
else
  G_OK="[ok]"; G_FAIL="[!!]"; G_WARN="[!]"
fi

# A spinner is a background loop rewriting one line; it MUST be stopped before
# anything else prints, so every printer below calls spin_stop first.
SPIN_PID=""
spin_start() {
  [ "$STYLE" = tty ] || return 0
  spin_stop
  (
    set +e
    if [ "$UNICODE" = 1 ]; then
      set -- ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏
    else
      set -- '-' '\' '|' '/'
    fi
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
TMP_FILES=""
# Always exits 0: under dash a failing last command in an EXIT trap replaces
# the script's own exit status.
cleanup() {
  spin_stop
  for f in $TMP_FILES; do rm -f "$f"; done
  return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT TERM
# new_tmp: a private temp file in $NEW_TMP, removed on exit. Never call it in a
# command substitution: the subshell would lose TMP_FILES and leak the file.
new_tmp() {
  NEW_TMP="$(mktemp)"; chmod 600 "$NEW_TMP"; TMP_FILES="$TMP_FILES $NEW_TMP"
}

# The app-container AppArmor profile, verbatim from deploy/apparmor/quasar-app
# (deploy/test-enroll-host.sh fails if the two drift). Loaded only on a host that
# enforces AppArmor; see the host checks below.
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


# ── sub-commands ─────────────────────────────────────────────────────────────
for arg in "$@"; do
  case "$arg" in
    --print-apparmor-profile) apparmor_profile; exit 0 ;;
    --help|-h)                help_text; exit 0 ;;
    --reset-identity)         RESET_IDENTITY=1 ;;
    --fix)                    FIX=1 ;;
    --fix-only)               FIX_ONLY=1 ;;
    *) usage_error "unknown argument '$arg' (try --help)" ;;
  esac
done

# read_install_inputs: the string, name, roots and images an install needs.
read_install_inputs() {
# ── 1. the enrollment string ─────────────────────────────────────────────────
blob="${QUASAR_ENROLLMENT:-}"
[ -n "$blob" ] || usage_error "QUASAR_ENROLLMENT is not set. Create a command in Admin → Fleet → Add host and run it as printed:
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
# The seed hands the string to the agent verbatim; nothing else may ride in it.
printf '%s' "$blob" | grep -Eq '^qenr1\.[0-9A-F:]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9._~-]+$' ||
  usage_error "QUASAR_ENROLLMENT carries characters an enrollment string never has — was it pasted whole?"

pad=$(( (4 - ${#url_b64} % 4) % 4 ))
padding=""
while [ "$pad" -gt 0 ]; do padding="$padding="; pad=$((pad - 1)); done
url="$(printf '%s%s' "$url_b64" "$padding" | tr -- '-_' '+/' | base64 -d 2>/dev/null)" ||
  usage_error "the control-plane address inside the enrollment string does not decode — was it pasted whole?"
case "$url" in
  wss://*) ;;
  ws://*) usage_error "the enrollment string points at $url — a cleartext ws:// control plane. The enrollment token and this host's node secret would cross the network unencrypted. Create the command from the control plane's https:// page instead." ;;
  *) usage_error "the enrollment string's control-plane address is not a wss:// URL" ;;
esac

# Names and paths reach the seed's environment: the same rules the recovery actor
# applies (recipe::validate), checked here so a refusal names the variable.
node_name="${QUASAR_NODE_NAME:-}"
if [ -n "$node_name" ] && ! printf '%s' "$node_name" | grep -Eq '^[A-Za-z0-9._-]{1,253}$'; then
  usage_error "QUASAR_NODE_NAME '$node_name' must be 1-253 letters, digits, '-', '_' or '.'"
fi
home_root="${QUASAR_HOME_ROOT:-/var/lib/quasar/homes}"
home_root="${home_root%/}"
template_root="${QUASAR_TEMPLATE_ROOT:-${home_root%/*}/templates}"
for p in "$home_root" "$template_root"; do
  case "$p" in
    /*) ;;
    *) usage_error "'$p' is not an absolute path (QUASAR_HOME_ROOT, QUASAR_TEMPLATE_ROOT)" ;;
  esac
  printf '%s' "$p" | grep -Eq '^/[A-Za-z0-9._/-]*$' ||
    usage_error "'$p' has characters a host path for Quasar may not (letters, digits, '.', '_', '-', '/')"
done
[ "$home_root" != "$template_root" ] || usage_error "QUASAR_HOME_ROOT and QUASAR_TEMPLATE_ROOT must be different directories"

# repository@sha256:<64 lowercase hex>, never a tag: the seed refuses anything else.
is_digest_ref() {
  printf '%s' "$1" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[0-9a-f]{64}$' || return 1
  repo="${1%%@*}"
  case "${repo##*/}" in *:*) return 1 ;; esac
  return 0
}
seed_image="${QUASAR_SEED_IMAGE:-$PINNED_SEED_IMAGE}"
agent_image="${QUASAR_AGENT_IMAGE:-$PINNED_AGENT_IMAGE}"
[ -n "$seed_image" ] && [ -n "$agent_image" ] || usage_error "this script names no seed or node-agent image to install. The control plane that served it has none configured: set QUASAR_ENROLL_SEED_IMAGE and QUASAR_ENROLL_AGENT_IMAGE there (docs/configuration.md \"Add host\"), or give QUASAR_SEED_IMAGE and QUASAR_AGENT_IMAGE here."
is_digest_ref "$seed_image" || usage_error "the seed image '$seed_image' is not pinned by digest (repository@sha256:…)"
is_digest_ref "$agent_image" || usage_error "the node-agent image '$agent_image' is not pinned by digest (repository@sha256:…)"

step "Control plane"
say "  address:     $url"
if [ -n "$fp" ]; then
  say "  certificate: pinned to $fp"
  say "               (compare with the fingerprint= line in the control plane's startup log)"
else
  say "  certificate: public CA — verified normally, nothing pinned"
fi
say "  token:       read from QUASAR_ENROLLMENT (not shown)"
}

if [ "$FIX_ONLY" = 1 ]; then
  [ "$DRY" != 1 ] || usage_error "--fix-only applies fixes and QUASAR_ENROLL_DRY_RUN=1 applies none: use one"
  FIX=1
else
  read_install_inputs
fi

# ── privileges ───────────────────────────────────────────────────────────────
# Never prompt from inside a pipe: every privileged command runs `sudo -n`, and a
# host whose sudo wants a password is refused up front with the way out.
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || host_error "not root and no sudo: this talks to the Docker daemon and prepares the host. Run it from a root shell (su -, then paste the command)."
  if ! sudo -n true 2>/dev/null; then
    host_error "sudo asks $(id -un) for a password on this host, and this script never prompts. Either allow passwordless sudo for this user (NOPASSWD in sudoers), or open a root shell first (sudo -i) and paste the command there."
  fi
  SUDO="sudo -n"
fi
dk() { $SUDO docker "$@"; }

# ── 2. host checks, before anything is started ───────────────────────────────
step "Host checks"

# Whether a fix may be offered at the terminal the operator is typing in. The
# script itself is stdin, so the question goes to /dev/tty.
can_ask() { [ "$STYLE" = tty ] && (exec </dev/tty) 2>/dev/null; }

# fix_or_stop <problem> <fix command> <still-failing test command>
# Applies the fix when asked to (QUASAR_ENROLL_FIX=1, --fix, or yes at the terminal),
# then re-runs the test; otherwise stops with the fix printed. The fix runs as root.
fix_or_stop() {
  problem="$1"; fix="$2"; still="$3"
  apply=0
  if [ "$DRY" = 1 ]; then
    apply=0
  elif [ "$FIX" = 1 ]; then
    apply=1
  elif [ "$FIX" != 0 ] && can_ask; then
    spin_stop
    printf '  %s%s%s %s\n  fix: %s\n  apply this fix now? [y/N] ' "$C_WARN" "$G_WARN" "$C_OFF" "$problem" "$fix"
    answer=""
    read -r answer </dev/tty || answer=""
    case "$answer" in y|Y|yes|YES) apply=1 ;; esac
  fi
  if [ "$apply" = 1 ]; then
    say "  applying: $fix"
    $SUDO sh -c "$fix" || host_error "the fix failed: $fix"
    if eval "$still"; then host_error "$problem The fix ran but did not take; check the host by hand."; fi
    ok "fixed"
    return 0
  fi
  if [ "$DRY" = 1 ]; then
    warn "$problem Fix: $fix"
    return 0
  fi
  host_error "$problem Fix, then re-run (or re-run with QUASAR_ENROLL_FIX=1 to apply it):
  $fix"
}

spin "checking docker"
command -v docker >/dev/null 2>&1 || host_error "docker is not installed. Install Docker Engine first (https://docs.docker.com/engine/install/) — this script does not install it."
if [ "$DRY" != 1 ]; then
  dk info >/dev/null 2>&1 || host_error "the Docker daemon is not reachable (is it running, and can $(id -un) use it?)"
fi
ok "docker: ok"

distro=""
if [ -r "$ROOT/etc/os-release" ]; then
  distro="$(sed -n 's/^ID=//p' "$ROOT/etc/os-release" | tr -d '"')"
fi

# GPU: sysfs is the kernel's own view; the same source the agent's readiness reads.
# /sys/class/drm is not namespaced, so inside a system container it also lists
# GPUs whose device node the container was never given: only a node that exists
# counts.
gpu=""; render_node=""
for vendor_file in "$ROOT"/sys/class/drm/renderD*/device/vendor; do
  [ -r "$vendor_file" ] || continue
  vendor="$(tr -d '[:space:]' < "$vendor_file")"
  node_dir="${vendor_file%/device/vendor}"
  candidate="/dev/dri/${node_dir##*/}"
  [ -e "$ROOT$candidate" ] || continue
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
    host_error "NVIDIA GPU, but the NVIDIA Container Toolkit is not installed (no nvidia-ctk / nvidia-container-cli), so the agent cannot be given the GPU. Install it, then re-run: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html"
  fi
  if [ ! -e "$ROOT/etc/cdi/nvidia.yaml" ] && [ ! -e "$ROOT/var/run/cdi/nvidia.yaml" ]; then
    warn "no CDI spec at /etc/cdi/nvidia.yaml — if the agent's readiness reports a CDI problem, generate one:"
    say "        sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml"
  fi
  ok "nvidia container toolkit: ok"
fi

if [ ! -e "$ROOT/dev/uinput" ]; then
  fix_or_stop "/dev/uinput is missing — virtual keyboard, mouse and gamepad injection cannot work." \
    "modprobe uinput && mkdir -p $ROOT/etc/modules-load.d && echo uinput > $ROOT/etc/modules-load.d/uinput.conf" \
    "[ ! -e '$ROOT/dev/uinput' ]"
else
  ok "/dev/uinput: ok"
fi

# Unprivileged user namespaces, in the order the agent's readiness decides them.
# knob_is <path> <value>
knob_is() { [ -r "$1" ] && [ "$(tr -d '[:space:]' < "$1")" = "$2" ]; }
sysctl_fix() { # sysctl_fix <key=value>...: set now, and persist in one sysctl.d file
  cmd="sysctl -w $*"
  for kv in "$@"; do
    cmd="$cmd && mkdir -p $ROOT/etc/sysctl.d && echo $kv >> $ROOT/etc/sysctl.d/99-quasar-userns.conf"
  done
  printf '%s' "$cmd"
}
knob="$ROOT/proc/sys/kernel/unprivileged_userns_clone"
if knob_is "$knob" 0; then
  fix_or_stop "unprivileged user namespaces are disabled by kernel.unprivileged_userns_clone — sandboxed app launchers (bwrap, Steam's container runtime) cannot start." \
    "$(sysctl_fix kernel.unprivileged_userns_clone=1)" "knob_is '$knob' 0"
fi
knob="$ROOT/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
if knob_is "$knob" 1; then
  fix_or_stop "the host restricts unprivileged user namespaces through AppArmor (kernel.apparmor_restrict_unprivileged_userns=1, the Ubuntu 24.04+ default) — Steam's container runtime cannot start and every Steam-class app would exit before producing video (#76)." \
    "$(sysctl_fix kernel.apparmor_restrict_unprivileged_userns=0)" "knob_is '$knob' 1"
fi
knob="$ROOT/proc/sys/user/max_user_namespaces"
if knob_is "$knob" 0; then
  case "$distro" in
    debian|ubuntu) userns_fix="$(sysctl_fix user.max_user_namespaces=15000 kernel.unprivileged_userns_clone=1)" ;;
    *) userns_fix="$(sysctl_fix user.max_user_namespaces=15000)" ;;
  esac
  fix_or_stop "unprivileged user namespaces are disabled (user.max_user_namespaces=0) — sandboxed app launchers cannot start." \
    "$userns_fix" "knob_is '$knob' 0"
fi
ok "user namespaces: ok"

# The app-container AppArmor profile (AppArmor hosts only). Without `quasar-app`
# loaded the agent falls back to launching app containers `apparmor=unconfined`
# (#76): sessions work, confinement does not, and the readiness card says so.
# Loading policy needs root ON THE HOST, which is why it happens here and never
# in the agent.
aa_load() { # aa_load <file>: load, retrying without the AppArmor 4 `userns` rule
  new_tmp; aa_err="$NEW_TMP"
  if $SUDO apparmor_parser -r -W "$1" 2>"$aa_err"; then
    return 0
  fi
  # AppArmor 3 parsers (Ubuntu 22.04, Debian 12) reject the `userns` rule, which
  # only matters on the AppArmor 4 kernels that mediate userns creation at all.
  grep -q userns "$aa_err" 2>/dev/null || return 1
  new_tmp; aa_tmp="$NEW_TMP"
  sed '/^[[:space:]]*userns,$/d' "$1" > "$aa_tmp"
  $SUDO apparmor_parser -r -W "$aa_tmp" 2>/dev/null
}
knob="$ROOT/sys/module/apparmor/parameters/enabled"
if knob_is "$knob" Y; then
  loaded=0
  if $SUDO cat "$ROOT/sys/kernel/security/apparmor/profiles" 2>/dev/null | grep -q '^quasar-app '; then
    loaded=1
  fi
  persist=0
  { [ "$PERSIST_AA" = 1 ] || [ "$FIX" = 1 ]; } && persist=1
  if [ "$DRY" = 1 ]; then
    if [ "$loaded" = 1 ]; then ok "app-container AppArmor profile: loaded"; else say "  would load the quasar-app AppArmor profile"; fi
  elif [ "$loaded" = 1 ]; then
    ok "app-container AppArmor profile: already loaded"
  elif ! command -v apparmor_parser >/dev/null 2>&1; then
    warn "apparmor_parser is not installed, so the quasar-app profile was not loaded; app containers will run apparmor-unconfined. Install the apparmor tools and re-run."
  else
    new_tmp; aa_file="$NEW_TMP"
    apparmor_profile > "$aa_file"
    if aa_load "$aa_file"; then
      ok "loaded the quasar-app AppArmor profile"
    else
      warn "could not load the quasar-app AppArmor profile; app containers will run apparmor-unconfined. Load it by hand: sh enroll-host.sh --print-apparmor-profile > quasar-app && sudo apparmor_parser -r -W quasar-app"
    fi
  fi
  if [ "$DRY" != 1 ] && [ "$persist" = 1 ]; then
    new_tmp; aa_file="$NEW_TMP"
    apparmor_profile > "$aa_file"
    $SUDO install -D -m 0644 "$aa_file" "$ROOT/etc/apparmor.d/quasar-app"
    ok "persisted /etc/apparmor.d/quasar-app (loaded again at every boot)"
  fi
fi

if [ "$FIX_ONLY" = 1 ]; then
  say ""
  ok "host prepared: every check passes. Nothing was pulled or started; add the host with the Dockge or Arcane stack."
  exit 0
fi

# ── this machine as it is now ────────────────────────────────────────────────
# names_of <docker ps filter>…: container names, one per line
names_of() { dk ps -a --format '{{.Names}}' "$@" 2>/dev/null || true; }
state_of() { dk inspect -f '{{.State.Status}}' "$1" 2>/dev/null || true; }

other_seed=""
if [ "$DRY" != 1 ]; then
  # A Compose-installed agent (the pre-RH06 enroll-host.sh stack) would hold the
  # same node name and health port: the two cannot share a machine.
  legacy="$(dk ps -a --filter label=com.docker.compose.service=quasar-node-agent \
    --format '{{.Names}}|{{.Label "io.quasar.installation"}}' 2>/dev/null | sed -n 's/|$//p' | head -n 1 || true)"
  if [ -n "$legacy" ]; then
    host_error "this machine still runs the Compose-installed node agent '$legacy'. Remove that stack first (for the earlier one-line install: docker compose --project-directory /opt/quasar-agent down; homes are kept), then re-run this command."
  fi
  other_seed="$(dk ps -a --no-trunc --format '{{.Names}}|{{.Command}}' 2>/dev/null \
    | grep 'quasar-recovery seed"*$' | cut -d'|' -f1 | grep -vx "$SEED" | head -n 1 || true)"
  # The documented stacks name their seed quasar-seed too: one a stack manager runs
  # carries Compose's project label, and is not this script's to replace or remove.
  if [ -z "$other_seed" ] && [ -n "$(dk inspect -f '{{with index .Config.Labels "com.docker.compose.project"}}{{.}}{{end}}' "$SEED" 2>/dev/null || true)" ]; then
    other_seed="$SEED"
  fi
  # Refused with a reset too: that seed would survive it and re-create the actor.
  if [ -n "$other_seed" ]; then
    host_error "this machine already has a seed, '$other_seed', started by a stack manager or by hand. Quasar is installed through that one: keep it and remove this command, or remove that seed first."
  fi
fi

volumes_of() { dk volume ls -q "$@" 2>/dev/null || true; }

# remove_install: this machine's GPU-host install, by the recovery actor's own
# `uninstall --purge` (#366): it removes only what the installation created, and its
# volumes. Homes are host paths and stay. Never on a machine holding a control plane
# or Quasar's Postgres, or their data.
remove_install() {
  for role in control-plane postgres; do
    if [ -n "$(names_of --filter "label=io.quasar.platform-service=$role")$(volumes_of --filter "label=io.quasar.platform-service=$role")" ]; then
      host_error "this machine holds a Quasar $role (a container or its volume); removing its install is not this script's job."
    fi
  done
  # The installation, and the image to uninstall it with: the actor's, else the seed's.
  inst=""; uninstall_image="$seed_image"
  for c in $(names_of --filter label=io.quasar.platform-service=recovery-actor) $(names_of --filter label=io.quasar.installation); do
    inst="$(dk inspect -f '{{index .Config.Labels "io.quasar.installation"}}' "$c" 2>/dev/null || true)"
    [ -z "$inst" ] || { uninstall_image="$(dk inspect -f '{{.Config.Image}}' "$c" 2>/dev/null || echo "$seed_image")"; break; }
  done
  # The seed first: it is this script's own, and it would re-create a removed actor.
  if [ -n "$(state_of "$SEED")" ]; then
    dk rm -f "$SEED" >/dev/null 2>&1 || host_error "could not remove the seed $SEED; remove it by hand and re-run."
  fi
  if [ -n "$inst" ]; then
    dk run --rm --security-opt label=disable \
      -v /var/run/docker.sock:/var/run/docker.sock \
      -v "$MACHINE_VOLUME:/var/lib/quasar-machine" \
      "$uninstall_image" uninstall --purge --confirm "$inst" >/dev/null ||
      host_error "the recovery actor's uninstall of installation $inst did not finish; run this command again, which continues it."
  fi
  # uninstall empties the machine-state volume it has mounted; the volume goes here.
  if dk volume inspect "$MACHINE_VOLUME" >/dev/null 2>&1; then
    dk volume rm "$MACHINE_VOLUME" >/dev/null 2>&1 || host_error "could not remove volume $MACHINE_VOLUME; something still holds it."
  fi
  # Only when no container mounts it: kept otherwise, and said so.
  LEFT=""
  if dk volume inspect "$SOCKET_VOLUME" >/dev/null 2>&1; then
    users="$(names_of --filter "volume=$SOCKET_VOLUME" | tr '\n' ' ' | sed 's/ $//')"
    if [ -n "$users" ]; then
      LEFT="the volume $SOCKET_VOLUME, which $users still mounts"
    else
      dk volume rm "$SOCKET_VOLUME" >/dev/null 2>&1 || LEFT="the volume $SOCKET_VOLUME, which could not be removed"
    fi
  fi
}

actors=""; fresh=1
if [ "$DRY" != 1 ]; then
  if [ "$RESET_IDENTITY" = 1 ]; then
    remove_install
    ok "removed this machine's GPU-host install (QUASAR_RESET_IDENTITY): it enrolls from scratch"
    [ -z "$LEFT" ] || warn "left in place: $LEFT"
  fi
  actors="$(names_of --filter label=io.quasar.platform-service=recovery-actor)"
  # Anything of an installation already here makes this run not the installer:
  # a refused string then removes nothing.
  if [ -n "$actors$(names_of --filter label=io.quasar.installation)$(volumes_of --filter label=io.quasar.installation)" ] ||
     dk volume inspect "$MACHINE_VOLUME" >/dev/null 2>&1 || dk volume inspect "$SOCKET_VOLUME" >/dev/null 2>&1; then
    fresh=0
  fi
fi
machine_name="$(dk info --format '{{.Name}}' 2>/dev/null || hostname)"
shown_name="${node_name:-$machine_name}"

if [ -n "$actors" ]; then
  # ── already installed: start nothing, change nothing, report ───────────────
  step "Already installed"
  ok "this machine has a recovery actor ($(printf '%s' "$actors" | tr '\n' ' ' | sed 's/ $//')); nothing is started or changed"
  dim "the enrollment string in this command is not used"
  if [ -z "$(state_of "$SEED")" ] && [ -z "$other_seed" ]; then
    warn "no seed on this machine, so a deleted recovery actor would not be re-created. Start the seed again the way it was first started."
  fi
else
  step "Install"
  say "  node name:     $shown_name"
  say "  home root:     $home_root"
  say "  template root: $template_root"
  say "  seed image:    $seed_image"
  say "  agent image:   $agent_image"
  if [ "$DRY" = 1 ]; then
    say ""
    say "dry run: nothing pulled, nothing started."
    exit 0
  fi

  # ── 3. images, by digest ───────────────────────────────────────────────────
  pull() { # pull <image> <what>
    if dk image inspect "$1" >/dev/null 2>&1; then ok "$2 image: present"; return 0; fi
    spin "pulling the $2 image"
    dk pull -q "$1" >/dev/null 2>&1 || host_error "could not pull $1. Check that this machine can reach its registry (a plain-HTTP registry must be listed in the Docker daemon's insecure-registries)."
    ok "pulled the $2 image"
  }
  pull "$seed_image" seed
  pull "$agent_image" "node-agent (large on first install)"

  # ── 4. the seed ────────────────────────────────────────────────────────────
  # A seed left by an earlier run that created no recovery actor did nothing
  # durable: it is replaced, so a corrected command takes effect.
  if [ -n "$(state_of "$SEED")" ]; then
    dk rm -f "$SEED" >/dev/null 2>&1 || host_error "could not replace the earlier seed '$SEED'; remove it by hand and re-run."
    dim "replaced the seed an earlier, unfinished run started"
  fi
  # The string reaches the seed through a 0600 env file, never a docker argv.
  new_tmp; env_file="$NEW_TMP"
  {
    printf 'QUASAR_ROLE=gpu\n'
    printf 'QUASAR_ENROLLMENT=%s\n' "$blob"
    printf 'QUASAR_HOME_ROOT=%s\n' "$home_root"
    printf 'QUASAR_TEMPLATE_ROOT=%s\n' "$template_root"
    printf 'QUASAR_AGENT_IMAGE=%s\n' "$agent_image"
    [ -z "$node_name" ] || printf 'QUASAR_NODE_NAME=%s\n' "$node_name"
  } > "$env_file"
  run_ok=1
  dk run -d --name "$SEED" --restart unless-stopped --security-opt label=disable \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$MACHINE_VOLUME:/var/lib/quasar-machine:ro" \
    --env-file "$env_file" "$seed_image" seed >/dev/null || run_ok=0
  # The engine has read it; the token stays on disk no longer than that.
  rm -f "$env_file"
  [ "$run_ok" = 1 ] || host_error "the seed did not start: docker run failed (output above)"
  ok "started the seed ($SEED); it creates the recovery actor, which creates the node agent"
fi

# ── 5. wait for the agent ────────────────────────────────────────────────────
# fail_install <message>: on a machine this run installed, take the install away
# again so the next command starts clean, and say so.
fail_install() {
  if [ "$fresh" = 1 ]; then
    remove_install
    if [ -z "$LEFT" ]; then
      host_error "$1 Nothing was left on this machine: create a new command in Admin → Fleet → Add host and run it."
    fi
    host_error "$1 What this run installed was removed except $LEFT; remove that, then create a new command in Admin → Fleet → Add host and run it."
  fi
  host_error "$1"
}
refused() { # refused <why>: the control plane turned the enrollment string down
  if [ "$fresh" = 1 ]; then
    fail_install "$1"
  fi
  host_error "$1 This machine keeps the string it was installed with, so create a new command in Admin → Fleet → Add host and run it with QUASAR_RESET_IDENTITY=1: that removes this machine's install (never its homes) and enrolls it afresh."
}

step "Waiting for the node agent to enroll (up to ${TAIL_SECS}s)"
elapsed=0
tick=0
verdict=""; detail=""; seen_seed=""; started_actor=0
wait_beat() {
  if [ "$STYLE" = tty ]; then
    for _ in 1 2 3 4; do
      if [ "$UNICODE" = 1 ]; then
        set -- ⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏
      else
        set -- '-' '\' '|' '/'
      fi
      frame_index=$((tick % $#))
      while [ "$frame_index" -gt 0 ]; do
        shift
        frame_index=$((frame_index - 1))
      done
      printf '%s  %s waiting for the agent to enroll… %ss' "$CLR" "$1" "$elapsed"
      tick=$((tick + 1))
      sleep 0.5
    done
  else
    sleep 2
  fi
  elapsed=$((elapsed + 2))
}
while :; do
  if [ -n "$(state_of "$AGENT")" ]; then
    agent_log="$(dk logs --tail 2000 "$AGENT" 2>&1 || true)"
    line="$(printf '%s\n' "$agent_log" | grep -E 'enrolled as host|reconnected as host|auth_failed|cp-tls-pin-mismatch|cp-register-stale-identity-unresolvable|boot-enrollment-unconfigured' | tail -n 1 || true)"
    # An agent this run installed may reconnect right after it enrolls (its policy-seed
    # reconnect): its enrollment is the verdict, not the reconnect that follows it.
    if [ -z "$actors" ]; then
      case "$line" in
        *'reconnected as host'*)
          enrolled_line="$(printf '%s\n' "$agent_log" | grep 'enrolled as host' | tail -n 1 || true)"
          [ -z "$enrolled_line" ] || line="$enrolled_line" ;;
      esac
    fi
    case "$line" in
      *'enrolled as host'*) verdict=enrolled ;;
      *'reconnected as host'*) verdict=reconnected ;;
      *'live agent is already registered'*) verdict=live ;;
      *auth_failed*) verdict=auth_failed ;;
      *cp-tls-pin-mismatch*) verdict=pin_mismatch ;;
      *cp-register-stale-identity-unresolvable*) verdict=stale_identity ;;
      *boot-enrollment-unconfigured*) verdict=unconfigured ;;
    esac
    if [ -z "$verdict" ] && command -v curl >/dev/null 2>&1 &&
       curl -fsS --max-time 2 "$AGENT_HEALTH" 2>/dev/null | grep -q '"connected":true'; then
      verdict=connected
    fi
    case "$(state_of "$AGENT")" in exited|dead) [ -n "$verdict" ] || verdict=agent_exited ;; esac
  fi
  [ -z "$verdict" ] || break

  if [ -n "$(state_of "$ACTOR")" ]; then
    detail="$(dk logs --tail 200 "$ACTOR" 2>&1 | grep -E 'actor-resume-failed|actor-lease-unavailable|actor-role-invalid' | tail -n 1 || true)"
    if [ -n "$detail" ]; then verdict=actor_failed; break; fi
    case "$(state_of "$ACTOR")" in exited|dead) verdict=actor_exited; break ;; esac
  fi

  if [ -n "$(state_of "$SEED")" ] && [ -z "$(state_of "$ACTOR")$(state_of "$AGENT")" ] || [ "$(state_of "$ACTOR")" = created ]; then
    seed_status="$(dk exec "$SEED" quasar-recovery status 2>/dev/null || true)"
    case "$seed_status" in
      *'docker start quasar-recovery'*)
        # A seed replaced by this run between its predecessor's create and start
        # leaves the actor unstarted; starting it finishes that create (ADR 0007).
        if [ "$started_actor" = 0 ]; then
          dk start "$ACTOR" >/dev/null 2>&1 && dim "started the recovery actor an earlier seed created"
          started_actor=1
        fi ;;
      *'seed: idle:'*) detail="${seed_status#seed: idle: }"; verdict=seed_idle; break ;;
      *'seed: retrying:'*)
        if [ "$seed_status" != "$seen_seed" ]; then
          dim "${seed_status%% (*}"
          seen_seed="$seed_status"
        fi ;;
    esac
    case "$(state_of "$SEED")" in exited|dead) verdict=seed_exited; break ;; esac
  fi

  [ "$elapsed" -lt "$TAIL_SECS" ] || { verdict=timeout; break; }
  wait_beat
done
[ "$STYLE" = tty ] && printf '%s' "$CLR"

summary() {
  say ""
  dim "services: docker ps --filter label=io.quasar.installation"
  dim "logs:     docker logs $AGENT (the agent), docker logs $ACTOR (the recovery actor)"
  dim "again:    running this command again changes nothing"
  say "Firewall was not touched. If sessions launch but video never arrives, the"
  say "host's readiness in Admin → Fleet names the UDP range and the exact rule."
}

case "$verdict" in
  enrolled)
    if [ -n "$actors" ]; then
      ok "already enrolled: this host is '$shown_name' in Admin → Fleet."
    else
      ok "enrolled: this host is now '$shown_name' in Admin → Fleet."
    fi
    summary
    exit 0 ;;
  reconnected|connected)
    ok "already enrolled: the node agent is connected to the control plane with its saved identity."
    summary
    exit 0 ;;
  live)
    refused "the control plane refused the enrollment: a live agent is already registered as '$shown_name'. Stop that agent, or create a command with another node name." ;;
  auth_failed)
    refused "the control plane refused the enrollment string (auth_failed): it has expired, was already used, or was created for a different node name than '$shown_name'." ;;
  pin_mismatch)
    refused "the certificate the control plane presented does not match the pin in the enrollment string (cp-tls-pin-mismatch). Create the command from the control plane's own page." ;;
  stale_identity)
    refused "this machine's node agent holds a node secret the control plane does not recognise, and no enrollment string to fall back on." ;;
  unconfigured)
    fail_install "the node agent started without an enrollment string (boot-enrollment-unconfigured). Its log: docker logs $AGENT" ;;
  agent_exited)
    fail_install "the node agent exited. Its log: docker logs $AGENT" ;;
  actor_failed)
    fail_install "the recovery actor could not install this machine: ${detail#*token=}" ;;
  actor_exited)
    fail_install "the recovery actor exited. Its log: docker logs $ACTOR" ;;
  seed_idle)
    fail_install "the seed refused to install: $detail" ;;
  seed_exited)
    fail_install "the seed exited. Its log: docker logs $SEED" ;;
  *)
    printf 'enroll-host: not enrolled after %ss — still connecting. Watch it with:\n  docker logs -f %s\nRunning this command again resumes the wait and changes nothing.\n' "$TAIL_SECS" "$AGENT" >&2
    exit 3 ;;
esac
