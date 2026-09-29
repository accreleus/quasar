#!/usr/bin/env bash
# Offline tests for deploy/prepare-host.sh, the one-time root step (RH-07, #400).
# A fake root directory (QUASAR_PREP_ROOT) stands in for the machine: the script
# writes its files there and records, instead of running, every command that would
# act on the live system. So these assert, with no root and no engine:
#
#   1. a fresh run writes exactly the expected files, and nothing under /usr;
#   2. a second run changes nothing;
#   3. the device rules add access for the quasar group to Quasar's devices only,
#      and change no device's owner, group or mode;
#   4. an optional setting is written only when asked for;
#   5. rootful prepares the device and kernel parts, and no account;
#   6. an existing CDI specification is used, not duplicated;
#   7. a failing step leaves earlier steps in place and says to run it again;
#   8. a dry run changes nothing;
#   9. an existing homes root is never re-owned.
#
# Run: bash deploy/test-prepare-host.sh
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/deploy/prepare-host.sh"
tmp="$(mktemp -d /tmp/quasar-prepare-host.XXXXXX)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

PASS_N=0; FAIL_N=0
pass() { PASS_N=$((PASS_N + 1)); printf 'PASS %s\n' "$1"; }
fail() { FAIL_N=$((FAIL_N + 1)); printf 'FAIL %s — %s\n' "$1" "${2:-}" >&2; }

# A PATH with only the tools the script uses, plus stub engines, so engine
# detection does not depend on what this machine has installed.
bin="$tmp/bin"; mkdir -p "$bin"
for t in awk cat cmp cp mv chmod mkdir mktemp grep dirname rm ln stat id printf sh basename tr head readlink; do
  p="$(command -v "$t" || true)"; [ -n "$p" ] && ln -s "$p" "$bin/$t"
done
stubs() { # stubs <dir> <name>... : empty executables that only exist
  local d="$1"; shift; mkdir -p "$d"
  for n in "$@"; do printf '#!/bin/sh\nexit 0\n' > "$d/$n"; chmod +x "$d/$n"; done
}
stubs "$tmp/podman-only" podman
stubs "$tmp/docker-only" docker
stubs "$tmp/no-ctk" podman
# nvidia-ctk prints a specification naming the driver it saw.
for d in "$tmp/podman-only" "$tmp/docker-only"; do
  printf '#!/bin/sh\nprintf "kind: nvidia.com/gpu\\n# driver %%s\\n" "$(cat "$QUASAR_PREP_ROOT/proc/driver/nvidia/version" | grep -oE "[0-9]+[.][0-9]+[.][0-9]+")"\n' > "$d/nvidia-ctk"
  chmod +x "$d/nvidia-ctk"
done

mk_root() { # mk_root <dir> [nvidia]
  local r="$1"
  rm -rf "$r"; mkdir -p "$r/etc" "$r/usr/bin" "$r/proc/driver"
  printf 'root:x:0:0:root:/root:/bin/bash\n' > "$r/etc/passwd"
  printf 'root:x:0:\n' > "$r/etc/group"
  printf 'someone:100000:65536\n' > "$r/etc/subuid"
  printf 'someone:100000:65536\n' > "$r/etc/subgid"
  printf '#!/bin/sh\n' > "$r/usr/bin/setfacl"; chmod +x "$r/usr/bin/setfacl"
  if [ "${2:-}" = nvidia ]; then mkdir -p "$r/proc/driver/nvidia" "$r/dev"; printf 'NVRM version: 615.71.09\n' > "$r/proc/driver/nvidia/version"; : > "$r/dev/nvidiactl"; fi
}

prep() { # prep <root> <stubdir> args...
  local r="$1" s="$2"; shift 2
  QUASAR_PREP_ROOT="$r" TMPDIR="$tmp" PATH="$s:$bin" sh "$script" "$@"
}

# Every file under the root with its content hash, except the command log.
tree() { (cd "$1" && find . \( -type f -o -type l \) ! -name .prepare-host-commands -print0 | sort -z | xargs -0 -I{} sh -c 'if [ -L "{}" ]; then printf "%s -> %s\n" "{}" "$(readlink "{}")"; else printf "%s %s\n" "{}" "$(cksum < "{}")"; fi'); }

