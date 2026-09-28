#!/bin/sh
# Quasar host preparation: the one step that runs as root (RH-07, #400).
#
# It readies a machine for Quasar and changes nothing else. Run it once as root,
# before the install; run it again at any time, and it changes only what is
# missing. Every change is printed with the reason for it. Quasar itself never
# runs as root, and nothing here grants more than the devices Quasar uses.
#
#   sudo sh prepare-host.sh --mode rootless --engine podman
#
# What it does:
#   - rootless: creates the `quasar` account (the Quasar user) with its
#     subordinate UID/GID ranges, and enables lingering so its engine runs
#     without a login;
#   - rootful: creates a `quasar` group, which owns nothing else;
#   - writes udev rules that give the `quasar` group /dev/uinput, the input
#     devices Quasar itself creates (matched by name), and the GPU render nodes;
#     with --console also the display cards, sound devices and i2c buses that
#     console mode needs. No broad group such as `input` or `video` is granted;
#   - loads the kernel modules Quasar needs, now and at boot;
#   - sets the kernel settings Quasar needs; --allow-kernel-log and
#     --unprivileged-port-start N add two optional ones;
#   - on an NVIDIA host, makes sure an NVIDIA CDI specification exists;
#   - on Podman, makes the engine start Quasar's containers again at boot;
#   - with --homes DIR (and --templates DIR), creates the homes root (and the templates
#     root) owned by the Quasar user; on an SELinux Podman host, labels each for
#     containers (container_file_t), so sessions can write their homes.
#
# It writes under /etc, plus the Quasar user's own home and systemd's linger
# record under /var. It never writes under /usr, so image-based systems
# (Fedora Atomic: Silverblue, Bazzite, uCore) work. It never re-owns existing
# files, and never relaxes SELinux.
#
# QUASAR_PREP_ROOT (tests only) prefixes every path and skips the root check
# and every command that acts on the running system.
set -eu

R="${QUASAR_PREP_ROOT:-}"
MODE=""
ENGINE="auto"
QUSER="quasar"
HOMES=""
TEMPLATES=""
CONSOLE=0
KERNEL_LOG=0
PORT_START=""
DRY_RUN=0
STEP="start"

usage() {
  cat <<'EOF'
Usage: prepare-host.sh --mode rootless|rootful [options]

  --mode rootless|rootful       the engine mode Quasar will run under (required)
  --engine auto|docker|podman   the container engine (default: auto, detected)
  --user NAME                   the Quasar user (default: quasar)
  --homes DIR                   create the homes root DIR, owned by the Quasar user
  --templates DIR               the same for the templates root (Steam's prepared home)
  --console                     also grant the display, sound and i2c devices
                                console mode uses
  --allow-kernel-log            optional: let Quasar read GPU fault messages from
                                the kernel log (kernel.dmesg_restrict=0)
  --unprivileged-port-start N   optional: let unprivileged services bind ports
                                from N (only needed for ports below 1024)
  --dry-run                     print what would change, change nothing
EOF
}

die() { printf 'prepare-host: %s\n' "$*" >&2; exit 2; }

while [ $# -gt 0 ]; do
  case "$1" in
    --mode) [ $# -ge 2 ] || die "--mode needs a value"; MODE="$2"; shift 2 ;;
    --engine) [ $# -ge 2 ] || die "--engine needs a value"; ENGINE="$2"; shift 2 ;;
    --user) [ $# -ge 2 ] || die "--user needs a value"; QUSER="$2"; shift 2 ;;
    --homes) [ $# -ge 2 ] || die "--homes needs a value"; HOMES="$2"; shift 2 ;;
    --templates) [ $# -ge 2 ] || die "--templates needs a value"; TEMPLATES="$2"; shift 2 ;;
    --console) CONSOLE=1; shift ;;
    --allow-kernel-log) KERNEL_LOG=1; shift ;;
    --unprivileged-port-start) [ $# -ge 2 ] || die "--unprivileged-port-start needs a value"; PORT_START="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
done

case "$MODE" in rootless|rootful) ;; *) usage >&2; die "--mode must be rootless or rootful" ;; esac
case "$ENGINE" in auto|docker|podman) ;; *) die "--engine must be auto, docker or podman" ;; esac
case "$QUSER" in
  ''|-*|*[!a-z0-9_-]*) die "--user must be a lowercase account name" ;;
