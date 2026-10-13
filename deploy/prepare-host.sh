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
#   - installs Quasar's udev rules (the files in deploy/udev/, byte for byte): the
#     `quasar` group gets /dev/uinput, the input devices Quasar itself creates
#     (matched by name), and the GPU render nodes; with --console also the display
#     cards, sound devices, i2c buses and this machine's own input devices, which a
#     console session's desktop is given (it holds the screen and the raw input of
#     this machine), and console mode's virtual terminal (tty8, with no login prompt
#     on it). On a rootless engine these ACLs are the only access there is: no device
#     rule reaches a rootless container, so a device plugged in later opens through
#     them alone.
#     A seat rule keeps Quasar's virtual input devices off the desktop's seat, so a
#     desktop user logged in on the host does not receive a player's input. No broad
#     group such as `input` or `video` is granted;
#   - loads the kernel modules Quasar needs, now and at boot;
#   - sets the kernel settings Quasar needs; --allow-kernel-log and
#     --unprivileged-port-start N add two optional ones;
#   - on an NVIDIA host, makes sure an NVIDIA CDI specification exists;
#   - with --console on an SELinux host, loads a small SELinux module that lets a console
#     session's confined type open this machine's input, sound and hidraw nodes and read
#     the udev database (SELinux stays enforcing);
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
CONSOLE_AUDIO_RETIRED=0
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
  --console                     also grant the display cards, sound, i2c and this
                                machine's input devices and the terminal (tty8) that
                                console mode uses (on SELinux, also a module for them)
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
    # Retired (#461): a console desktop plays its own audio. Accepted so an existing
    # command line still runs; it changes nothing.
    --console-audio-user) [ $# -ge 2 ] || die "--console-audio-user needs a value"; CONSOLE_AUDIO_RETIRED=1; shift 2 ;;
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
  # The path becomes an SELinux file-context regex: plain characters only.
  case "$dir" in *[!A-Za-z0-9._/-]*|*//*|*/./*|*/../*|*/.|*/..) die "--$opt must be a plain path (letters, digits, . _ - /)" ;; esac
  # Labelling a system tree for containers would break the host.
  case "${dir%/}" in
    /bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/var|/var/home|/var/lib|/var/log|/var/run|/var/tmp)
      die "--$opt must be a directory of its own, not $dir" ;;
  esac
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

# SELinux confines Podman's containers: a directory they write must carry the container
# file label. A persistent file-context rule plus restorecon labels it (and anything
# later created in it); that changes labels, never ownership, and SELinux stays enforcing.
label_root() { # label_root DIR
  re="$(printf '%s\n' "$1" | awk '{ gsub(/\./, "\\."); print }')"
  # Never an existing user's home, or a directory holding one: it would be relabelled.
  if awk -F: -v d="$1" '$6 == d || index($6, d "/") == 1 {f=1} END {exit !f}' "$R/etc/passwd" 2>/dev/null; then
    die "$1 is, or holds, a user's home directory; choose a directory of its own for Quasar"
  fi
  if live; then
    for tool in semanage restorecon; do
      have "$tool" || die "$tool was not found: install policycoreutils-python-utils (on an image-based system: rpm-ostree install policycoreutils-python-utils, then reboot), then run this again"
    done
    if semanage fcontext -l -C 2>/dev/null | awk -v r="$re(/.*)?" '$1 == r && /container_file_t/ {f=1} END {exit !f}'; then
      # The rule only labels what restorecon visits: a root recreated since (a reinstall)
      # inherits its parent's type until it is relabelled.
      if [ "$(stat -c %C "$1" 2>/dev/null | cut -d: -f3)" = container_file_t ]; then
        say ok "SELinux label on $1"; return
      fi
      if [ "$DRY_RUN" = 1 ]; then say would "relabel $1 for containers (container_file_t)"; return; fi
      run restorecon -R "$1"
      say changed "SELinux label on $1 — relabelled to its container_file_t rule; nothing is re-owned"
      return
    fi
  elif grep -qxF "$1" "$R/.selinux-fcontext" 2>/dev/null; then
    say ok "SELinux label on $1"; return
  fi
  if [ "$DRY_RUN" = 1 ]; then say would "label $1 for containers (container_file_t)"; return; fi
  run semanage fcontext -a -t container_file_t "$re(/.*)?"
  run restorecon -R "$1"
  stand_in && printf '%s\n' "$1" >> "$R/.selinux-fcontext"
  say changed "SELinux label on $1 — sessions in confined containers can write there; labels only, nothing is re-owned"
}