# ── 1. fresh rootless Podman on NVIDIA: exactly these files ─────────────────
r="$tmp/r1"; mk_root "$r" nvidia
out="$(prep "$r" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar 2>&1)" || { fail "fresh rootless run" "$out"; }
expected="./dev/nvidiactl
./etc/cdi/nvidia.yaml
./etc/group
./etc/modules-load.d/quasar.conf
./etc/passwd
./etc/subgid
./etc/subuid
./etc/sysctl.d/99-quasar.conf
./etc/tmpfiles.d/quasar.conf
./etc/udev/rules.d/70-quasar.rules
./home/quasar/.config/systemd/user/default.target.wants/podman-restart.service
./home/quasar/.config/systemd/user/sockets.target.wants/podman.socket
./proc/driver/nvidia/version
./usr/bin/setfacl
./var/lib/systemd/linger/quasar"
got="$(cd "$r" && find . \( -type f -o -type l \) ! -name .prepare-host-commands | sort)"
if [ "$got" = "$expected" ]; then pass "fresh run writes exactly the expected files"; else fail "fresh run file set" "$(diff <(echo "$expected") <(echo "$got") | head -20)"; fi
[ -d "$r/var/lib/quasar" ] && pass "homes root created" || fail "homes root" "missing"
if [ "$(find "$r/usr" -newer "$r/etc/passwd" 2>/dev/null | wc -l)" = 0 ] && [ "$(ls "$r/usr/bin")" = setfacl ]; then pass "nothing written under /usr"; else fail "nothing under /usr" "$(find "$r/usr")"; fi
grep -q '^quasar:524288:65536$' "$r/etc/subuid" && grep -q '^someone:100000:65536$' "$r/etc/subuid" \
  && pass "subordinate range appended after existing ones" || fail "subuid" "$(cat "$r/etc/subuid")"
grep -q 'useradd --create-home --user-group' "$r/.prepare-host-commands" && grep -q 'loginctl enable-linger quasar' "$r/.prepare-host-commands" \
  && pass "account and lingering go through useradd and loginctl" || fail "account commands" "$(cat "$r/.prepare-host-commands")"
[ "$(readlink "$r/home/quasar/.config/systemd/user/default.target.wants/podman-restart.service")" = /usr/lib/systemd/user/podman-restart.service ] \
  && pass "podman-restart.service enabled for the Quasar user" || fail "podman-restart link" ""
grep -q '# driver 615.71.09' "$r/etc/cdi/nvidia.yaml" && pass "CDI spec generated by nvidia-ctk for the loaded driver" || fail "cdi generate" "$(cat "$r/etc/cdi/nvidia.yaml" 2>&1)"
for why in "runs everything under this one unprivileged account" "keep running with nobody logged in" "devices Quasar uses, and only those" "Podman has no daemon"; do
  printf '%s' "$out" | grep -q "$why" && pass "change printed with its reason: $why" || fail "reason printed: $why" "$out"
done

grep -q '^d /run/quasar-agent 0750 quasar quasar -$' "$r/etc/tmpfiles.d/quasar.conf" && grep -q 'systemd-tmpfiles --create /etc/tmpfiles.d/quasar.conf' "$r/.prepare-host-commands" \
  && pass "rootless: /run/quasar-agent is created for the Quasar user by tmpfiles.d" || fail "runtime dir" "$(cat "$r/etc/tmpfiles.d/quasar.conf" 2>&1)"

# ── 2. a second run changes nothing ─────────────────────────────────────────
before="$(tree "$r")"
out2="$(prep "$r" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar 2>&1)" || fail "second run" "$out2"
after="$(tree "$r")"
[ "$before" = "$after" ] && pass "second run leaves every file identical" || fail "idempotent files" "$(diff <(echo "$before") <(echo "$after") | head)"
if printf '%s' "$out2" | grep -qE '^  (changed|would) '; then fail "second run reports no change" "$(printf '%s' "$out2" | grep -E '^  (changed|would)')"; else pass "second run reports no change"; fi

