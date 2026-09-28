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
#   - with --homes DIR, creates the homes root owned by the Quasar user.
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
if [ -n "$HOMES" ]; then
  case "$HOMES" in /*) ;; *) die "--homes must be an absolute path" ;; esac
  case "$HOMES" in /|/usr|/usr/*) die "--homes must not be / or under /usr" ;; esac
fi

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
  mkdir -p "$(dirname "$dest")"
  # Same directory, then rename: a reader never sees half a file.
  cp "$tmp" "$dest.quasar-new"
  chmod "$mode" "$dest.quasar-new"
  mv -f "$dest.quasar-new" "$dest"
  rm -f "$tmp"
  say changed "$1 — $reason"
  return 0
}

# stand_in: true when a test root should record the effect of a command `run` skipped.
stand_in() { ! live && [ "$DRY_RUN" = 0 ]; }

have() { command -v "$1" >/dev/null 2>&1; }

passwd_has() { grep -q "^$2:" "$R/etc/$1" 2>/dev/null; }

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
    run useradd --create-home --user-group --comment "Quasar" "$QUSER"
    stand_in && printf '%s:x:1100:1100:Quasar:/home/%s:/bin/bash\n' "$QUSER" "$QUSER" >> "$R/etc/passwd"
    stand_in && printf '%s:x:1100:\n' "$QUSER" >> "$R/etc/group"
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
      | put "/etc/$f" 0644 "subordinate ${f#sub} range for $QUSER: the containers' own users map into it" || true
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

# ── device access ──────────────────────────────────────────────────────────
STEP="device rules"
SETFACL=""
for p in /usr/bin/setfacl /bin/setfacl; do [ -x "$R$p" ] || [ -x "$p" ] && { SETFACL="$p"; break; }; done
[ -n "$SETFACL" ] || die "setfacl was not found (install the acl package). The device rules add access with it rather than changing a device's owner."
acl="RUN+=\"$SETFACL -m g:$QUSER:rw \$devnode\""
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
} | if put /etc/udev/rules.d/70-quasar.rules 0644 "give the $QUSER group the devices Quasar uses, and only those"; then
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
} | put /etc/modules-load.d/quasar.conf 0644 "load $MODULES at boot" || true
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
} | put /etc/sysctl.d/99-quasar.conf 0644 "kernel settings Quasar needs, applied at every boot$([ "$KERNEL_LOG" = 1 ] && echo ', plus kernel-log access (asked for)')$([ -n "$PORT_START" ] && echo ", plus ports from $PORT_START (asked for)")" || true
# The file covers the next boot; the running kernel is checked every run, so a
# setting that failed to apply once is retried rather than taken as done.
for kv in $SYSCTLS; do
  key="${kv%%=*}"; want="${kv#*=}"
  if live; then
    have_v="$(sysctl -n "$key" 2>/dev/null || true)"
    if [ "$have_v" = "$want" ]; then
      say ok "$key=$want (running kernel)"
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
if [ "$KERNEL_LOG" = 0 ]; then
  say note "GPU fault messages stay hidden from Quasar; --allow-kernel-log enables that optional diagnostic"
fi

# ── NVIDIA: the CDI specification ──────────────────────────────────────────
STEP="NVIDIA CDI specification"
if [ -e "$R/proc/driver/nvidia/version" ]; then
  spec=""
  for d in /etc/cdi /var/run/cdi /run/cdi; do
    for f in "$R$d"/nvidia*.yaml "$R$d"/nvidia*.json; do
      [ -f "$f" ] && { spec="${f#"$R"}"; break 2; }
    done
  done
  if [ -n "$spec" ]; then
    say ok "NVIDIA CDI specification ($spec)"
  elif have nvidia-ctk; then
    run nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
    stand_in && { mkdir -p "$R/etc/cdi" && printf 'kind: nvidia.com/gpu\n' > "$R/etc/cdi/nvidia.yaml"; }
    say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "/etc/cdi/nvidia.yaml — describes the NVIDIA GPU to the engine, so containers get it without extra privilege; run this again after a driver upgrade"
  else
    die "this is an NVIDIA host but nvidia-ctk was not found. Install the NVIDIA Container Toolkit, then run this again."
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

# ── the homes root ─────────────────────────────────────────────────────────
STEP="homes root"
if [ -n "$HOMES" ]; then
  if [ -d "$R$HOMES" ]; then
    owner="$(stat -c %U "$R$HOMES" 2>/dev/null || echo unknown)"
    if live && [ "$owner" != "$QUSER" ] && [ "$MODE" = rootless ]; then
      say warn "$HOMES exists and is owned by $owner, not $QUSER. Quasar will not re-own it: give $QUSER write access to that one directory, or choose another --homes"
    else
      say ok "homes root $HOMES"
    fi
  elif [ "$MODE" = rootful ]; then
    say skipped "homes root $HOMES — a rootful install keeps today's ownership, which the installer sets"
  elif [ "$DRY_RUN" = 1 ]; then
    say would "create $HOMES owned by $QUSER"
  else
    run install -d -m 0750 -o "$QUSER" -g "$QUSER" "$HOMES"
    stand_in && mkdir -p "$R$HOMES"
    say changed "homes root $HOMES — where each user's game saves and settings live"
  fi
fi

STEP="done"
printf 'Host preparation is complete. Run it again at any time: it changes only what is missing.\n'
