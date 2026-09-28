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
for t in awk cat cmp cp mv chmod mkdir mktemp grep dirname rm ln stat id printf sh basename tr head; do
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
  if [ "${2:-}" = nvidia ]; then mkdir -p "$r/proc/driver/nvidia"; printf 'NVRM version: 615.71.09\n' > "$r/proc/driver/nvidia/version"; fi
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
expected="./etc/cdi/nvidia.yaml
./etc/group
./etc/modules-load.d/quasar.conf
./etc/passwd
./etc/subgid
./etc/subuid
./etc/sysctl.d/99-quasar.conf
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
prep "$r3" "$tmp/podman-only" --mode rootless --engine podman --console >/dev/null 2>&1
grep -q 'KERNEL=="card\[0-9\]\*"' "$r3/etc/udev/rules.d/70-quasar.rules" && grep -q 'i2c-dev' "$r3/etc/modules-load.d/quasar.conf" \
  && pass "--console adds display, sound and i2c, and loads i2c-dev" || fail "--console" ""

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
out7d2="$(prep "$r7d" "$tmp/podman-only" --mode rootless --engine podman 2>&1)"
printf '%s' "$out7d2" | grep -q 'ok       SELinux container_use_xserver_devices on' && pass "the boolean is left alone once on" || fail "selinux idempotent" "$out7d2"
r7e="$tmp/r7e"; mk_root "$r7e" nvidia; mkdir -p "$r7e/sys/fs/selinux/booleans"; printf '0 0' > "$r7e/sys/fs/selinux/booleans/container_use_xserver_devices"
prep "$r7e" "$tmp/docker-only" --mode rootful >/dev/null 2>&1
if grep -q setsebool "$r7e/.prepare-host-commands" 2>/dev/null; then fail "no boolean for Docker" ""; else pass "Docker does not get the SELinux boolean"; fi

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

# ── argument validation ─────────────────────────────────────────────────────
if prep "$tmp/none" "$tmp/podman-only" 2>/dev/null; then fail "--mode required" "exit 0"; else pass "--mode is required"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --unprivileged-port-start 8080 2>/dev/null; then fail "port range" "exit 0"; else pass "--unprivileged-port-start must be at most 1024"; fi
if prep "$tmp/none" "$tmp/podman-only" --mode rootless --homes /usr/share/q 2>/dev/null; then fail "homes under /usr" "exit 0"; else pass "--homes under /usr is refused"; fi

printf '\n%d passed, %d failed\n' "$PASS_N" "$FAIL_N"
[ "$FAIL_N" = 0 ]