# ── 3. device rules grant only Quasar's devices, by ACL ─────────────────────
rules="$r/etc/udev/rules.d/70-quasar.rules"
if grep -qE 'GROUP=|MODE=|OWNER=' "$rules"; then fail "rules change no owner/group/mode" "$(grep -E 'GROUP=|MODE=|OWNER=' "$rules")"; else pass "rules change no device's owner, group or mode"; fi
[ "$(grep -c 'ACTION!="remove", ENV{DEVNAME}=="?\*"' "$rules")" = 3 ] && pass "every rule skips remove events and devices without a node" || fail "udev guards" "$(grep -v '^#' "$rules")"
grep -q 'ATTRS{name}=="Quasar Virtual \*"' "$rules" && pass "input rule matches only Quasar's devices by name" || fail "input by name" ""
[ "$(grep -c 'setfacl -m g:quasar:rw \$devnode' "$rules")" = 3 ] && pass "three device rules without --console (uinput, Quasar input, render nodes)" || fail "rule count" "$(grep -v '^#' "$rules")"
if grep -qE 'card|sound|i2c' <(grep -v '^#' "$rules"); then fail "console devices only with --console" ""; else pass "no display, sound or i2c access without --console"; fi
if grep -qiE 'g:(input|video|render|audio):' "$rules"; then fail "no broad group" ""; else pass "no broad group such as input or video is granted"; fi
r3="$tmp/r3"; mk_root "$r3"
out3="$(prep "$r3" "$tmp/podman-only" --mode rootless --engine podman --console 2>&1)"
grep -q 'KERNEL=="card\[0-9\]\*"' "$r3/etc/udev/rules.d/70-quasar.rules" && grep -q 'i2c-dev' "$r3/etc/modules-load.d/quasar.conf" \
  && pass "--console adds display, sound and i2c, and loads i2c-dev" || fail "--console" ""
r3rules="$r3/etc/udev/rules.d/70-quasar.rules"
grep -q 'SUBSYSTEM=="input", KERNEL=="event\*", ENV{ID_INPUT_KEYBOARD}=="1"' "$r3rules" \
  && grep -q 'SUBSYSTEM=="input", KERNEL=="event\*", ENV{ID_INPUT_MOUSE}=="1"' "$r3rules" \
  && grep -q 'SUBSYSTEM=="input", KERNEL=="event\*", ENV{ID_INPUT_JOYSTICK}=="1"' "$r3rules" \
  && pass "--console grants the host's physical keyboards, mice and joysticks by udev property" || fail "physical input rules" "$(cat "$r3rules")"
printf '%s' "$out3" | grep -q "the quasar group can read what is typed on this machine's keyboard" \
  && pass "--console states plainly that the quasar group can read this machine's keyboard" || fail "plain keyboard warning" "$out3"
grep -q '^SUBSYSTEM=="tty", KERNEL=="tty8", ACTION!="remove", ENV{DEVNAME}=="?\*", RUN+="/usr/bin/setfacl -m g:quasar:rw \$devnode"$' "$r3rules" \
  && [ "$(grep -c 'SUBSYSTEM=="tty"' "$r3rules")" = 1 ] \
  && pass "--console grants tty8, and only tty8, by ACL" || fail "console VT rule" "$(grep tty "$r3rules")"
if grep -q 'tty' <(grep -v '^#' "$r/etc/udev/rules.d/70-quasar.rules"); then fail "no terminal without --console" ""; else pass "no terminal access without --console"; fi
grep -q 'udevadm trigger .*--subsystem-match=tty' "$r3/.prepare-host-commands" \
  && pass "--console re-applies the rules to terminals" || fail "tty trigger" "$(grep udevadm "$r3/.prepare-host-commands")"
[ "$(readlink "$r3/etc/systemd/system/getty@tty8.service")" = /dev/null ] && [ "$(readlink "$r3/etc/systemd/system/autovt@tty8.service")" = /dev/null ] \
  && grep -q 'systemctl mask getty@tty8.service' "$r3/.prepare-host-commands" && grep -q 'systemctl mask autovt@tty8.service' "$r3/.prepare-host-commands" \
  && pass "--console masks getty@tty8 and autovt@tty8" || fail "tty8 getty masked" "$(cat "$r3/.prepare-host-commands")"
if grep -q -- '--now' "$r3/.prepare-host-commands"; then fail "nothing running is stopped" "$(grep -- --now "$r3/.prepare-host-commands")"; else pass "masking stops nothing that runs"; fi
[ ! -e "$r/etc/systemd/system/getty@tty8.service" ] && pass "no unit is masked without --console" || fail "mask without --console" ""
before3="$(tree "$r3")"
out3again="$(prep "$r3" "$tmp/podman-only" --mode rootless --engine podman --console 2>&1)"
[ "$before3" = "$(tree "$r3")" ] && printf '%s' "$out3again" | grep -q 'ok       getty@tty8.service masked' \
  && [ "$(grep -c 'systemctl mask getty@tty8.service' "$r3/.prepare-host-commands")" = 1 ] \
  && pass "--console re-run masks nothing again" || fail "tty8 mask idempotent" "$out3again"