esac
if [ -n "$PORT_START" ]; then
  case "$PORT_START" in ''|*[!0-9]*) die "--unprivileged-port-start must be a port number" ;; esac
  [ "$PORT_START" -ge 1 ] && [ "$PORT_START" -le 1024 ] || die "--unprivileged-port-start must be between 1 and 1024"
fi
for pair in "homes:$HOMES" "templates:$TEMPLATES"; do
  opt="${pair%%:*}"; dir="${pair#*:}"
  [ -n "$dir" ] || continue
  case "$dir" in /*) ;; *) die "--$opt must be an absolute path" ;; esac
  case "$dir" in /|/usr|/usr/*) die "--$opt must not be / or under /usr" ;; esac
  case "$dir" in *'('*|*')'*|*'*'*|*' '*) die "--$opt must be a plain path" ;; esac
done

live() { [ -z "$R" ]; }
if live && [ "$(id -u)" != 0 ]; then
  die "run this as root (sudo sh prepare-host.sh ...). It is the only Quasar step that needs root."
fi

on_exit() {
  rc=$?
  if [ "$rc" -ne 0 ] && [ "$STEP" != "done" ]; then
    printf '\nprepare-host: the step "%s" failed (exit %s). Every earlier step is in place,\n' "$STEP" "$rc" >&2
    printf 'and nothing is half-written. Fix the cause above and run this script again:\n' >&2
    printf 'it changes only what is still missing.\n' >&2
  fi
}
trap on_exit EXIT

say() { printf '  %-8s %s\n' "$1" "$2"; }

# run CMD... : act on the running system (skipped against a test root, and in a dry run).
run() {
  if [ "$DRY_RUN" = 1 ]; then return 0; fi
  if live; then "$@"; else printf '%s\n' "$*" >> "$R/.prepare-host-commands"; fi
}

# put PATH MODE REASON: write stdin to PATH atomically when it differs.
# Returns 0 when it wrote (or would write), 1 when the file was already right, and
# exits 2 when it could not write. Callers treat anything but 0 or 1 as fatal:
# `put` runs in a pipeline subshell, where its `die` cannot end this script.
put() {
  dest="$R$1"; mode="$2"; reason="$3"
  tmp="$(mktemp "${TMPDIR:-/tmp}/quasar-prep.XXXXXX")"
  cat > "$tmp"
  if [ -f "$dest" ] && cmp -s "$tmp" "$dest"; then
    rm -f "$tmp"; say ok "$1"; return 1
  fi
  if [ "$DRY_RUN" = 1 ]; then
    rm -f "$tmp"; say "would" "write $1 — $reason"; return 0
  fi
  # Same directory, then rename: a reader never sees half a file.
  if ! { mkdir -p "$(dirname "$dest")" && cp "$tmp" "$dest.quasar-new" \
      && chmod "$mode" "$dest.quasar-new" && mv -f "$dest.quasar-new" "$dest"; }; then
    rm -f "$tmp" "$dest.quasar-new"
    die "could not write $1"
  fi
  rm -f "$tmp"
  say changed "$1 — $reason"
  return 0
}

# stand_in: true when a test root should record the effect of a command `run` skipped.
stand_in() { ! live && [ "$DRY_RUN" = 0 ]; }

have() { command -v "$1" >/dev/null 2>&1; }

# After a `put` that returned non-zero: 1 means "already right"; anything else is fatal.
unchanged() { rc=$?; [ "$rc" -eq 1 ] || exit "$rc"; }
unchanged_then_false() { rc=$?; [ "$rc" -eq 1 ] || exit "$rc"; return 1; }

# passwd_has DB NAME: the account database has NAME (getent on the live system, so
# LDAP/SSSD accounts count; the files under a test root).
passwd_has() {
  if live && { [ "$1" = passwd ] || [ "$1" = group ]; }; then getent "$1" "$2" >/dev/null 2>&1
  else grep -q "^$2:" "$R/etc/$1" 2>/dev/null; fi
}

printf 'Quasar host preparation (%s%s)\n' "$MODE" "$([ "$DRY_RUN" = 1 ] && echo ', dry run' || true)"

# ── engine ─────────────────────────────────────────────────────────────────
STEP="engine"
if [ "$ENGINE" = auto ]; then
  if have podman && ! have docker && ! have dockerd; then ENGINE=podman
  elif ! have podman && { have docker || have dockerd; }; then ENGINE=docker
  elif have podman; then ENGINE=both
  else ENGINE=none
  fi
fi
case "$ENGINE" in
  none) say note "no container engine found; install Docker or Podman, then run this again" ;;
  both) say note "Docker and Podman are both installed; preparing for both" ;;
  *) say note "engine: $ENGINE" ;;
esac

# ── the Quasar user or group ───────────────────────────────────────────────
STEP="account"
if [ "$MODE" = rootless ]; then
  if passwd_has passwd "$QUSER"; then
    say ok "account $QUSER"
  elif [ "$DRY_RUN" = 1 ]; then
    say would "create account $QUSER — a rootless install runs everything under this one unprivileged account"
  else
    # A rootful preparation made the group already: join it rather than fail.
    if passwd_has group "$QUSER"; then group_arg="-g $QUSER"; else group_arg="--user-group"; fi
    # shellcheck disable=SC2086 # two words on purpose
    run useradd --create-home $group_arg --comment "Quasar" "$QUSER"
    stand_in && printf '%s:x:1100:1100:Quasar:/home/%s:/bin/bash\n' "$QUSER" "$QUSER" >> "$R/etc/passwd"
    stand_in && [ "$group_arg" = --user-group ] && printf '%s:x:1100:\n' "$QUSER" >> "$R/etc/group"
    say changed "account $QUSER — a rootless install runs everything under this one unprivileged account"
  fi
  STEP="subordinate IDs"
  for f in subuid subgid; do
    if passwd_has "$f" "$QUSER"; then
      say ok "/etc/$f entry for $QUSER"
      continue
    fi
    # The next free range after every existing one, 65536 wide.
    next="$(awk -F: 'BEGIN{m=524288} NF>=3 {e=$2+$3; if (e>m) m=e} END{print m}' "$R/etc/$f" 2>/dev/null || echo 524288)"
    [ -n "$next" ] || next=524288
    { [ -f "$R/etc/$f" ] && cat "$R/etc/$f"; printf '%s:%s:65536\n' "$QUSER" "$next"; } \
      | put "/etc/$f" 0644 "subordinate ${f#sub} range for $QUSER: the containers' own users map into it" || unchanged
  done
  STEP="lingering"
  if [ -e "$R/var/lib/systemd/linger/$QUSER" ]; then
    say ok "lingering for $QUSER"
  else
    run loginctl enable-linger "$QUSER"
    stand_in && { mkdir -p "$R/var/lib/systemd/linger" && : > "$R/var/lib/systemd/linger/$QUSER"; }
    say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "lingering for $QUSER — its engine and Quasar keep running with nobody logged in, and start at boot"
  fi
else
  if passwd_has group "$QUSER"; then
    say ok "group $QUSER"
  elif [ "$DRY_RUN" = 1 ]; then
    say would "create group $QUSER — owns the device access Quasar's containers use, and nothing else"
  else
    run groupadd --system "$QUSER"
    stand_in && printf '%s:x:990:\n' "$QUSER" >> "$R/etc/group"
    say changed "group $QUSER — owns the device access Quasar's containers use, and nothing else"
  fi
fi

# ── the agent's runtime directory ──────────────────────────────────────────
STEP="runtime directory"
# The node agent shares /run/quasar-agent with the sessions it starts, at the same path
# on the host and in every container. /run is root's, and a rootless engine will not
# create a missing bind source, so systemd creates it for the Quasar user at every boot.
if [ "$MODE" = rootless ]; then
  printf '# Written by Quasar host preparation. The node agent'"'"'s runtime directory, shared with its sessions.\nd /run/quasar-agent 0750 %s %s -\n' "$QUSER" "$QUSER" \
    | put /etc/tmpfiles.d/quasar.conf 0644 "/run/quasar-agent for $QUSER, recreated at every boot (a rootless engine cannot create it)" || unchanged
  # Always applied (idempotent): it also corrects the owner and mode of a directory a
  # rootful install, or an engine creating a missing bind source, left behind as root's.
  run systemd-tmpfiles --create /etc/tmpfiles.d/quasar.conf
  stand_in && mkdir -p "$R/run/quasar-agent"
fi

# ── device access ──────────────────────────────────────────────────────────
STEP="device rules"
SETFACL=""
for p in /usr/bin/setfacl /bin/setfacl; do [ -x "$R$p" ] || [ -x "$p" ] && { SETFACL="$p"; break; }; done
[ -n "$SETFACL" ] || die "setfacl was not found (install the acl package). The device rules add access with it rather than changing a device's owner."
acl="ACTION!=\"remove\", ENV{DEVNAME}==\"?*\", RUN+=\"$SETFACL -m g:$QUSER:rw \$devnode\""
{
  cat <<EOF
# Written by Quasar's host preparation (deploy/prepare-host.sh). Re-run it to change this file.
# Each rule ADDS read/write for the '$QUSER' group to one kind of device. It changes no
# device's owner or mode, so every existing user of these devices keeps its access.

# Creating virtual input devices (keyboard, mouse, gamepad, touch).
KERNEL=="uinput", SUBSYSTEM=="misc", $acl
# The input devices Quasar itself creates, matched by name, and no others.
SUBSYSTEM=="input", KERNEL=="event*|js*", ATTRS{name}=="Quasar Virtual *", $acl
# GPU render nodes: hardware encode and rendering.
SUBSYSTEM=="drm", KERNEL=="renderD*", $acl
EOF
  if [ "$CONSOLE" = 1 ]; then
    cat <<EOF

# Console mode: the display cards, local sound and monitor control (DDC over i2c).
SUBSYSTEM=="drm", KERNEL=="card[0-9]*", $acl
SUBSYSTEM=="sound", KERNEL=="pcmC*|controlC*|timer", $acl
SUBSYSTEM=="i2c-dev", KERNEL=="i2c-[0-9]*", $acl
EOF
  fi
} | if put /etc/udev/rules.d/70-quasar.rules 0644 "give the $QUSER group the devices Quasar uses, and only those" || unchanged_then_false; then
  if run udevadm control --reload; then
    run udevadm trigger --subsystem-match=misc --subsystem-match=input --subsystem-match=drm --subsystem-match=sound --subsystem-match=i2c-dev
  else
    say note "udev is not running here (a container?); the rules take effect when it runs, at the latest at boot"
  fi
fi

# ── kernel modules ─────────────────────────────────────────────────────────
STEP="kernel modules"
MODULES="uinput"
[ "$CONSOLE" = 1 ] && MODULES="uinput i2c-dev"
{
  printf '# Written by Quasar host preparation. uinput: virtual input devices.\n'
  [ "$CONSOLE" = 1 ] && printf '# i2c-dev: monitor control (DDC) in console mode.\n'
  for m in $MODULES; do printf '%s\n' "$m"; done
} | put /etc/modules-load.d/quasar.conf 0644 "load $MODULES at boot" || unchanged
for m in $MODULES; do
  # Already loaded, or built in: nothing to do (and a container cannot load modules).
  mod_dir="$(printf '%s' "$m" | tr - _)"
  if [ -d "$R/sys/module/$mod_dir" ] || { live && [ -d "/sys/module/$mod_dir" ]; }; then
    say ok "kernel module $m loaded"
  else
    run modprobe "$m"
  fi
done

# ── kernel settings ────────────────────────────────────────────────────────
STEP="kernel settings"
old_sysctl="$(cat "$R/etc/sysctl.d/99-quasar.conf" 2>/dev/null || true)"
SYSCTLS="net.core.wmem_default=2097152"
[ "$KERNEL_LOG" = 1 ] && SYSCTLS="$SYSCTLS kernel.dmesg_restrict=0"
[ -n "$PORT_START" ] && SYSCTLS="$SYSCTLS net.ipv4.ip_unprivileged_port_start=$PORT_START"
{
  printf '# Written by Quasar host preparation (deploy/prepare-host.sh).\n'
  printf '# Larger default UDP send buffers, so a video burst is not dropped by the kernel.\n'
  printf 'net.core.wmem_default=2097152\n'
  if [ "$KERNEL_LOG" = 1 ]; then
    printf '# Optional (--allow-kernel-log): let Quasar read GPU fault messages from the kernel log.\n'
    printf 'kernel.dmesg_restrict=0\n'
  fi
  if [ -n "$PORT_START" ]; then
    printf '# Optional (--unprivileged-port-start): let unprivileged services bind ports from %s.\n' "$PORT_START"
    printf 'net.ipv4.ip_unprivileged_port_start=%s\n' "$PORT_START"
  fi
} | put /etc/sysctl.d/99-quasar.conf 0644 "kernel settings Quasar needs, applied at every boot$([ "$KERNEL_LOG" = 1 ] && echo ', plus kernel-log access (asked for)')$([ -n "$PORT_START" ] && echo ", plus ports from $PORT_START (asked for)")" || unchanged
# The file covers the next boot; the running kernel is checked every run, so a
# setting that failed to apply once is retried rather than taken as done.
for kv in $SYSCTLS; do
  key="${kv%%=*}"; want="${kv#*=}"
  if live; then
    have_v="$(sysctl -n "$key" 2>/dev/null || true)"
    # A larger send buffer than Quasar needs is the operator's choice, never lowered.
    if [ "$have_v" = "$want" ] || { [ "$key" = net.core.wmem_default ] && [ -n "$have_v" ] && [ "$have_v" -ge "$want" ] 2>/dev/null; }; then
      say ok "$key=${have_v} (running kernel)"
    elif [ "$DRY_RUN" = 1 ]; then
      say would "set $key=$want now (it is ${have_v:-unset})"
    elif sysctl -q -w "$key=$want" >/dev/null 2>&1; then
      say changed "$key=$want now — it was ${have_v:-unset}"
    else
      die "could not set $key=$want (it is ${have_v:-unset}). In a container, set it on the machine that runs the kernel; then run this again."
    fi
  else
    run sysctl -q -w "$key=$want"
  fi
done
for opt in kernel.dmesg_restrict net.ipv4.ip_unprivileged_port_start; do
  case " $SYSCTLS " in *" $opt="*) continue ;; esac
  if printf '%s\n' "$old_sysctl" | grep -q "^$opt="; then
    say warn "$opt was granted by an earlier run and is no longer asked for: the file no longer sets it, but the running kernel keeps it until reboot (or set it back with sysctl -w)"
  fi
done
if [ "$KERNEL_LOG" = 0 ]; then
  say note "GPU fault messages stay hidden from Quasar; --allow-kernel-log enables that optional diagnostic"
fi

# ── NVIDIA: the CDI specification and SELinux ──────────────────────────────
STEP="NVIDIA CDI specification"
if [ -e "$R/proc/driver/nvidia/version" ]; then
  drv="$(grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' "$R/proc/driver/nvidia/version" | head -n 1)"
  # A specification someone else keeps (the toolkit's refresh service writes
  # /var/run/cdi) counts only while it describes the loaded driver.
  foreign=""
  for d in /var/run/cdi /run/cdi /etc/cdi; do
    for f in "$R$d"/nvidia*.yaml "$R$d"/nvidia*.json; do
      [ -f "$f" ] || continue
      [ "${f#"$R"}" = /etc/cdi/nvidia.yaml ] && continue
      grep -q "$drv" "$f" 2>/dev/null && { foreign="${f#"$R"}"; break 2; }
    done
  done
  if [ -n "$foreign" ]; then
    say ok "NVIDIA CDI specification ($foreign, driver $drv)"
  elif have nvidia-ctk; then
    # Ours is regenerated every run, so a driver upgrade is picked up by running this
    # again; an unchanged result writes nothing.
    spec="$(mktemp "${TMPDIR:-/tmp}/quasar-cdi.XXXXXX")"
    nvidia-ctk cdi generate > "$spec" 2>/dev/null || { rm -f "$spec"; die "nvidia-ctk could not generate the NVIDIA CDI specification"; }
    put /etc/cdi/nvidia.yaml 0644 "describes the NVIDIA GPU (driver $drv) to the engine, so containers get it without extra privilege" < "$spec" || unchanged
    rm -f "$spec"
  else
    die "this is an NVIDIA host but nvidia-ctk was not found. Install the NVIDIA Container Toolkit, then run this again."
  fi
  STEP="SELinux"
  # NVIDIA's device nodes are labelled xserver_misc_device_t. Podman confines its
  # containers under SELinux, and the policy's own boolean for exactly that label is
  # what lets them open the GPU: narrower than disabling labels for the container.
  bool="$R/sys/fs/selinux/booleans/container_use_xserver_devices"
  if { [ "$ENGINE" = podman ] || [ "$ENGINE" = both ]; } && [ -f "$bool" ]; then
    case "$(cat "$bool")" in
      1*) say ok "SELinux container_use_xserver_devices on" ;;
      *)
        run setsebool -P container_use_xserver_devices on
        stand_in && printf '1 1' > "$bool"
        say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "SELinux container_use_xserver_devices on — lets confined containers open the NVIDIA device nodes (labelled xserver_misc_device_t), and nothing else; SELinux stays enforcing"
        ;;
    esac
  fi
else
  say skipped "NVIDIA CDI specification — no NVIDIA driver loaded"
fi

# ── Podman: restart at boot ────────────────────────────────────────────────
STEP="engine restart at boot"
if [ "$ENGINE" = podman ] || [ "$ENGINE" = both ]; then
  if [ "$MODE" = rootful ]; then
    link="/etc/systemd/system/default.target.wants/podman-restart.service"
    if [ -L "$R$link" ] || [ -e "$R$link" ]; then
      say ok "podman-restart.service enabled"
    else
      run systemctl enable podman-restart.service
      stand_in && { mkdir -p "$(dirname "$R$link")" && ln -s /usr/lib/systemd/system/podman-restart.service "$R$link"; }
      say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "podman-restart.service enabled — Podman has no daemon, so this is what starts Quasar's containers at boot"
    fi
  else
    home="$(awk -F: -v u="$QUSER" '$1==u {print $6}' "$R/etc/passwd" 2>/dev/null || true)"
    if [ -z "$home" ]; then
      [ "$DRY_RUN" = 1 ] || die "cannot find the home of $QUSER"
      home="/home/$QUSER"
    fi
    for unit in podman-restart.service:default.target podman.socket:sockets.target; do
      name="${unit%%:*}"; target="${unit#*:}"
      link="$home/.config/systemd/user/$target.wants/$name"
      if [ -L "$R$link" ] || [ -e "$R$link" ]; then
        say ok "$name enabled for $QUSER"
        continue
      fi
      if [ "$DRY_RUN" = 1 ]; then
        say would "enable $name for $QUSER"
        continue
      fi
      # Created as the Quasar user, in its own home: exactly what
      # `systemctl --user enable` writes, without needing its session yet.
      if live; then
        runuser -u "$QUSER" -- mkdir -p "$home/.config/systemd/user/$target.wants"
        runuser -u "$QUSER" -- ln -s "/usr/lib/systemd/user/$name" "$link"
      else
        mkdir -p "$R$home/.config/systemd/user/$target.wants"
        ln -s "/usr/lib/systemd/user/$name" "$R$link"
        printf 'runuser -u %s -- ln -s /usr/lib/systemd/user/%s %s\n' "$QUSER" "$name" "$link" >> "$R/.prepare-host-commands"
      fi
      case "$name" in
        podman-restart.service) why="Podman has no daemon, so this is what starts Quasar's containers at boot" ;;
        *) why="the engine socket the Quasar seed and recovery actor talk to" ;;
      esac
      say changed "$name enabled for $QUSER — $why"
      user_units_changed=1
    done
    if [ "${user_units_changed:-0}" = 1 ]; then
      # Lingering may already have started the user's manager: have it read the new
      # units now. If it is not running yet, it reads them when it starts.
      uid="$(awk -F: -v u="$QUSER" '$1==u {print $3}' "$R/etc/passwd" 2>/dev/null || true)"
      if ! run runuser -u "$QUSER" -- env XDG_RUNTIME_DIR="/run/user/$uid" systemctl --user daemon-reload \
        || ! run runuser -u "$QUSER" -- env XDG_RUNTIME_DIR="/run/user/$uid" systemctl --user start podman.socket; then
        say note "the user manager of $QUSER did not answer; the units take effect when it next starts (or at boot)"
      fi
    fi
  fi
fi
if [ "$MODE" = rootless ] && { [ "$ENGINE" = docker ] || [ "$ENGINE" = both ]; }; then
  say note "rootless Docker: install it for $QUSER with dockerd-rootless-setuptool.sh and enable its docker.service; lingering (above) keeps it running"
fi

# ── the homes and templates roots ──────────────────────────────────────────
# SELinux confines Podman's containers: a directory they write must carry the container
# file label. A persistent file-context rule plus restorecon labels it (and anything
# later created in it); that changes labels, never ownership, and SELinux stays enforcing.
label_root() { # label_root DIR
  if live; then
    if semanage fcontext -l -C 2>/dev/null | grep -F "$1(/.*)?" | grep -q container_file_t; then
      say ok "SELinux label on $1"; return
    fi
  elif grep -qxF "$1" "$R/.selinux-fcontext" 2>/dev/null; then
    say ok "SELinux label on $1"; return
  fi
  if [ "$DRY_RUN" = 1 ]; then say would "label $1 for containers (container_file_t)"; return; fi
  run semanage fcontext -a -t container_file_t "$1(/.*)?"
  run restorecon -R "$1"
  stand_in && printf '%s\n' "$1" >> "$R/.selinux-fcontext"
  say changed "SELinux label on $1 — sessions in confined containers can write there; labels only, nothing is re-owned"
}

data_root() { # data_root DIR WHAT
  if [ -d "$R$1" ]; then
    owner="$(stat -c %U "$R$1" 2>/dev/null || echo unknown)"
    if live && [ "$owner" != "$QUSER" ] && [ "$MODE" = rootless ]; then
      say warn "$1 exists and is owned by $owner, not $QUSER. Quasar will not re-own it: give $QUSER write access to that one directory, or choose another path"
    else
      say ok "$2 $1"
    fi
  elif [ "$MODE" = rootful ]; then
    say skipped "$2 $1 — a rootful install keeps today's ownership, which the installer sets"
  elif [ "$DRY_RUN" = 1 ]; then
    say would "create $1 owned by $QUSER"
  else
    run install -d -m 0750 -o "$QUSER" -g "$QUSER" "$1"
    stand_in && mkdir -p "$R$1"
    say changed "$2 $1 — $3"
  fi
  if { [ "$ENGINE" = podman ] || [ "$ENGINE" = both ]; } && [ -d "$R/sys/fs/selinux" ] \
      && { [ -d "$R$1" ] || [ "$DRY_RUN" = 1 ]; }; then
    label_root "$1"
  fi
}

STEP="homes root"
[ -n "$HOMES" ] && data_root "$HOMES" "homes root" "where each user's game saves and settings live"
STEP="templates root"
[ -n "$TEMPLATES" ] && data_root "$TEMPLATES" "templates root" "where prepared app homes (Steam) are kept"

STEP="done"
printf 'Host preparation is complete. Run it again at any time: it changes only what is missing.\n'