# containers_selinux: the engine confines its containers with SELinux here, so what they
# share needs the label. Podman always does; Docker when its daemon runs --selinux-enabled
# (Fedora CoreOS and uCore ship it so).
docker_selinux() {
  { [ "$ENGINE" = docker ] || [ "$ENGINE" = both ]; } || return 1
  grep -qs -- '--selinux-enabled' "$R/usr/lib/systemd/system/docker.service" \
      "$R/etc/systemd/system/docker.service" "$R"/etc/systemd/system/docker.service.d/*.conf \
    || grep -qs '"selinux-enabled"[[:space:]]*:[[:space:]]*true' "$R/etc/docker/daemon.json"
}
containers_selinux() {
  [ -d "$R/sys/fs/selinux" ] || return 1
  [ "$ENGINE" = podman ] || [ "$ENGINE" = both ] || docker_selinux
}

have() { command -v "$1" >/dev/null 2>&1; }

# selinux_module NAME CIL PUT_REASON LOAD_REASON: write /etc/quasar/selinux/NAME.cil and load
# it as the SELinux module NAME. Idempotent: the module is loaded again only when it is
# missing or its text changed (what was loaded is kept beside it as .NAME.loaded).
# SELinux stays enforcing; a module only adds the allow rules it names.
selinux_module() {
  m_name="$1"; m_cil="$2"
  printf '%s\n' "$m_cil" | put "/etc/quasar/selinux/$m_name.cil" 0644 "$3" || unchanged
  if live && ! have semodule; then
    die "semodule was not found: install policycoreutils, then run this again"
  fi
  if live && semodule -l 2>/dev/null | grep -qx "$m_name" \
      && cmp -s "$R/etc/quasar/selinux/$m_name.cil" "$R/etc/quasar/selinux/.$m_name.loaded"; then
    say ok "SELinux module $m_name"
  elif ! live && grep -qx "$m_name" "$R/.selinux-modules" 2>/dev/null \
      && cmp -s "$R/etc/quasar/selinux/$m_name.cil" "$R/etc/quasar/selinux/.$m_name.loaded"; then
    say ok "SELinux module $m_name"
  else
    run semodule -i "/etc/quasar/selinux/$m_name.cil"
    # What was loaded, so a changed rule is loaded again on the next run.
    [ "$DRY_RUN" = 1 ] || cp "$R/etc/quasar/selinux/$m_name.cil" "$R/etc/quasar/selinux/.$m_name.loaded"
    stand_in && echo "$m_name" >> "$R/.selinux-modules"
    say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "SELinux module $m_name — $4"
  fi
}

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
elif containers_selinux; then
  # Rootful: the engine would create a missing bind source itself, but unlabelled. Made
  # at boot by systemd, it takes the container label the rule below records.
  printf '# Written by Quasar host preparation. The node agent'"'"'s runtime directory, shared with its sessions.\nd /run/quasar-agent 0755 root root -\n' \
    | put /etc/tmpfiles.d/quasar.conf 0644 "/run/quasar-agent, made at every boot with its SELinux label" || unchanged
  run systemd-tmpfiles --create /etc/tmpfiles.d/quasar.conf
  stand_in && mkdir -p "$R/run/quasar-agent"
fi
# Sessions connect to the Wayland and PulseAudio sockets the agent makes here.
if containers_selinux && { [ -d "$R/run/quasar-agent" ] || [ "$DRY_RUN" = 1 ]; }; then
  label_root /run/quasar-agent
fi

# ── device access ──────────────────────────────────────────────────────────
STEP="device rules"
SETFACL=""
for p in /usr/bin/setfacl /bin/setfacl; do [ -x "$R$p" ] || [ -x "$p" ] && { SETFACL="$p"; break; }; done
[ -n "$SETFACL" ] || die "setfacl was not found (install the acl package). The device rules add access with it rather than changing a device's owner."
# The rules are the files in deploy/udev/ of the Quasar repository, byte for byte
# (deploy/test-prepare-host.sh fails when they differ), so installing them by hand
# from the device-rules page and running this script give the same result. Only a
# --user other than quasar, or setfacl under /bin, changes the written text.
tailor() {
  if [ "$QUSER" = quasar ] && [ "$SETFACL" = /usr/bin/setfacl ]; then cat
  else awk -v u="$QUSER" -v f="$SETFACL" '{ gsub(/g:quasar:rw/, "g:" u ":rw"); gsub(/'"'"'quasar'"'"' group/, "'"'"'" u "'"'"' group"); gsub(/\/usr\/bin\/setfacl/, f); print }'; fi
}
rules_base() {
  cat <<'EOF'
# Quasar device rules: the devices every Quasar host uses.
# Documented at https://accreleus.github.io/quasar/install/device-rules/
# Each rule ADDS read/write for the 'quasar' group to one kind of device. It changes no
# device's owner or mode, so every existing user of these devices keeps its access.

# Creating virtual input devices (keyboard, mouse, gamepad, touch) through /dev/uinput.
KERNEL=="uinput", SUBSYSTEM=="misc", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# The input devices Quasar itself creates, matched by name, and no others.
SUBSYSTEM=="input", KERNEL=="event*|js*", ATTRS{name}=="Quasar Virtual *", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# SELinux hosts only: give Quasar's own input devices the container file type, so a confined session container may open them.
SUBSYSTEM=="input", KERNEL=="event*|js*", ATTRS{name}=="Quasar Virtual *", ACTION!="remove", ENV{DEVNAME}=="?*", SECLABEL{selinux}="system_u:object_r:container_file_t:s0"
# GPU render nodes: hardware encode and rendering.
SUBSYSTEM=="drm", KERNEL=="renderD*", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
EOF
}
rules_console() {
  cat <<'EOF'
# Quasar device rules: console mode only (a desktop drives this machine's own screen).
# Documented at https://accreleus.github.io/quasar/install/device-rules/
# Each rule ADDS read/write for the 'quasar' group to one kind of device. It changes no
# device's owner or mode. With these rules the 'quasar' group can read what is typed on
# this machine's keyboards, so install this file only on a host that uses console mode.

# Display cards: the console desktop drives the screen directly, and Quasar reads which monitors are connected.
SUBSYSTEM=="drm", KERNEL=="card[0-9]*", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# Local sound devices: the console desktop plays its sound on this machine.
SUBSYSTEM=="sound", KERNEL=="pcmC*|controlC*|timer", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# i2c buses: monitor control (DDC), such as switching the monitor's input.
SUBSYSTEM=="i2c-dev", KERNEL=="i2c-[0-9]*", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# This machine's input devices (keyboards, mice, controllers, touchpads, buttons), so the console desktop can use them, including ones plugged in later.
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT}=="1", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
# Console mode's own virtual terminal: a session makes it the active one with the kernel keyboard off, so nothing typed in the session reaches a login prompt.
SUBSYSTEM=="tty", KERNEL=="tty8", ACTION!="remove", ENV{DEVNAME}=="?*", RUN+="/usr/bin/setfacl -m g:quasar:rw $devnode"
EOF
}
rules_seat() {
  cat <<'EOF'
# Quasar device rules: keep players' input off this machine's desktop.
# Documented at https://accreleus.github.io/quasar/install/device-rules/
# This file must sort after 70-uaccess.rules and before 73-seat-late.rules, which is
# where systemd-logind gives the logged-in desktop user access to seat0's devices.

# Quasar's virtual input devices: put them on a seat of their own and drop the uaccess tag, so a desktop user logged in on this machine gets no access to a player's controller, keyboard or mouse.
SUBSYSTEM=="input", ATTRS{name}=="Quasar Virtual *", ENV{ID_SEAT}="seat-quasar", TAG-="uaccess"
EOF
}
rules_changed=0
if rules_base | tailor | put /etc/udev/rules.d/70-quasar.rules 0644 "give the $QUSER group the devices Quasar uses, and only those"; then
  rules_changed=1
else unchanged; fi
if [ "$CONSOLE" = 1 ]; then
  if rules_console | tailor | put /etc/udev/rules.d/71-quasar-console.rules 0644 "console mode: give the $QUSER group the display cards, sound, i2c buses, tty8 and this machine's input devices, which the console desktop is given; the $QUSER group can read what is typed on this machine's keyboard"; then
    rules_changed=1
  else unchanged; fi
elif [ -f "$R/etc/udev/rules.d/71-quasar-console.rules" ]; then
  if [ "$DRY_RUN" = 1 ]; then
    say would "remove /etc/udev/rules.d/71-quasar-console.rules — console mode was not asked for"
  else
    rm -f "$R/etc/udev/rules.d/71-quasar-console.rules" || die "could not remove /etc/udev/rules.d/71-quasar-console.rules"
    say changed "/etc/udev/rules.d/71-quasar-console.rules removed — console mode was not asked for (run again with --console to keep it)"
  fi
  rules_changed=1
fi
if rules_seat | put /etc/udev/rules.d/72-quasar-seat.rules 0644 "keep Quasar's virtual input devices off this machine's desktop seat, so a desktop user logged in here does not receive a player's input"; then
  rules_changed=1
else unchanged; fi
if [ "$rules_changed" = 1 ]; then
  if run udevadm control --reload; then
    tty_match=""
    [ "$CONSOLE" = 1 ] && tty_match="--subsystem-match=tty"
    # shellcheck disable=SC2086 # empty or one word
    run udevadm trigger --subsystem-match=misc --subsystem-match=input --subsystem-match=drm --subsystem-match=sound --subsystem-match=i2c-dev $tty_match
  else
    say note "udev is not running here (a container?); the rules take effect when it runs, at the latest at boot"
  fi
fi

# ── console terminal ───────────────────────────────────────────────────────
# No login prompt may run on tty8: console mode takes it as its terminal, and a getty
# holding it would make every console session refuse to start. logind starts none there
# by default (NAutoVTs=6); masking both units keeps it so. Nothing running is stopped.
if [ "$CONSOLE" = 1 ]; then
  STEP="console terminal"
  for unit in getty@tty8.service autovt@tty8.service; do
    link="/etc/systemd/system/$unit"
    if [ "$(readlink "$R$link" 2>/dev/null)" = /dev/null ]; then
      say ok "$unit masked"
    elif [ -e "$R$link" ] || [ -L "$R$link" ]; then
      say warn "$link is this machine's own unit, left as it is: while a login prompt runs on tty8, console sessions refuse to start"
    else
      run systemctl mask "$unit"
      stand_in && { mkdir -p "$R/etc/systemd/system" && ln -s /dev/null "$R$link"; }
      say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "$unit masked — console mode uses tty8 as its terminal, so no login prompt may run there"
    fi
  done
fi

# ── console audio (retired) ────────────────────────────────────────────────
# A console session's desktop plays its own audio through the sound device it is given.
# Earlier runs could give the agent a PipeWire socket on a desktop user's session; nothing
# uses it now, and what they wrote is named here rather than removed from under that user.
STEP="console audio"
[ "$CONSOLE_AUDIO_RETIRED" = 1 ] \
  && say note "--console-audio-user is retired and ignored: a console desktop plays its own audio"
for f in /etc/pipewire/pipewire-pulse.conf.d/90-quasar-console.conf \
         /etc/tmpfiles.d/quasar-console-audio.conf \
         /etc/systemd/user/quasar-console-audio.service; do
  [ -e "$R$f" ] && say note "$f was written by an earlier run for console audio, which is retired: nothing uses it, remove it when convenient"
done

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
# The driver alone is not an NVIDIA host: a container shares its host's /proc, and a hybrid
# machine can load the driver with no usable device. The control node says a GPU is here.
if [ -e "$R/proc/driver/nvidia/version" ] && [ -e "$R/dev/nvidiactl" ]; then
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
  if containers_selinux && [ -f "$bool" ]; then
    case "$(cat "$bool")" in
      1*) say ok "SELinux container_use_xserver_devices on" ;;
      *)
        run setsebool -P container_use_xserver_devices on
        stand_in && printf '1 1' > "$bool"
        say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "SELinux container_use_xserver_devices on — lets confined containers open the NVIDIA device nodes (labelled xserver_misc_device_t), and nothing else; SELinux stays enforcing"
        ;;
    esac
    # Sessions run as container_engine_t, the policy's confined type for nested sandboxes
    # (Steam's bwrap). The boolean above covers container_t only; this one rule gives
    # container_engine_t the same NVIDIA device access, and nothing more.
    selinux_module quasar-nested-gpu "; Written by Quasar host preparation (deploy/prepare-host.sh).
(allow container_engine_t xserver_misc_device_t (chr_file (getattr ioctl lock map open read write append)))" \
      "the NVIDIA device rule for sessions' nested-sandbox SELinux type" \
      "lets sessions (container_engine_t) open the NVIDIA device nodes, as the boolean does for container_t; SELinux stays enforcing"
  fi
elif [ -e "$R/proc/driver/nvidia/version" ]; then
  say skipped "NVIDIA CDI specification — the NVIDIA driver is loaded but this machine has no NVIDIA device (/dev/nvidiactl)"
else
  say skipped "NVIDIA CDI specification — no NVIDIA driver loaded"
fi

# ── SELinux: session sound ─────────────────────────────────────────────────
# A session's PulseAudio sidecar runs as container_t and listens on a unix socket; an app that
# needs the nested-sandbox type (Steam's bwrap) runs as container_engine_t and must connect to
# it. The policy's own connectto rule is "self": it covers two containers of the same type
# only, so without this one the connect is denied and the stream is silent. MCS categories do
# not constrain connectto, so sharing them would change nothing. This is the one rule, on the
# one class, and nothing else. It is written on every SELinux host, not only NVIDIA ones.
STEP="SELinux session sound"
if containers_selinux; then
  selinux_module quasar-nested-audio "; Written by Quasar host preparation (deploy/prepare-host.sh).
(allow container_engine_t container_t (unix_stream_socket (connectto)))" \
    "the PulseAudio socket rule for sessions' nested-sandbox SELinux type" \
    "lets sessions (container_engine_t) connect to their session's PulseAudio sidecar (container_t); SELinux stays enforcing"
fi

# ── SELinux: console devices ───────────────────────────────────────────────
# A console session runs as container_engine_t too, and the policy gives that type none of
# what a desktop on the screen needs: the input nodes (event_device_t, under /dev/input,
# which is device_t), the sound nodes (sound_device_t), the hidraw nodes Steam Input reads
# (usb_device_t) and the udev database libudev reads (udev_var_run_t). Without them KWin
# starts with no keyboard or mouse. The display cards (dri_device_t) are already allowed by
# the boolean above. `watch` on the device directories is PipeWire's inotify on /dev/snd: without
# it the ALSA monitor fails and the desktop has no sound card. One module names exactly these,
# and only when --console asks for them.
STEP="SELinux console devices"
console_cil="/etc/quasar/selinux/quasar-console-devices.cil"
if [ "$CONSOLE" = 1 ]; then
  if containers_selinux; then
    selinux_module quasar-console-devices "; Written by Quasar host preparation (deploy/prepare-host.sh --console).
(allow container_engine_t device_t (dir (getattr open read search watch)))
(allow container_engine_t event_device_t (chr_file (getattr ioctl lock map open read write append)))
(allow container_engine_t sound_device_t (chr_file (getattr ioctl lock map open read write append)))
(allow container_engine_t usb_device_t (chr_file (getattr ioctl lock map open read write append)))
(allow container_engine_t udev_var_run_t (dir (getattr open read search)))
(allow container_engine_t udev_var_run_t (file (getattr open read map)))" \
      "the input, sound, hidraw and udev-data rules for a console session's SELinux type" \
      "lets console sessions (container_engine_t) open this machine's input, sound and hidraw nodes and read the udev database; SELinux stays enforcing"
  fi
elif [ -f "$R$console_cil" ] || { live && have semodule && semodule -l 2>/dev/null | grep -qx quasar-console-devices; }; then
  say note "SELinux module quasar-console-devices is installed from an earlier --console run and is left in place: remove it with 'semodule -r quasar-console-devices' (and delete $console_cil) if console mode is retired on this machine"
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
# Rootful Docker restarts Quasar's containers itself, but only once its daemon runs: some
# systems (Fedora CoreOS, uCore) ship it disabled.
if [ "$MODE" = rootful ] && { [ "$ENGINE" = docker ] || [ "$ENGINE" = both ]; }; then
  link="/etc/systemd/system/multi-user.target.wants/docker.service"
  if [ -L "$R$link" ] || [ -e "$R$link" ]; then
    say ok "docker.service enabled"
  elif [ -e "$R/usr/lib/systemd/system/docker.service" ]; then
    run systemctl enable docker.service
    stand_in && { mkdir -p "$(dirname "$R$link")" && ln -s /usr/lib/systemd/system/docker.service "$R$link"; }
    say "$([ "$DRY_RUN" = 1 ] && echo would || echo changed)" "docker.service enabled — the daemon, and with it Quasar's containers, start at boot"
  fi
fi
if [ "$MODE" = rootless ] && { [ "$ENGINE" = docker ] || [ "$ENGINE" = both ]; }; then
  say note "rootless Docker: install it for $QUSER with dockerd-rootless-setuptool.sh and enable its docker.service; lingering (above) keeps it running"
fi

# ── the homes and templates roots ──────────────────────────────────────────
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
  if containers_selinux && { [ -d "$R$1" ] || [ "$DRY_RUN" = 1 ]; }; then
    label_root "$1"
  fi
}

STEP="homes root"
[ -n "$HOMES" ] && data_root "$HOMES" "homes root" "where each user's game saves and settings live"
STEP="templates root"
[ -n "$TEMPLATES" ] && data_root "$TEMPLATES" "templates root" "where prepared app homes (Steam) are kept"

STEP="done"
printf 'Host preparation is complete. Run it again at any time: it changes only what is missing.\n'