r3o="$tmp/r3o"; mk_root "$r3o"; mkdir -p "$r3o/etc/systemd/system"; printf '[Service]\n' > "$r3o/etc/systemd/system/getty@tty8.service"
out3o="$(prep "$r3o" "$tmp/podman-only" --mode rootless --engine podman --console 2>&1)" || fail "operator unit run" "$out3o"
printf '%s' "$out3o" | grep -q 'getty@tty8.service is this machine.s own unit, left as it is' && [ -f "$r3o/etc/systemd/system/getty@tty8.service" ] && [ ! -L "$r3o/etc/systemd/system/getty@tty8.service" ] \
  && pass "an operator's own tty8 unit is left alone, with a warning" || fail "operator tty8 unit" "$out3o"
r3d="$tmp/r3d"; mk_root "$r3d"
out3d="$(prep "$r3d" "$tmp/podman-only" --mode rootless --engine podman --console --dry-run 2>&1)"
[ ! -e "$r3d/etc/systemd/system" ] && printf '%s' "$out3d" | grep -q 'would    getty@tty8.service masked' \
  && pass "--console dry run masks nothing and says it would" || fail "tty8 dry run" "$out3d"

# ── 3b. console audio (PipeWire) ────────────────────────────────────────────
r3b="$tmp/r3b"; mk_root "$r3b"; printf 'alice:x:1500:1500::/home/alice:/bin/bash\n' >> "$r3b/etc/passwd"
out3b="$(prep "$r3b" "$tmp/podman-only" --mode rootless --engine podman --console --console-audio-user alice 2>&1)" || fail "console audio run" "$out3b"
pw="$r3b/etc/pipewire/pipewire-pulse.conf.d/90-quasar-console.conf"
grep -q '"unix:native"' "$pw" && grep -q 'address = "unix:/run/quasar-console-audio/native"' "$pw" && grep -q 'client.access = "restricted"' "$pw" \
  && pass "the PipeWire drop-in keeps unix:native and adds the Quasar console socket" || fail "pipewire drop-in" "$(cat "$pw" 2>&1)"
tf="$r3b/etc/tmpfiles.d/quasar-console-audio.conf"
grep -q '^d /run/quasar-console-audio 0750 alice quasar -$' "$tf" \
  && pass "tmpfiles.d creates /run/quasar-console-audio owned alice:quasar 0750" || fail "console audio tmpfiles" "$(cat "$tf" 2>&1)"
printf '%s' "$out3b" | grep -q "restart alice's pipewire-pulse" && pass "console audio output explains the desktop user must restart pipewire-pulse" || fail "console audio restart note" "$out3b"

if prep "$tmp/none" "$tmp/podman-only" --mode rootless --console-audio-user ghost 2>/dev/null; then fail "unknown console-audio-user" "exit 0"; else pass "--console-audio-user refuses an unknown account"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --console-audio-user alice 2>/dev/null; then fail "console-audio-user without --console" "exit 0"; else pass "--console-audio-user without --console is refused"; fi

before3b="$(tree "$r3b")"
out3b2="$(prep "$r3b" "$tmp/podman-only" --mode rootless --engine podman --console --console-audio-user alice 2>&1)"
[ "$before3b" = "$(tree "$r3b")" ] && pass "console audio re-run leaves every file identical" || fail "console audio idempotent files" "$(diff <(echo "$before3b") <(tree "$r3b") | head)"
if printf '%s' "$out3b2" | grep -qE '^  (changed|would) '; then fail "console audio re-run reports no change" "$(printf '%s' "$out3b2" | grep -E '^  (changed|would)')"; else pass "console audio re-run prints only ok lines"; fi

# ── 4. optional settings only when asked ────────────────────────────────────
if grep -qE 'dmesg_restrict|unprivileged_port_start' "$r/etc/sysctl.d/99-quasar.conf"; then fail "no optional sysctl by default" ""; else pass "optional kernel settings absent unless asked"; fi
printf '%s' "$out" | grep -q -- '--allow-kernel-log enables that optional diagnostic' && pass "says how to enable the optional diagnostic" || fail "optional note" ""
r4="$tmp/r4"; mk_root "$r4"
prep "$r4" "$tmp/podman-only" --mode rootless --engine podman --allow-kernel-log --unprivileged-port-start 443 >/dev/null 2>&1
out4b="$(prep "$r4" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
printf '%s' "$out4b" | grep -q 'warn     kernel.dmesg_restrict was granted by an earlier run' && pass "a withdrawn optional setting is announced" || fail "withdrawn optional" "$out4b"
prep "$r4" "$tmp/podman-only" --mode rootless --engine podman --allow-kernel-log --unprivileged-port-start 443 >/dev/null 2>&1
grep -q '^kernel.dmesg_restrict=0$' "$r4/etc/sysctl.d/99-quasar.conf" && grep -q '^net.ipv4.ip_unprivileged_port_start=443$' "$r4/etc/sysctl.d/99-quasar.conf" \
  && pass "optional kernel settings written when asked" || fail "optional sysctl" "$(cat "$r4/etc/sysctl.d/99-quasar.conf")"

# ── 5. rootful: devices and kernel, no account ──────────────────────────────
r5="$tmp/r5"; mk_root "$r5" nvidia
out5="$(prep "$r5" "$tmp/podman-only" --mode rootful --engine podman --homes /srv/quasar 2>&1)" || fail "rootful run" "$out5"
if grep -q '^quasar:' "$r5/etc/passwd" || grep -q '^quasar:' "$r5/etc/subuid" || [ -e "$r5/var/lib/systemd/linger/quasar" ]; then fail "rootful creates no account" ""; else pass "rootful creates no account, subordinate range or lingering"; fi
[ ! -e "$r5/etc/tmpfiles.d/quasar.conf" ] && pass "rootful needs no runtime-directory entry (the engine creates it)" || fail "rootful tmpfiles" ""
grep -q 'groupadd --system quasar' "$r5/.prepare-host-commands" && pass "rootful creates only the quasar group" || fail "rootful group" ""
[ -L "$r5/etc/systemd/system/default.target.wants/podman-restart.service" ] && pass "rootful Podman enables the system podman-restart.service" || fail "rootful restart" ""
[ -f "$r5/etc/udev/rules.d/70-quasar.rules" ] && [ -f "$r5/etc/sysctl.d/99-quasar.conf" ] && pass "rootful writes the device and kernel parts" || fail "rootful parts" ""
[ ! -e "$r5/srv/quasar" ] && printf '%s' "$out5" | grep -q "keeps today's ownership" && pass "rootful leaves the homes root to the installer" || fail "rootful homes" "$out5"
r5d="$tmp/r5d"; mk_root "$r5d"
out5d="$(prep "$r5d" "$tmp/docker-only" --mode rootful 2>&1)" || fail "rootful docker auto" "$out5d"
printf '%s' "$out5d" | grep -q 'engine: docker' && [ ! -e "$r5d/etc/systemd/system" ] && pass "rootful Docker (detected) needs no restart service" || fail "rootful docker" "$out5d"

# ── 6. an existing CDI specification is used ────────────────────────────────
r6="$tmp/r6"; mk_root "$r6" nvidia; mkdir -p "$r6/var/run/cdi"; printf 'kind: nvidia.com/gpu\n# 615.71.09\n' > "$r6/var/run/cdi/nvidia.yaml"
out6="$(prep "$r6" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
[ ! -e "$r6/etc/cdi" ] && printf '%s' "$out6" | grep -q 'ok       NVIDIA CDI specification (/var/run/cdi/nvidia.yaml, driver 615.71.09)' \
  && pass "a current CDI specification kept by the toolkit is used, none written" || fail "existing cdi" "$out6"
r6s="$tmp/r6s"; mk_root "$r6s" nvidia; mkdir -p "$r6s/var/run/cdi"; printf 'kind: nvidia.com/gpu\n# 610.57.04\n' > "$r6s/var/run/cdi/nvidia.yaml"
prep "$r6s" "$tmp/podman-only" --mode rootless --engine podman >/dev/null 2>&1
grep -q '615.71.09' "$r6s/etc/cdi/nvidia.yaml" 2>/dev/null && pass "a specification for another driver version is not trusted; ours is generated" || fail "stale foreign cdi" ""
printf 'NVRM version: 620.10.01\n' > "$r/proc/driver/nvidia/version"
prep "$r" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar >/dev/null 2>&1
grep -q '620.10.01' "$r/etc/cdi/nvidia.yaml" && pass "a re-run after a driver upgrade regenerates our specification" || fail "cdi regen" "$(cat "$r/etc/cdi/nvidia.yaml")"
r6b="$tmp/r6b"; mk_root "$r6b"
out6b="$(prep "$r6b" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
printf '%s' "$out6b" | grep -q 'skipped  NVIDIA CDI specification — no NVIDIA driver loaded' && pass "no CDI work on a host without NVIDIA" || fail "no nvidia" "$out6b"
r6c="$tmp/r6c"; mk_root "$r6c" nvidia; rm -f "$r6c/dev/nvidiactl"
out6c="$(prep "$r6c" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
printf '%s' "$out6c" | grep -q 'skipped  NVIDIA CDI specification — the NVIDIA driver is loaded but this machine has no NVIDIA device' && [ ! -e "$r6c/etc/cdi/nvidia.yaml" ] \
  && pass "a loaded driver with no NVIDIA device (another GPU's container, a hybrid machine) is not an NVIDIA host" || fail "driver without device" "$out6c"

# ── 7. a failing step: earlier steps stay, and the fix is to re-run ─────────
r7="$tmp/r7"; mk_root "$r7" nvidia
if out7="$(prep "$r7" "$tmp/no-ctk" --mode rootless --engine podman 2>&1)"; then fail "missing nvidia-ctk fails" "exit 0"; else
  printf '%s' "$out7" | grep -q 'Install the NVIDIA Container Toolkit' && pass "missing nvidia-ctk names the fix" || fail "ctk fix" "$out7"
  printf '%s' "$out7" | grep -q 'run this script again' && pass "failure says earlier steps are in place and to re-run" || fail "rerun hint" "$out7"
  [ -f "$r7/etc/udev/rules.d/70-quasar.rules" ] && [ -f "$r7/etc/sysctl.d/99-quasar.conf" ] && pass "earlier steps stay in place after a failure" || fail "earlier steps" ""
  if find "$r7" -name '*.quasar-new' | grep -q .; then fail "no half-written file" "$(find "$r7" -name '*.quasar-new')"; else pass "nothing is half-written"; fi
fi

# ── 7b. a write that fails is fatal, never reported as done ─────────────────
r7b="$tmp/r7b"; mk_root "$r7b"; mkdir -p "$r7b/etc/sysctl.d"; chmod 555 "$r7b/etc/sysctl.d"
if out7b="$(prep "$r7b" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"; then fail "failed write is fatal" "exit 0: $out7b"; else
  printf '%s' "$out7b" | grep -q 'could not write /etc/sysctl.d/99-quasar.conf' && pass "a failed write names the file" || fail "failed write message" "$out7b"
  if printf '%s' "$out7b" | grep -qE 'changed  /etc/sysctl.d|preparation is complete'; then fail "failed write not reported as done" "$out7b"; else pass "a failed write is never reported as a change or completion"; fi
fi
chmod 755 "$r7b/etc/sysctl.d"

# ── 7c. rootful first, then rootless: the account joins the existing group ─
r7c="$tmp/r7c"; mk_root "$r7c"
prep "$r7c" "$tmp/podman-only" --mode rootful --engine podman >/dev/null 2>&1
prep "$r7c" "$tmp/podman-only" --mode rootless --engine podman >/dev/null 2>&1
grep -q 'useradd --create-home -g quasar' "$r7c/.prepare-host-commands" && pass "rootless after rootful joins the existing quasar group" || fail "rootful then rootless" "$(cat "$r7c/.prepare-host-commands")"

# ── 7d. SELinux: the one targeted boolean, only where Podman confines the GPU ─
r7d="$tmp/r7d"; mk_root "$r7d" nvidia; mkdir -p "$r7d/sys/fs/selinux/booleans"; printf '0 0' > "$r7d/sys/fs/selinux/booleans/container_use_xserver_devices"
out7d="$(prep "$r7d" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
grep -q 'setsebool -P container_use_xserver_devices on' "$r7d/.prepare-host-commands" && printf '%s' "$out7d" | grep -q 'SELinux stays enforcing' \
  && pass "NVIDIA + SELinux + Podman turns on container_use_xserver_devices, with its reason" || fail "selinux boolean" "$out7d"
if grep -qE 'setenforce|label=disable|container_use_devices' "$script"; then fail "never relaxes SELinux" ""; else pass "never disables SELinux, labels or all-device access"; fi
grep -q 'semodule -i /etc/quasar/selinux/quasar-nested-gpu.cil' "$r7d/.prepare-host-commands" \
  && grep -q '(allow container_engine_t xserver_misc_device_t (chr_file' "$r7d/etc/quasar/selinux/quasar-nested-gpu.cil" \
  && [ "$(grep -c allow "$r7d/etc/quasar/selinux/quasar-nested-gpu.cil")" = 1 ] \
  && pass "the nested-sandbox type gets exactly one NVIDIA device rule" || fail "nested gpu module" "$(cat "$r7d/.prepare-host-commands")"
out7d2="$(prep "$r7d" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
printf '%s' "$out7d2" | grep -q 'ok       SELinux container_use_xserver_devices on' && pass "the boolean is left alone once on" || fail "selinux idempotent" "$out7d2"
[ "$(grep -c 'semodule -i' "$r7d/.prepare-host-commands")" = 1 ] && pass "the module is installed once" || fail "module idempotent" "$(cat "$r7d/.prepare-host-commands")"
r7e="$tmp/r7e"; mk_root "$r7e" nvidia; mkdir -p "$r7e/sys/fs/selinux/booleans"; printf '0 0' > "$r7e/sys/fs/selinux/booleans/container_use_xserver_devices"
prep "$r7e" "$tmp/docker-only" --mode rootful >/dev/null 2>&1
if grep -q setsebool "$r7e/.prepare-host-commands" 2>/dev/null; then fail "no boolean for Docker" ""; else pass "Docker without SELinux does not get the SELinux boolean"; fi

# ── 8. a dry run changes nothing ────────────────────────────────────────────
r8="$tmp/r8"; mk_root "$r8" nvidia
b8="$(tree "$r8")"
out8="$(prep "$r8" "$tmp/podman-only" --mode rootless --engine podman --dry-run 2>&1)" || fail "dry run" "$out8"
[ "$b8" = "$(tree "$r8")" ] && [ ! -e "$r8/.prepare-host-commands" ] && pass "dry run changes nothing and runs nothing" || fail "dry run" "$(tree "$r8")"
printf '%s' "$out8" | grep -q 'would    create account quasar' && pass "dry run says what it would do" || fail "dry run output" "$out8"

# ── 9. an existing homes root is never re-owned ─────────────────────────────
r9="$tmp/r9"; mk_root "$r9"; mkdir -p "$r9/data/homes"
prep "$r9" "$tmp/podman-only" --mode rootless --engine podman --homes /data/homes >/dev/null 2>&1
if grep -qE 'chown|install -d' "$r9/.prepare-host-commands"; then fail "existing homes untouched" "$(cat "$r9/.prepare-host-commands")"; else pass "an existing homes root is never re-owned"; fi
if grep -rq 'chown -R\|chown --recursive' "$script"; then fail "no recursive chown in the script" ""; else pass "the script contains no recursive re-own"; fi

# ── 9b. SELinux Podman: the data roots carry the container label, persistently ─
r9b="$tmp/r9b"; mk_root "$r9b"; mkdir -p "$r9b/sys/fs/selinux"
out9b="$(prep "$r9b" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar/homes --templates /var/lib/quasar/templates 2>&1)" || fail "labelled run" "$out9b"
grep -qF 'semanage fcontext -a -t container_file_t /var/lib/quasar/homes(/.*)?' "$r9b/.prepare-host-commands" \
  && grep -qF 'restorecon -R /var/lib/quasar/templates' "$r9b/.prepare-host-commands" \
  && pass "homes and templates roots get a persistent container_file_t context" || fail "selinux labels" "$(cat "$r9b/.prepare-host-commands" 2>/dev/null) $out9b"
[ -d "$r9b/var/lib/quasar/templates" ] && pass "--templates creates the templates root" || fail "templates root" "missing"
grep -qF 'semanage fcontext -a -t container_file_t /run/quasar-agent(/.*)?' "$r9b/.prepare-host-commands" \
  && pass "the agent's runtime directory (session sockets) gets the container label" || fail "runtime dir label" "$(cat "$r9b/.prepare-host-commands")"
out9b2="$(prep "$r9b" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar/homes --templates /var/lib/quasar/templates 2>&1)"
[ "$(grep -c 'semanage fcontext' "$r9b/.prepare-host-commands")" = 3 ] && printf '%s' "$out9b2" | grep -q 'ok       SELinux label on /var/lib/quasar/homes' \
  && pass "labels are added once" || fail "label idempotent" "$out9b2"
r9c="$tmp/r9c"; mk_root "$r9c"; mkdir -p "$r9c/sys/fs/selinux"
prep "$r9c" "$tmp/docker-only" --mode rootless --engine docker --homes /var/lib/quasar/homes >/dev/null 2>&1
if grep -q semanage "$r9c/.prepare-host-commands" 2>/dev/null; then fail "no label for Docker" ""; else pass "Docker without SELinux: data roots are not relabelled"; fi
grep 'Quasar Virtual' "$r9b/etc/udev/rules.d/70-quasar.rules" | grep -q 'SECLABEL{selinux}="system_u:object_r:container_file_t:s0"' \
  && [ "$(grep -c SECLABEL "$r9b/etc/udev/rules.d/70-quasar.rules")" = 1 ] \
  && pass "only Quasar's own input devices get the container label" || fail "input seclabel" "$(cat "$r9b/etc/udev/rules.d/70-quasar.rules")"
if grep -q SECLABEL "$r9c/etc/udev/rules.d/70-quasar.rules"; then fail "no input label for Docker" ""; else pass "Docker without SELinux: the input rule carries no label"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --templates 'relative' 2>/dev/null; then fail "--templates relative" "exit 0"; else pass "--templates must be absolute"; fi
for bad in '/srv/q|/etc' '/srv/q[a]' '/var/lib' '/home'; do
  if prep "$tmp/none" "$tmp/podman-only" --mode rootless --homes "$bad" 2>/dev/null; then fail "refuse $bad" "exit 0"; else pass "--homes $bad is refused (regex or system tree)"; fi
done
r9d="$tmp/r9d"; mk_root "$r9d"; mkdir -p "$r9d/sys/fs/selinux" "$r9d/data/people/ann"; printf 'ann:x:1500:1500::/data/people/ann:/bin/sh\n' >> "$r9d/etc/passwd"
if prep "$r9d" "$tmp/podman-only" --mode rootless --engine podman --homes /data/people >/dev/null 2>&1; then fail "a root holding a user's home is not labelled" "exit 0"; else pass "a directory holding a user's home is never relabelled"; fi
grep -qF 'container_file_t /var/lib/quasar\.d/h(/.*)?' "$(r=$tmp/r9e; mk_root "$r"; mkdir -p "$r/sys/fs/selinux"; prep "$r" "$tmp/podman-only" --mode rootless --engine podman --homes /var/lib/quasar.d/h >/dev/null 2>&1; echo "$r/.prepare-host-commands")" \
  && pass "a dot in the path is escaped in the file context" || fail "dot escaping" "$(cat "$tmp/r9e/.prepare-host-commands" 2>/dev/null)"

# ── 9f. rootful Docker running --selinux-enabled confines like Podman ───────
r9f="$tmp/r9f"; mk_root "$r9f" nvidia; mkdir -p "$r9f/sys/fs/selinux/booleans" "$r9f/usr/lib/systemd/system" "$r9f/var/lib/quasar/h" "$r9f/run/quasar-agent"
printf '0 0' > "$r9f/sys/fs/selinux/booleans/container_use_xserver_devices"
printf '[Service]\nExecStart=/usr/bin/dockerd \\\n    --selinux-enabled \\\n    --host=fd://\n' > "$r9f/usr/lib/systemd/system/docker.service"
out9f="$(prep "$r9f" "$tmp/docker-only" --mode rootful --engine docker --homes /var/lib/quasar/h 2>&1)" || fail "docker selinux run" "$out9f"
grep -qF 'semanage fcontext -a -t container_file_t /var/lib/quasar/h(/.*)?' "$r9f/.prepare-host-commands" \
  && grep -qF 'semanage fcontext -a -t container_file_t /run/quasar-agent(/.*)?' "$r9f/.prepare-host-commands" \
  && grep -q 'setsebool -P container_use_xserver_devices on' "$r9f/.prepare-host-commands" \
  && grep -q 'semodule -i' "$r9f/.prepare-host-commands" \
  && grep -q SECLABEL "$r9f/etc/udev/rules.d/70-quasar.rules" \
  && grep -q 'd /run/quasar-agent 0755 root root' "$r9f/etc/tmpfiles.d/quasar.conf" \
  && pass "rootful Docker with --selinux-enabled gets the labels, boolean, module and a boot-made runtime dir" \
  || fail "docker selinux" "$(cat "$r9f/.prepare-host-commands" 2>/dev/null) $out9f"
grep -q 'systemctl enable docker.service' "$r9f/.prepare-host-commands" && [ -L "$r9f/etc/systemd/system/multi-user.target.wants/docker.service" ] \
  && pass "rootful Docker is enabled at boot" || fail "docker at boot" "$(cat "$r9f/.prepare-host-commands")"
prep "$r9f" "$tmp/docker-only" --mode rootful --engine docker --homes /var/lib/quasar/h >/dev/null 2>&1
[ "$(grep -c 'systemctl enable docker.service' "$r9f/.prepare-host-commands")" = 1 ] && pass "docker.service is enabled once" || fail "docker enable idempotent" ""

# ── argument validation ─────────────────────────────────────────────────────
if prep "$tmp/none" "$tmp/podman-only" 2>/dev/null; then fail "--mode required" "exit 0"; else pass "--mode is required"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --unprivileged-port-start 8080 2>/dev/null; then fail "port range" "exit 0"; else pass "--unprivileged-port-start must be at most 1024"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --homes /usr/share/q 2>/dev/null; then fail "homes under /usr" "exit 0"; else pass "--homes under /usr is refused"; fi

printf '\n%d passed, %d failed\n' "$PASS_N" "$FAIL_N"
[ "$FAIL_N" = 0 ]
