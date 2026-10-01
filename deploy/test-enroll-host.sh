#!/usr/bin/env bash
# Offline contract tests for deploy/enroll-host.sh, the one-line GPU-host installer.
# A mock docker on PATH keeps a small model of the engine (containers, volumes) and
# records every invocation; a fake root directory stands in for /proc, /sys, /dev and
# /etc. So these assert the installer's contract without a daemon, a GPU or a network:
#
#   1. the enrollment string is required, parsed, and never allowed to point the
#      agent at a cleartext ws:// control plane;
#   2. the images come from the served pins (testdata/enroll-host/pins.json), by digest;
#   3. a failed host check stops before anything is pulled or started and names its
#      fix; QUASAR_ENROLL_FIX=1 applies it;
#   4. the seed is started with the token only in a 0600 env file, and nothing else is
#      written: no compose file, no .env, no install directory;
#   5. re-running on an installed machine starts and changes nothing;
#   6. a refused string leaves nothing behind on a machine this run installed, and an
#      installed machine is reset only on request;
#   7. the app-container AppArmor profile is loaded on an AppArmor host only;
#   8. the engine and its mode are found by their sockets and confirmed by the engine
#      (Docker and Podman, rootful and rootless, with a mock podman beside the mock
#      docker); a rootless run never calls sudo; an unsupported engine profile is
#      refused by name before anything is pulled; a rootless host without host
#      preparation gets a pointer to the rootless steps and nothing else;
#   9. the script's engine-profile table is testdata/engine-profiles/profiles.json.
#
# Run: bash deploy/test-enroll-host.sh
#      bash deploy/test-enroll-host.sh --write-profiles   (regenerate the table in
#      deploy/enroll-host.sh from testdata/engine-profiles/profiles.json)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/deploy/enroll-host.sh"
pins="$root/testdata/enroll-host/pins.json"
profiles="$root/testdata/engine-profiles/profiles.json"

# The table enroll-host.sh carries between its markers, generated from profiles.json.
# The shell has no JSON reader, so the table is rendered here, where python3 is (the DX
# layer needs it anyway), and the test below fails when the two drift.
PROFILES_BEGIN='# BEGIN engine profiles (generated)'
PROFILES_END='# END engine profiles (generated)'
generate_profiles() {
  python3 - "$profiles" <<'PY_GEN'
import json, sys
t = json.load(open(sys.argv[1]))
def alts(a):
    return ' '.join(x['engine'] + '/' + x['mode'] + ('@' + x['platform'] if 'platform' in x else '') for x in a)
out = ["engine_profiles() {", "cat <<'PROFILES'"]
out += ['platform|%s|%s' % (k, v['label']) for k, v in t['platforms'].items()]
out += ['engine|%s|%s' % (k, v['label']) for k, v in t['engines'].items()]
for r in t['profiles']:
    out.append('profile|%s|%s|%s|%s|%s|%s' % (r['platform'], r['engine'], r['mode'], r['status'], alts(r['alternatives']), r['reason']))
u = t['unknownEngine']
out.append('unknown|%s|%s|%s' % (u['status'], alts(u['alternatives']), u['reason']))
out += ["PROFILES", "}"]
for line in out:
    if '\n' in line or line.count('|') > 7:
        sys.exit('profiles.json: a field carries a newline or a | the shell table cannot hold: ' + line)
print('\n'.join(out))
PY_GEN
}
embedded_profiles() { sed -n "/^$PROFILES_BEGIN\$/,/^$PROFILES_END\$/p" "$script" | sed '1d;$d'; }
if [ "${1:-}" = --write-profiles ]; then
  generated="$(generate_profiles)"
  awk -v b="$PROFILES_BEGIN" -v e="$PROFILES_END" -v g="$generated" '
    $0 == b { print; print g; skip = 1; next }
    $0 == e { skip = 0 }
    !skip { print }' "$script" > "$script.new" && cat "$script.new" > "$script" && rm -f "$script.new"
  echo "rewrote the engine-profile table in deploy/enroll-host.sh"
  exit 0
fi
tmp="$(mktemp -d /tmp/quasar-enroll-host.XXXXXX)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

PASS_N=0; FAIL_N=0
pass() { PASS_N=$((PASS_N + 1)); printf 'PASS %s\n' "$1"; }
fail() { FAIL_N=$((FAIL_N + 1)); printf 'FAIL %s — %s\n' "$1" "${2:-}" >&2; }

# ── fixtures ────────────────────────────────────────────────────────────────
FP="0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9:0A:1B:2C:3D:4E:5F:60:71:82:93:A4:B5:C6:D7:E8:F9"
TOKEN="s3cr3t-t0ken.with.dots"
b64url() { printf '%s' "$1" | base64 | tr -d '\n=' | tr '+/' '-_'; }
WSS_BLOB="qenr1.$FP.$(b64url 'wss://cp.example:8443').$TOKEN"
WS_BLOB="qenr1..$(b64url 'ws://cp.example:8080').$TOKEN"
SEED_IMG="$(sed -n 's/.*"seed_image": *"\([^"]*\)".*/\1/p' "$pins")"
AGENT_IMG="$(sed -n 's/.*"agent_image": *"\([^"]*\)".*/\1/p' "$pins")"

# The script as the control plane serves it: the pins.json lines in place of the
# placeholders (control-plane/internal/enrollscript renders exactly these).
served="$tmp/served.sh"
seed_line="$(grep -o "PINNED_SEED_IMAGE='[^']*'" "$pins")"
agent_line="$(grep -o "PINNED_AGENT_IMAGE='[^']*'" "$pins")"
sed -e "s|^PINNED_SEED_IMAGE=''\$|$seed_line|" -e "s|^PINNED_AGENT_IMAGE=''\$|$agent_line|" "$script" > "$served"

# A fake host root. Defaults describe a healthy AMD box on a Debian-family distro.
mk_root() { # mk_root <dir> [vendor]
  local r="$1" vendor="${2:-0x1002}"
  rm -rf "$r"
  mkdir -p "$r/sys/class/drm/renderD128/device" "$r/dev/dri" "$r/proc/sys/kernel" "$r/proc/sys/user" "$r/etc"
  printf '%s\n' "$vendor" > "$r/sys/class/drm/renderD128/device/vendor"
  : > "$r/dev/dri/renderD128"
  : > "$r/dev/uinput"
  printf '15000\n' > "$r/proc/sys/user/max_user_namespaces"
  printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$r/etc/os-release"
  # The rootful Docker socket (a plain file: the script tests existence).
  mkdir -p "$r/var/run"; : > "$r/var/run/docker.sock"
}

# The engine model: $MOCK_STATE/c/<name>/{state,command,labels,logs}, $MOCK_STATE/v/<name>/labels.
state="$tmp/engine"
reset_engine() { rm -rf "$state"; mkdir -p "$state/c" "$state/v"; }
container() { # container <name> <state> <command> [key=value label…]
  local d="$state/c/$1"; mkdir -p "$d"
  printf '%s' "$2" > "$d/state"; printf '%s' "$3" > "$d/command"; : > "$d/labels"; : > "$d/logs"
  shift 3
  local l; for l in "$@"; do printf '%s\n' "$l" >> "$d/labels"; done
}
volume() { # volume <name> [key=value label…]
  local d="$state/v/$1"; mkdir -p "$d"; : > "$d/labels"
  shift
  local l; for l in "$@"; do printf '%s\n' "$l" >> "$d/labels"; done
}
# An installed GPU host: seed, recovery actor, agent and their volumes.
installed_machine() { # installed_machine <agent log>
  reset_engine
  container quasar-seed running "/usr/local/bin/quasar-recovery seed"
  container quasar-recovery running "/usr/local/bin/quasar-recovery actor" \
    io.quasar.installation=inst-0 io.quasar.platform-service=recovery-actor io.quasar.recipe=1
  container quasar-node-agent running "/usr/local/bin/quasar-node-agent-entrypoint" \
    io.quasar.installation=inst-0 io.quasar.platform-service=node-agent io.quasar.recipe=1
  printf '%s\n' "$1" > "$state/c/quasar-node-agent/logs"
  volume quasar-machine
  volume quasar-agent-data io.quasar.installation=inst-0
  volume quasar-recovery-agent
  echo quasar-recovery-agent > "$state/c/quasar-recovery/volumes"
  echo quasar-recovery-agent > "$state/c/quasar-node-agent/volumes"
}

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -uo pipefail
S="${MOCK_STATE:?}"
CLI="${0##*/}"
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
printf '%s DOCKER_HOST=%s CONTAINER_HOST=%s\n' "$CLI" "${DOCKER_HOST:-}" "${CONTAINER_HOST:-}" >>"${MOCK_CLI_LOG:-/dev/null}"
# A row's labels in a --format: Docker's rows have a Label method and a string .Labels;
# Podman's have a .Labels map and no Label method. The wrong form fails as the real one does.
case "$*" in
  *'{{.Label "'*) [ "$CLI" = podman ] && { echo "Error: template: ps:1: can't evaluate field Label" >&2; exit 125; } ;;
esac
case "$*" in
  *'{{index .Labels "'*) case "${1:-}" in ps|volume) [ "$CLI" = docker ] && { echo "template: :1: error calling index: cannot index slice/array with type string" >&2; exit 1; } ;; esac ;;
esac
mk() {
  local d="$S/c/$1"; mkdir -p "$d"
  printf '%s' "$2" >"$d/state"; printf '%s' "$3" >"$d/command"; : >"$d/labels"; : >"$d/logs"
  shift 3
  local l; for l in "$@"; do printf '%s\n' "$l" >>"$d/labels"; done
}
has_label() { case "$2" in *=*) grep -qxF "$2" "$1/labels" ;; *) grep -q "^$2=" "$1/labels" ;; esac; }
label_value() { sed -n "s/^$2=//p" "$1/labels" | head -n 1; }
last="${!#}"
cmd="$1"; shift
case "$cmd" in
  info)
    [ "${MOCK_INFO_OK:-1}" = 1 ] || { echo "Cannot connect to the engine" >&2; exit 1; }
    case "${2:-}" in
      *SecurityOptions*) if [ "${MOCK_ROOTLESS:-0}" = 1 ]; then echo '["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]'; else echo '["name=seccomp,profile=builtin","name=cgroupns"]'; fi ;;
      *Rootless*) if [ "${MOCK_ROOTLESS:-0}" = 1 ]; then echo true; else echo false; fi ;;
      *) echo "${MOCK_HOSTNAME:-gpu-b}" ;;
    esac
    exit 0 ;;
  ps)
    fmt='{{.Names}}'; filters=()
    while [ $# -gt 0 ]; do
      case "$1" in --format) fmt="$2"; shift 2 ;; --filter) filters+=("$2"); shift 2 ;; *) shift ;; esac
    done
    for d in "$S"/c/*; do
      [ -d "$d" ] || continue
      keep=1
      for f in ${filters[@]+"${filters[@]}"}; do
        case "$f" in
          label=*) has_label "$d" "${f#label=}" || keep=0 ;;
          volume=*) grep -qxF "${f#volume=}" "$d/volumes" 2>/dev/null || keep=0 ;;
        esac
      done
      [ "$keep" = 1 ] || continue
      out="${fmt//\{\{.Names\}\}/${d##*/}}"
      out="${out//\{\{.Label \"io.quasar.installation\"\}\}/$(label_value "$d" io.quasar.installation)}"
      out="${out//\{\{index .Labels \"io.quasar.installation\"\}\}/$(label_value "$d" io.quasar.installation)}"
      out="${out//\{\{.Command\}\}/\"$(cat "$d/command")\"}"
      printf '%s\n' "$out"
    done
    exit 0 ;;
  inspect)
    [ -d "$S/c/$last" ] || { echo "Error: No such object: $last" >&2; exit 1; }
    case "${1:-}|${2:-}" in
      *'io.quasar.installation'*) label_value "$S/c/$last" io.quasar.installation ;;
      *'com.docker.compose.project'*) label_value "$S/c/$last" com.docker.compose.project ;;
      *'.Config.Image'*) echo "mock.example/quasar/quasar-recovery@sha256:$(printf 'a%.0s' $(seq 64))" ;;
      *) cat "$S/c/$last/state"; echo ;;
    esac
    exit 0 ;;
  image) [ "${MOCK_IMAGES_PRESENT:-0}" = 1 ]; exit ;;
  pull) [ "${MOCK_PULL_OK:-1}" = 1 ] || { echo "mock: pull refused: $last" >&2; exit 1; }; exit 0 ;;
  volume)
    sub="$1"
    case "$sub" in
      inspect) [ -d "$S/v/$last" ]; exit ;;
      ls)
        want=""; fmt=""
        for a in "$@"; do case "$a" in label=*) want="${a#label=}" ;; *'.Label'*) fmt="$a" ;; esac; done
        fmt="${fmt//\{\{index .Labels /\{\{.Label }"
        for d in "$S"/v/*; do
          [ -d "$d" ] && { [ -z "$want" ] || has_label "$d" "$want"; } || continue
          if [ -n "$fmt" ]; then
            out="${fmt//\{\{.Name\}\}/${d##*/}}"
            out="${out//\{\{.Label \"io.quasar.helper\"\}\}/$(label_value "$d" io.quasar.helper)}"
            printf '%s\n' "${out//\{\{.Label \"io.quasar.installation\"\}\}/$(label_value "$d" io.quasar.installation)}"
          else echo "${d##*/}"; fi
        done
        exit 0 ;;
      rm) [ "${MOCK_VOLUME_RM_OK:-1}" = 1 ] || { echo "mock: volume is in use" >&2; exit 1; }; rm -rf "${S:?}/v/$last"; exit 0 ;;
    esac
    exit 0 ;;
  rm) rm -rf "${S:?}/c/$last"; exit 0 ;;
  start) [ -d "$S/c/$last" ] && echo running >"$S/c/$last/state"; exit 0 ;;
  logs)
    # The waiting starts after the seed's docker run; its env file must be gone by then.
    if [ -f "$S/seed.env.path" ] && [ -e "$(cat "$S/seed.env.path")" ]; then echo alive >> "$S/seed.env.alive"; fi
    [ -d "$S/c/$last" ] && cat "$S/c/$last/logs"; exit 0 ;;
  exec) printf '%s\n' "${MOCK_SEED_STATUS:-}"; exit 0 ;;
  run)
    # The recovery actor's `uninstall --purge --confirm <id>`: what carries the id goes;
    # it empties the machine-state volume it mounts but cannot remove it.
    case " $* " in
      # A read of machine state: $S/v/quasar-machine/files/<name>.
      *" --entrypoint cat "*)
        cat "$S/v/quasar-machine/files/${last##*/}" 2>/dev/null; exit ;;
      *" uninstall "*)
        # An actor image from before uninstall existed refuses the command.
        case " $* " in *" mock.example/quasar/quasar-recovery@"*) [ "${MOCK_ACTOR_NO_UNINSTALL:-0}" = 1 ] && { echo "error: unrecognized subcommand 'uninstall'" >&2; exit 2; } ;; esac
        [ "${MOCK_UNINSTALL_OK:-1}" = 1 ] || { echo "ERROR uninstall: could not remove container quasar-node-agent: conflict" >&2; exit 1; }
        id=""; prev=""
        for a in "$@"; do [ "$prev" = --confirm ] && id="$a"; prev="$a"; done
        for d in "$S"/c/* "$S"/v/*; do
          [ -d "$d" ] && [ -n "$id" ] && has_label "$d" "io.quasar.installation=$id" && rm -rf "$d"
        done
        rm -rf "${S:?}/v/quasar-machine/files"
        printf '%s\n' "$*" >> "$S/uninstalls"
        exit 0 ;;
    esac
    envf=""; name=""; args=("$@"); i=0
    while [ "$i" -lt "${#args[@]}" ]; do
      case "${args[$i]}" in --env-file) envf="${args[$((i + 1))]}" ;; --name) name="${args[$((i + 1))]}" ;; esac
      i=$((i + 1))
    done
    if [ -n "$envf" ]; then cp "$envf" "$S/seed.env"; stat -c %a "$envf" > "$S/seed.env.mode"; printf '%s' "$envf" > "$S/seed.env.path"; fi
    [ "${MOCK_RUN_OK:-1}" = 1 ] || exit 125
    mk "$name" running "/usr/local/bin/quasar-recovery seed"
    mkdir -p "$S/v/quasar-machine"; : > "$S/v/quasar-machine/labels"
    # The seed at work: it creates the recovery actor, which creates the agent.
    if [ "${MOCK_SEED_CREATES:-1}" = 1 ]; then
      mk quasar-recovery running "/usr/local/bin/quasar-recovery actor" io.quasar.installation=inst-1 io.quasar.platform-service=recovery-actor
      echo quasar-recovery-agent > "$S/c/quasar-recovery/volumes"
      # The actor profile's socket volume: created by the engine, so unlabelled.
      mkdir -p "$S/v/quasar-recovery-agent"; : > "$S/v/quasar-recovery-agent/labels"
      printf '%s\n' "${MOCK_ACTOR_LOG:-}" > "$S/c/quasar-recovery/logs"
      if [ -z "${MOCK_ACTOR_LOG:-}" ]; then
        mk quasar-node-agent running "/usr/local/bin/quasar-node-agent-entrypoint" io.quasar.installation=inst-1 io.quasar.platform-service=node-agent
        echo quasar-recovery-agent > "$S/c/quasar-node-agent/volumes"
        printf '%s\n' "${MOCK_AGENT_LOG:-}" > "$S/c/quasar-node-agent/logs"
      fi
      for v in quasar-agent-data quasar-node-agent-secrets; do
        mkdir -p "$S/v/$v"; echo io.quasar.installation=inst-1 > "$S/v/$v/labels"
      done
    fi
    echo c0ffee; exit 0 ;;
  *) exit 0 ;;
esac
MOCK
chmod +x "$tmp/bin/docker"
ln -s docker "$tmp/bin/podman"
printf '#!/bin/sh\necho "${MOCK_HOSTNAME:-gpu-b}"\n' > "$tmp/bin/hostname"; chmod +x "$tmp/bin/hostname"
# sudo shim: the tests never run as root. It refuses to run without -n (the
# installer must never let sudo prompt from inside a pipe); MOCK_SUDO_PASSWORD=1
# makes it behave like a host whose sudo wants a password.
cat >"$tmp/bin/sudo" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_SUDO_LOG:-/dev/null}"
[ "${1:-}" = "-n" ] || { echo "mock sudo: invoked without -n (would prompt)" >&2; exit 97; }
shift
[ "${MOCK_SUDO_PASSWORD:-0}" = 1 ] && { echo "sudo: a password is required" >&2; exit 1; }
exec "$@"
MOCK
chmod +x "$tmp/bin/sudo"
cat >"$tmp/bin/apparmor_parser" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_AA_LOG:-/dev/null}"
[ -z "${MOCK_AA_COPY:-}" ] || cp "${!#}" "$MOCK_AA_COPY"
exit 0
MOCK
chmod +x "$tmp/bin/apparmor_parser"
# The host fixes act on the fake root: sysctl writes the knob, modprobe makes the node.
cat >"$tmp/bin/sysctl" <<'MOCK'
#!/usr/bin/env bash
printf 'sysctl %s\n' "$*" >>"${MOCK_FIX_LOG:?}"
for kv in "$@"; do
  case "$kv" in -w) continue ;; esac
  k="${kv%%=*}"; v="${kv#*=}"
  printf '%s\n' "$v" > "${QUASAR_ENROLL_ROOT:?}/proc/sys/${k//.//}"
done
MOCK
chmod +x "$tmp/bin/sysctl"
cat >"$tmp/bin/modprobe" <<'MOCK'
#!/usr/bin/env bash
printf 'modprobe %s\n' "$*" >>"${MOCK_FIX_LOG:?}"
[ "$1" = uinput ] && : > "${QUASAR_ENROLL_ROOT:?}/dev/uinput"
MOCK
chmod +x "$tmp/bin/modprobe"
cat >"$tmp/bin/curl" <<'MOCK'
#!/usr/bin/env bash
[ -n "${MOCK_HEALTH:-}" ] || exit 7
printf '%s\n' "$MOCK_HEALTH"
MOCK
chmod +x "$tmp/bin/curl"

# run_installer <label> [ENV=val …] — pipes the script (the served one unless
# SCRIPT=…) into sh the way the one-liner does; captures output, rc and the logs.
run_installer() {
  local label="$1"; shift
  local log="$tmp/$label.docker.log" aalog="$tmp/$label.aa.log" fixlog="$tmp/$label.fix.log"
  local clilog="$tmp/$label.cli.log" sudolog="$tmp/$label.sudo.log"
  : > "$log"; : > "$aalog"; : > "$fixlog"; : > "$clilog"; : > "$sudolog"
  set +e
  # The workstation's own engine never leaks in: no DOCKER_HOST, and a runtime directory
  # holding only the sockets a test put there.
  env -u DOCKER_HOST -u CONTAINER_HOST -u QUASAR_ENGINE XDG_RUNTIME_DIR="${XDG:-$tmp/xdg-none}" \
      PATH="$tmp/bin:$PATH" MOCK_STATE="$state" MOCK_DOCKER_LOG="$log" MOCK_AA_LOG="$aalog" MOCK_FIX_LOG="$fixlog" \
      MOCK_CLI_LOG="$clilog" MOCK_SUDO_LOG="$sudolog" \
      QUASAR_ENROLL_ROOT="${ROOT_DIR:-$tmp/root}" QUASAR_ENROLL_TAIL_SECS=1 QUASAR_ENROLL_FIX="${FIX:-0}" \
      QUASAR_ENROLL_STYLE="${STYLE:-plain}" "$@" sh ${EXTRA_ARGS:-} < "${SCRIPT:-$served}" > "$tmp/$label.out" 2>&1
  RC=$?
  set -e
  OUT="$(cat "$tmp/$label.out")"
  DOCKER_LOG="$(cat "$log")"
  AA_LOG="$(cat "$aalog")"
  FIX_LOG="$(cat "$fixlog")"
  CLI_LOG="$(cat "$clilog")"
  SUDO_LOG="$(cat "$sudolog")"
}
ENROLLED_LOG='2026-09-25T10:00:00Z INFO quasar_node_agent::agent: enrolled as host 3f2c…; node_secret saved to /var/lib/quasar-agent/node-secret'
OK_ENV=(QUASAR_ENROLLMENT="$WSS_BLOB" MOCK_AGENT_LOG="$ENROLLED_LOG")
# A read of machine state (`run --rm --entrypoint cat`) starts nothing.
acted() { grep -v -- '--entrypoint cat ' <<<"$DOCKER_LOG" || true; }
started() { acted | grep -q '^run '; }
nothing_started() { ! acted | grep -qE '^(run|pull|start|rm) '; }
engine_empty() { [ -z "$(ls -A "$state/c")" ] && [ -z "$(ls -A "$state/v")" ]; }

# ── 1. the string is required and never cleartext ────────────────────────────
mk_root "$tmp/root"; reset_engine

run_installer no-string
if [ "$RC" -eq 2 ] && grep -q 'QUASAR_ENROLLMENT' <<<"$OUT" && grep -q 'Add host' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "no enrollment string: rc=2, points at Admin → Fleet → Add host, touches no docker"
else
  fail "no enrollment string" "rc=$RC docker=[$DOCKER_LOG] out=$(head -3 <<<"$OUT")"
fi

run_installer ws-blob QUASAR_ENROLLMENT="$WS_BLOB"
if [ "$RC" -eq 2 ] && grep -qi 'cleartext' <<<"$OUT" && grep -q 'ws://' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "ws:// string: refused as cleartext before docker is touched"
else
  fail "ws:// string" "rc=$RC docker=[$DOCKER_LOG] out=$(head -3 <<<"$OUT")"
fi

run_installer bad-blob QUASAR_ENROLLMENT="qenr9.$FP.abc.$TOKEN"
if [ "$RC" -eq 2 ] && grep -qi 'not an enrollment string' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "unknown prefix: refused as not an enrollment string"
else
  fail "unknown prefix" "rc=$RC out=$(head -3 <<<"$OUT")"
fi

run_installer bad-fp QUASAR_ENROLLMENT="qenr1.NOT-A-FINGERPRINT.$(b64url 'wss://cp.example:8443').$TOKEN"
if [ "$RC" -eq 2 ] && grep -qi 'fingerprint' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "malformed fingerprint: refused"
else
  fail "malformed fingerprint" "rc=$RC out=$(head -3 <<<"$OUT")"
fi

run_installer bad-name "${OK_ENV[@]}" QUASAR_NODE_NAME='gpu b;x'
if [ "$RC" -eq 2 ] && grep -q 'QUASAR_NODE_NAME' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "a node name outside the agent's alphabet: refused, naming the variable"
else
  fail "bad node name" "rc=$RC out=$(head -3 <<<"$OUT")"
fi

# ── 2. the images: served pins, by digest ────────────────────────────────────
SCRIPT="$script" run_installer unpinned "${OK_ENV[@]}"
if [ "$RC" -eq 2 ] && grep -q 'QUASAR_ENROLL_SEED_IMAGE' <<<"$OUT" && grep -q 'QUASAR_ENROLL_AGENT_IMAGE' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "a script served with no pins: rc=2, names the control plane's two variables"
else
  fail "unpinned script" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

run_installer tag-image "${OK_ENV[@]}" QUASAR_SEED_IMAGE=registry.example.invalid/quasar/quasar-recovery:0.6.0
if [ "$RC" -eq 2 ] && grep -q 'not pinned by digest' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "a seed image given by tag: refused, the seed would refuse it too"
else
  fail "tag image" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# ── 3. host checks refuse before anything is pulled or started ───────────────
mk_root "$tmp/root"; reset_engine
run_installer sudo-pw "${OK_ENV[@]}" MOCK_SUDO_PASSWORD=1
if [ "$RC" -eq 1 ] && grep -q 'password' <<<"$OUT" && grep -q 'sudo -i' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "sudo wants a password: refused up front with the root-shell way out; never prompts"
else
  fail "sudo password" "rc=$RC docker=[$DOCKER_LOG] out=$(head -4 <<<"$OUT")"
fi

mk_root "$tmp/root"; reset_engine
printf '1\n' > "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
run_installer apparmor-knob "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'sysctl -w kernel.apparmor_restrict_unprivileged_userns=0' <<<"$OUT" && grep -q '#76' <<<"$OUT" \
   && grep -q 'QUASAR_ENROLL_FIX=1' <<<"$OUT" && nothing_started && [ -z "$FIX_LOG" ]; then
  pass "Ubuntu 24.04 AppArmor knob: refused up front, names the fix and QUASAR_ENROLL_FIX=1, applies nothing"
else
  fail "apparmor knob" "rc=$RC docker=[$DOCKER_LOG] out=$(head -4 <<<"$OUT")"
fi
FIX=1 run_installer apparmor-knob-fix "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q 'sysctl -w kernel.apparmor_restrict_unprivileged_userns=0' <<<"$FIX_LOG" \
   && [ "$(cat "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns")" = 0 ] \
   && grep -qxF 'kernel.apparmor_restrict_unprivileged_userns=0' "$tmp/root/etc/sysctl.d/99-quasar-userns.conf" \
   && grep -q 'fixed' <<<"$OUT" && started; then
  pass "QUASAR_ENROLL_FIX=1: the knob is set now and persisted in sysctl.d, then the install goes ahead"
else
  fail "apparmor knob fix" "rc=$RC fix=[$FIX_LOG] out=$(tail -5 <<<"$OUT")"
fi

mk_root "$tmp/root"; reset_engine
rm "$tmp/root/dev/uinput"
run_installer uinput "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'modprobe uinput' <<<"$OUT" && nothing_started; then
  pass "missing /dev/uinput: refused with the modprobe line"
else
  fail "uinput check" "rc=$RC out=$(head -4 <<<"$OUT")"
fi
reset_engine
EXTRA_ARGS="-s -- --fix" run_installer uinput-fix "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q 'modprobe uinput' <<<"$FIX_LOG" && [ -e "$tmp/root/dev/uinput" ] \
   && grep -qxF uinput "$tmp/root/etc/modules-load.d/uinput.conf" && started; then
  pass "--fix: uinput loaded now and at every boot, then the install goes ahead"
else
  fail "uinput fix" "rc=$RC fix=[$FIX_LOG] out=$(tail -4 <<<"$OUT")"
fi

mk_root "$tmp/root"; reset_engine
printf '0\n' > "$tmp/root/proc/sys/user/max_user_namespaces"
FIX=1 run_installer userns-fix "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q 'user.max_user_namespaces=15000 kernel.unprivileged_userns_clone=1' <<<"$FIX_LOG" \
   && grep -qxF 'user.max_user_namespaces=15000' "$tmp/root/etc/sysctl.d/99-quasar-userns.conf"; then
  pass "max_user_namespaces=0 on Ubuntu: both knobs set and persisted by the fix"
else
  fail "userns fix" "rc=$RC fix=[$FIX_LOG] out=$(tail -4 <<<"$OUT")"
fi

mk_root "$tmp/root"; reset_engine
rm -r "$tmp/root/sys/class/drm/renderD128"
FIX=1 run_installer nogpu "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -qi 'render node' <<<"$OUT" && nothing_started; then
  pass "no render node: refused, and no fix is pretended"
else
  fail "gpu check" "rc=$RC out=$(head -4 <<<"$OUT")"
fi

# ── 4. a fresh AMD host ──────────────────────────────────────────────────────
mk_root "$tmp/root"; reset_engine
touch "$tmp/before-amd-ok"
run_installer amd-ok "${OK_ENV[@]}" QUASAR_NODE_NAME=gpu-host-6
runline="$(grep '^run ' <<<"$DOCKER_LOG" || true)"
envf="$state/seed.env"
if [ "$RC" -eq 0 ] \
   && grep -q -- "-d --name quasar-seed --restart unless-stopped --security-opt label=disable -v /var/run/docker.sock:/var/run/docker.sock -v quasar-machine:/var/lib/quasar-machine:ro --env-file " <<<"$runline" \
   && grep -q -- " $SEED_IMG seed\$" <<<"$runline" \
   && grep -q "^pull -q $SEED_IMG\$" <<<"$DOCKER_LOG" && grep -q "^pull -q $AGENT_IMG\$" <<<"$DOCKER_LOG" \
   && [ "$(cat "$state/seed.env.mode")" = 600 ] \
   && diff <(sort "$envf") <(printf '%s\n' QUASAR_ROLE=gpu "QUASAR_ENROLLMENT=$WSS_BLOB" QUASAR_HOME_ROOT=/var/lib/quasar/homes \
        QUASAR_TEMPLATE_ROOT=/var/lib/quasar/templates "QUASAR_AGENT_IMAGE=$AGENT_IMG" QUASAR_NODE_NAME=gpu-host-6 | sort) >/dev/null \
   && grep -q "enrolled: this host is now 'gpu-host-6'" <<<"$OUT"; then
  pass "fresh AMD host: both served images pulled by digest, the seed started as documented with a 0600 env file, enrolled"
else
  fail "happy path" "rc=$RC run=[$runline] env=[$(cat "$envf" 2>/dev/null)] out=$(tail -5 <<<"$OUT")"
fi
if ! grep -qF "$TOKEN" <<<"$DOCKER_LOG" && ! grep -qF "$TOKEN" <<<"$OUT" && [ ! -e "$(cat "$state/seed.env.path")" ] \
   && [ ! -e "$state/seed.env.alive" ]; then
  pass "the token reaches only the seed's 0600 env file, deleted right after docker run: no docker argv, no output line"
else
  fail "token containment" "leaked into docker argv or output"
fi
written="$(find "$tmp/root" -newer "$tmp/before-amd-ok" | head -3)"
if ! grep -q '^compose' <<<"$DOCKER_LOG" && [ -z "$written" ]; then
  pass "no compose file, no .env, no install directory: nothing written on the host"
else
  fail "nothing written" "$written"
fi

reset_engine
run_installer amd-defaults "${OK_ENV[@]}" QUASAR_HOME_ROOT=/srv/quasar/homes MOCK_HOSTNAME=study-pc
if [ "$RC" -eq 0 ] && grep -qxF QUASAR_TEMPLATE_ROOT=/srv/quasar/templates "$state/seed.env" \
   && ! grep -q QUASAR_NODE_NAME "$state/seed.env" && grep -q "this host is now 'study-pc'" <<<"$OUT"; then
  pass "templates always set beside the home root; unbound, the seed names the host after the machine"
else
  fail "defaults" "rc=$RC env=[$(cat "$state/seed.env")] out=$(tail -3 <<<"$OUT")"
fi

reset_engine
MOCK_IMAGES_PRESENT=1 run_installer images-present "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && ! grep -q '^pull' <<<"$DOCKER_LOG" && grep -q 'image: present' <<<"$OUT"; then
  pass "images already on the engine: not pulled again"
else
  fail "images present" "rc=$RC docker=[$DOCKER_LOG]"
fi

reset_engine
run_installer pull-fails "${OK_ENV[@]}" MOCK_PULL_OK=0
if [ "$RC" -eq 1 ] && grep -q 'insecure-registries' <<<"$OUT" && ! started; then
  pass "an image that cannot be pulled: refused before the seed starts, with the registry hint"
else
  fail "pull fails" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# ── 5. NVIDIA host ───────────────────────────────────────────────────────────
mk_root "$tmp/root" 0x10de; reset_engine
run_installer nv-notoolkit "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'NVIDIA Container Toolkit' <<<"$OUT" && nothing_started; then
  pass "NVIDIA without the container toolkit: refused, names it"
else
  fail "nvidia toolkit check" "rc=$RC out=$(head -6 <<<"$OUT")"
fi
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/bin/nvidia-ctk"; chmod +x "$tmp/bin/nvidia-ctk"
run_installer nv-ok "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q 'CDI' <<<"$OUT" && started; then
  pass "NVIDIA host with the toolkit: installs; a missing CDI spec is a named warning"
else
  fail "nvidia happy path" "rc=$RC out=$(tail -6 <<<"$OUT")"
fi
rm -f "$tmp/bin/nvidia-ctk"

# A system container sees every GPU in /sys but only its own device nodes.
mk_root "$tmp/root" 0x10de; reset_engine
rm "$tmp/root/dev/dri/renderD128"
mkdir -p "$tmp/root/sys/class/drm/renderD129/device"
printf '0x1002\n' > "$tmp/root/sys/class/drm/renderD129/device/vendor"
: > "$tmp/root/dev/dri/renderD129"
run_installer sysfs-only-gpu "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q 'gpu: amd (/dev/dri/renderD129)' <<<"$OUT" && ! grep -q 'NVIDIA' <<<"$OUT"; then
  pass "a GPU listed in /sys without its device node is not this machine's: the AMD node is used"
else
  fail "sysfs-only gpu" "rc=$RC out=$(grep -i gpu <<<"$OUT" | head -3)"
fi

# ── 6. re-runs, refusals, interrupted runs ───────────────────────────────────
mk_root "$tmp/root"
installed_machine 'INFO quasar_node_agent::agent: reconnected as host 3f2c…'
before="$(find "$state" -type f | sort | xargs cat | md5sum)"
run_installer rerun QUASAR_ENROLLMENT="$WSS_BLOB"
if [ "$RC" -eq 0 ] && nothing_started && [ "$(find "$state" -type f | sort | xargs cat | md5sum)" = "$before" ] \
   && grep -q 'Already installed' <<<"$OUT" && grep -q 'already enrolled' <<<"$OUT" && grep -q 'not used' <<<"$OUT"; then
  pass "re-run on an enrolled machine: nothing pulled, started, removed or changed; reports the agent"
else
  fail "re-run" "rc=$RC docker=[$(grep -E '^(run|pull|start|rm)' <<<"$DOCKER_LOG")] out=$(tail -4 <<<"$OUT")"
fi
installed_machine ''
run_installer rerun-quiet QUASAR_ENROLLMENT="$WSS_BLOB" MOCK_HEALTH='{"status":"ok","sessions":0,"connected":true}'
if [ "$RC" -eq 0 ] && nothing_started && grep -q 'already enrolled' <<<"$OUT"; then
  pass "re-run with no verdict in the agent's recent log: its health endpoint says connected"
else
  fail "re-run health" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

reset_engine
run_installer spent "${OK_ENV[@]}" MOCK_AGENT_LOG='ERROR quasar_node_agent::agent: control plane rejected register: auth_failed: authentication failed'
if [ "$RC" -eq 1 ] && grep -q 'expired, was already used' <<<"$OUT" && grep -q 'Add host' <<<"$OUT" \
   && grep -q 'Nothing was left on this machine' <<<"$OUT" && engine_empty \
   && grep -q '^run --rm .* uninstall --purge --confirm inst-1$' <<<"$DOCKER_LOG" \
   && [ "$(grep -n '^rm -f quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" -lt "$(grep -n ' uninstall --purge ' <<<"$DOCKER_LOG" | cut -d: -f1)" ]; then
  pass "spent or expired token on a fresh machine: clear message, create-a-new-command hint, nothing left (seed removed first, then the actor's uninstall --purge)"
else
  fail "spent token" "rc=$RC left=[$(ls "$state/c" "$state/v")] out=$(tail -3 <<<"$OUT")"
fi
run_installer spent-then-new QUASAR_ENROLLMENT="qenr1.$FP.$(b64url 'wss://cp.example:8443').a-new-token" MOCK_AGENT_LOG="$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && grep -q 'a-new-token' "$state/seed.env" && grep -q 'enrolled: this host' <<<"$OUT"; then
  pass "then the regenerated command installs the machine from scratch"
else
  fail "regenerated command" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

reset_engine
run_installer enroll-then-reconnect QUASAR_ENROLLMENT="$WSS_BLOB" \
  MOCK_AGENT_LOG="$ENROLLED_LOG"$'\n''2026-09-25T10:00:02Z INFO quasar_node_agent::agent: reconnected as host 3f2c…'
if [ "$RC" -eq 0 ] && grep -q 'enrolled: this host' <<<"$OUT" && ! grep -q 'already enrolled' <<<"$OUT"; then
  pass "a fresh install whose agent reconnects right after enrolling reports the enrollment, not a saved identity"
else
  fail "enroll then reconnect" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
run_installer spent-installed QUASAR_ENROLLMENT="$WSS_BLOB"
if [ "$RC" -eq 1 ] && grep -q 'QUASAR_RESET_IDENTITY=1' <<<"$OUT" && nothing_started && [ -d "$state/c/quasar-node-agent" ]; then
  pass "a refused string on a machine installed earlier: nothing removed; names QUASAR_RESET_IDENTITY=1"
else
  fail "spent installed" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -3 <<<"$OUT")"
fi
run_installer reset "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
uninstall_at="$(grep -n ' uninstall --purge --confirm inst-0$' <<<"$DOCKER_LOG" | cut -d: -f1)"
if [ "$RC" -eq 0 ] && [ -n "$uninstall_at" ] && grep -q '^volume rm quasar-machine' <<<"$DOCKER_LOG" \
   && grep -q '^volume rm quasar-recovery-agent' <<<"$DOCKER_LOG" && started && grep -q 'enrolled' <<<"$OUT" \
   && ! grep -q 'left in place' <<<"$OUT" \
   && [ "$(grep -n '^rm -f quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" -lt "$uninstall_at" ] \
   && [ "$uninstall_at" -lt "$(grep -n '^run -d --name quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" ] \
   && grep -q 'io.quasar.installation=inst-1' "$state/v/quasar-agent-data/labels"; then
  pass "QUASAR_RESET_IDENTITY=1: the seed, then the actor's own uninstall --purge (agent, actor, volumes), the unlabelled socket volume, then a fresh install"
else
  fail "reset identity" "rc=$RC docker=[$(grep -E '^(rm|volume rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
# A host removed from the console: its actor and agent are gone, the seed idles on
# seed.json "uninstalled", and its volumes and machine state still name inst-0.
removed_machine() {
  reset_engine
  container quasar-seed running "/usr/local/bin/quasar-recovery seed"
  volume quasar-machine
  mkdir -p "$state/v/quasar-machine/files"
  printf '{\n  "format": 1,\n  "installation_id": "inst-0",\n  "role": "gpu"\n}\n' > "$state/v/quasar-machine/files/machine.json"
  printf '{"format":1,"state":"uninstalled"}\n' > "$state/v/quasar-machine/files/seed.json"
  printf '{"format":1,"by":"console"}\n' > "$state/v/quasar-machine/files/uninstalled.json"
  volume quasar-agent-data io.quasar.installation=inst-0
  volume quasar-node-agent-secrets io.quasar.installation=inst-0
  volume quasar-recovery-agent
}
removed_machine
run_installer re-add "${OK_ENV[@]}" QUASAR_NODE_NAME=gpu-host-4
uninstall_at="$(grep -n ' uninstall --purge --confirm inst-0$' <<<"$DOCKER_LOG" | cut -d: -f1)"
if [ "$RC" -eq 0 ] && [ -n "$uninstall_at" ] && grep -q 'added back' <<<"$OUT" && grep -q 'homes are kept' <<<"$OUT" \
   && [ "$(grep -n '^rm -f quasar-seed' <<<"$DOCKER_LOG" | head -n 1 | cut -d: -f1)" -lt "$uninstall_at" ] \
   && [ "$uninstall_at" -lt "$(grep -n '^run -d --name quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" ] \
   && grep -q '^QUASAR_NODE_NAME=gpu-host-4$' "$state/seed.env" && grep -q 'enrolled' <<<"$OUT" \
   && grep -q 'io.quasar.installation=inst-1' "$state/v/quasar-agent-data/labels" \
   && grep -q 'io.quasar.installation=inst-1' "$state/v/quasar-node-agent-secrets/labels"; then
  pass "after console removal, re-add: the old install is purged by its id from machine state (homes kept), then a fresh install under the same node name"
else
  fail "re-add after removal" "rc=$RC docker=[$(grep -E '^(rm|volume rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
removed_machine
volume quasar-old-data io.quasar.installation=inst-9
before="$(find "$state" -type f | sort | xargs cat | md5sum)"
run_installer re-add-stray "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'quasar-old-data (installation inst-9)' <<<"$OUT" && grep -q 'Nothing was removed' <<<"$OUT" \
   && nothing_started && ! grep -q ' uninstall ' <<<"$DOCKER_LOG" \
   && [ "$(find "$state" -type f | sort | xargs cat | md5sum)" = "$before" ]; then
  pass "re-add with a stray volume of another installation: refused, naming it; nothing removed or started"
else
  fail "re-add stray" "rc=$RC docker=[$(grep -E '^(rm|volume rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
removed_machine
volume quasar-final-dump io.quasar.installation=inst-9 io.quasar.helper=final-dump
run_installer re-add-final-dump "${OK_ENV[@]}" QUASAR_NODE_NAME=gpu-host-4
if [ "$RC" -eq 0 ] && grep -q ' uninstall --purge --confirm inst-0$' <<<"$DOCKER_LOG" && grep -q 'enrolled' <<<"$OUT" \
   && [ -d "$state/v/quasar-final-dump" ] && ! grep -q 'volume rm quasar-final-dump' <<<"$DOCKER_LOG"; then
  pass "re-add with an earlier installation's final dump on the machine: the dump is kept and not in the way"
else
  fail "re-add final dump" "rc=$RC docker=[$(grep -E '^(rm|volume rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
removed_machine
volume quasar-old-data io.quasar.installation=inst-9
mkdir -p "$state/v/quasar-machine/files"
printf '{"installation_id": "inst-0"}\n' > "$state/v/quasar-machine/files/machine.json"
run_installer reset-machine-state-first "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 1 ] && grep -q 'another Quasar installation than this one (inst-0)' <<<"$OUT" && ! grep -q ' uninstall ' <<<"$DOCKER_LOG"; then
  pass "machine state names the installation before any labelled volume: another's volume is never taken for it"
else
  fail "machine state first" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
removed_machine
rm -rf "$state/v/quasar-machine"
run_installer reset-volumes-only "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 0 ] && grep -q ' uninstall --purge --confirm inst-0$' <<<"$DOCKER_LOG" \
   && grep -q 'io.quasar.installation=inst-1' "$state/v/quasar-agent-data/labels"; then
  pass "reset with no labelled container left: the installation is named by its labelled volumes and purged"
else
  fail "reset volumes only" "rc=$RC docker=[$(grep -E '^(rm|volume rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
run_installer reset-old-actor "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1 MOCK_ACTOR_NO_UNINSTALL=1
if [ "$RC" -eq 0 ] && grep -q "mock.example/quasar/quasar-recovery@.* uninstall --purge --confirm inst-0$" <<<"$DOCKER_LOG" \
   && grep -q "$SEED_IMG uninstall --purge --confirm inst-0$" "$state/uninstalls" && grep -q 'enrolled' <<<"$OUT"; then
  pass "reset where the actor predates uninstall: the seed's image runs it instead"
else
  fail "reset old actor" "rc=$RC docker=[$(grep -E ' uninstall ' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
run_installer reset-uninstall-fails "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1 MOCK_ACTOR_NO_UNINSTALL=1 MOCK_UNINSTALL_OK=0
if [ "$RC" -eq 1 ] && grep -q 'uninstall of installation inst-0 did not finish' <<<"$OUT" \
   && grep -q "unrecognized subcommand 'uninstall'; ERROR uninstall: could not remove container quasar-node-agent: conflict" <<<"$OUT" \
   && ! grep -q '^run -d --name quasar-seed' <<<"$DOCKER_LOG"; then
  pass "a failed uninstall shows what each attempt said, and installs nothing"
else
  fail "uninstall fails" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
run_installer reset-reconnect-verdict "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1 \
  MOCK_AGENT_LOG='2026-09-25T10:00:02Z INFO quasar_node_agent::agent: reconnected as host 3f2c…'
if [ "$RC" -eq 0 ] && grep -q 'enrolled afresh' <<<"$OUT" && ! grep -q 'saved identity' <<<"$OUT"; then
  pass "after QUASAR_RESET_IDENTITY=1 the verdict says the host enrolled afresh, never that it kept a saved identity"
else
  fail "reset verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# The control plane's release trust rides the served script into the seed's inputs.
served_trust="$tmp/served-trust.sh"
trust_ns="$(grep -o "PINNED_ALLOWED_NAMESPACES='[^']*'" "$pins")"
trust_insecure="$(grep -o "PINNED_INSECURE_REGISTRIES='[^']*'" "$pins")"
sed -e "s|^PINNED_ALLOWED_NAMESPACES=''\$|$trust_ns|" -e "s|^PINNED_INSECURE_REGISTRIES=''\$|$trust_insecure|" "$served" > "$served_trust"
ns_value="$(sed -n 's/.*"allowed_namespaces": *"\([^"]*\)".*/\1/p' "$pins")"
insecure_value="$(sed -n 's/.*"insecure_registries": *"\([^"]*\)".*/\1/p' "$pins")"
reset_engine
SCRIPT="$served_trust" run_installer trust "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && [ -n "$ns_value" ] && grep -qxF "QUASAR_UPDATER_ALLOWED_NAMESPACES=$ns_value" "$state/seed.env" \
   && grep -qxF "QUASAR_PLATFORM_INSECURE_REGISTRIES=$insecure_value" "$state/seed.env" \
   && grep -qF "trusted:       $ns_value" <<<"$OUT"; then
  pass "the served release trust reaches the seed as the machine's QUASAR_UPDATER_ALLOWED_NAMESPACES and QUASAR_PLATFORM_INSECURE_REGISTRIES"
else
  fail "served trust" "rc=$RC env=[$(cat "$state/seed.env" 2>/dev/null)] out=$(tail -3 <<<"$OUT")"
fi
reset_engine
run_installer no-trust "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && ! grep -q 'QUASAR_UPDATER_ALLOWED_NAMESPACES\|QUASAR_PLATFORM_INSECURE_REGISTRIES' "$state/seed.env"; then
  pass "a control plane with no trust configured passes none: the seed keeps its defaults"
else
  fail "no trust" "rc=$RC env=[$(cat "$state/seed.env" 2>/dev/null)]"
fi

installed_machine "$ENROLLED_LOG"
container quasar-control-plane running "/usr/local/bin/quasar-control" io.quasar.installation=inst-0 io.quasar.platform-service=control-plane
run_installer reset-combined "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 1 ] && grep -q 'control plane' <<<"$OUT" && nothing_started; then
  pass "reset on a machine that runs the control plane: refused, nothing removed"
else
  fail "reset combined" "rc=$RC docker=[$(grep -E '^(rm|volume)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine "$ENROLLED_LOG"
volume quasar-postgres-data io.quasar.installation=inst-0 io.quasar.platform-service=postgres
run_installer reset-db-volume "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 1 ] && grep -q 'Quasar postgres' <<<"$OUT" && nothing_started && [ -d "$state/v/quasar-postgres-data" ]; then
  pass "reset where Quasar's Postgres data lives (its volume alone): refused, nothing removed"
else
  fail "reset db volume" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
container quasar-seed running "/usr/local/bin/quasar-recovery seed" com.docker.compose.project=quasar
run_installer reset-named-manager-seed "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 1 ] && grep -q "already has a seed, 'quasar-seed'" <<<"$OUT" && nothing_started && [ -d "$state/c/quasar-recovery" ]; then
  pass "a stack manager's seed named quasar-seed (the documented stack): refused, never replaced or removed"
else
  fail "named manager seed" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
container dockge-quasar-seed-1 running "/usr/local/bin/quasar-recovery seed"
run_installer reset-manager-seed "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 1 ] && grep -q "dockge-quasar-seed-1" <<<"$OUT" && nothing_started && [ -d "$state/c/quasar-recovery" ]; then
  pass "reset with a stack-manager seed on the machine: refused before anything is removed (that seed would re-create the actor)"
else
  fail "reset manager seed" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi

installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
container debug-shell running "/bin/sh"
echo quasar-recovery-agent > "$state/c/debug-shell/volumes"
run_installer reset-socket-in-use "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 0 ] && grep -q 'left in place: the volume quasar-recovery-agent, which debug-shell still mounts' <<<"$OUT" \
   && ! grep -q '^volume rm quasar-recovery-agent' <<<"$DOCKER_LOG" && [ -d "$state/c/debug-shell" ]; then
  pass "reset: the socket volume another container still mounts is kept, and the run says so"
else
  fail "reset socket in use" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(grep -i left <<<"$OUT")"
fi

reset_engine
volume quasar-recovery-agent
run_installer orphan-socket "${OK_ENV[@]}" MOCK_AGENT_LOG='ERROR control plane rejected register: auth_failed: authentication failed'
if [ "$RC" -eq 1 ] && ! grep -q 'Nothing was left' <<<"$OUT" && ! grep -qE '^(rm|volume rm) ' <<<"$DOCKER_LOG" \
   && [ -d "$state/v/quasar-recovery-agent" ]; then
  pass "a socket volume from before the run: not this run's to remove on a refused string"
else
  fail "orphan socket" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(tail -2 <<<"$OUT")"
fi

# Any trace of an installation already here: a refused string must not remove it.
reset_engine
volume quasar-agent-data io.quasar.installation=inst-0 io.quasar.platform-service=node-agent
run_installer not-fresh "${OK_ENV[@]}" MOCK_AGENT_LOG='ERROR control plane rejected register: auth_failed: authentication failed'
if [ "$RC" -eq 1 ] && grep -q 'QUASAR_RESET_IDENTITY=1' <<<"$OUT" && ! grep -q 'Nothing was left' <<<"$OUT" \
   && ! grep -qE '^(rm|volume rm) ' <<<"$DOCKER_LOG" && [ -d "$state/v/quasar-agent-data" ]; then
  pass "a labelled volume from before the run: the run is not the installer, and a refused string removes nothing"
else
  fail "not fresh" "rc=$RC docker=[$(grep -E '^(rm|volume rm)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi

reset_engine
run_installer live "${OK_ENV[@]}" QUASAR_NODE_NAME=gpu-host-2 MOCK_AGENT_LOG='ERROR control plane rejected register: auth_failed: a live agent is already registered under this node name; stop it before re-enrolling'
if [ "$RC" -eq 1 ] && grep -q "live agent is already registered as 'gpu-host-2'" <<<"$OUT" && engine_empty; then
  pass "a node name whose agent is live: named, and nothing left behind"
else
  fail "live refusal" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
reset_engine
run_installer pinfail "${OK_ENV[@]}" MOCK_AGENT_LOG='ERROR token="cp-tls-pin-mismatch" expected=… observed=…'
if [ "$RC" -eq 1 ] && grep -q "control plane's own page" <<<"$OUT"; then
  pass "pin mismatch: rc=1, create the command from the control plane's own page"
else
  fail "pin mismatch" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
reset_engine
run_installer actor-fails "${OK_ENV[@]}" MOCK_ACTOR_LOG='ERROR quasar_recovery: token="actor-resume-failed" owner_conflict: container quasar-node-agent (x) is not this installation'"'"'s'
if [ "$RC" -eq 1 ] && grep -q 'owner_conflict' <<<"$OUT" && engine_empty; then
  pass "the recovery actor cannot install: its line is shown, nothing left behind"
else
  fail "actor fails" "rc=$RC left=[$(ls "$state/c")] out=$(tail -3 <<<"$OUT")"
fi
reset_engine
run_installer seed-idle "${OK_ENV[@]}" MOCK_SEED_CREATES=0 \
  MOCK_SEED_STATUS='seed: idle: QUASAR_AGENT_IMAGE: the image declares no org.quasar.recipe revision; nothing was installed, and this seed stays idle until it is started again with corrected inputs (3 s ago)'
if [ "$RC" -eq 1 ] && grep -q 'the seed refused to install: QUASAR_AGENT_IMAGE' <<<"$OUT" && engine_empty; then
  pass "a seed idle on invalid inputs: its reason is the verdict, and the seed and its volume are taken away"
else
  fail "seed idle" "rc=$RC left=[$(ls "$state/c" "$state/v")] out=$(tail -3 <<<"$OUT")"
fi

# An interrupted run: the seed is there, no recovery actor yet.
reset_engine
container quasar-seed exited "/usr/local/bin/quasar-recovery seed"
run_installer resume-seed "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && [ "$(grep -n '^rm -f quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" -lt "$(grep -n '^run ' <<<"$DOCKER_LOG" | cut -d: -f1)" ] \
   && grep -q 'enrolled' <<<"$OUT"; then
  pass "interrupted before the actor existed: the earlier seed is replaced and the run completes"
else
  fail "resume seed" "rc=$RC docker=[$(grep -E '^(rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
# Interrupted between the seed's create and start of the actor, and the seed replaced since.
reset_engine
container quasar-seed running "/usr/local/bin/quasar-recovery seed"
container quasar-recovery created "/usr/local/bin/quasar-recovery actor" io.quasar.installation=inst-2 io.quasar.platform-service=recovery-actor
run_installer resume-actor QUASAR_ENROLLMENT="$WSS_BLOB" \
  MOCK_SEED_STATUS='seed: idle: quasar-recovery was created by another seed container (5eed) and never started; this seed does not start it (ADR 0007). Run docker start quasar-recovery (1 s ago)'
if grep -q '^start quasar-recovery$' <<<"$DOCKER_LOG" && ! grep -qE '^(run|rm) ' <<<"$DOCKER_LOG" && grep -q 'started the recovery actor' <<<"$OUT"; then
  pass "an actor created and never started: started once, which finishes that create; nothing else touched"
else
  fail "resume actor" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -3 <<<"$OUT")"
fi

reset_engine
container quasar-agent-quasar-node-agent-1 running "/usr/local/bin/quasar-node-agent-entrypoint" com.docker.compose.service=quasar-node-agent
run_installer legacy "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'Compose-installed node agent' <<<"$OUT" && grep -q 'compose --project-directory /opt/quasar-agent down' <<<"$OUT" && nothing_started; then
  pass "a machine still running the Compose-installed agent: refused with the removal command"
else
  fail "legacy stack" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
reset_engine
container dockge-quasar-seed-1 running "/usr/local/bin/quasar-recovery seed"
run_installer other-seed "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q "dockge-quasar-seed-1" <<<"$OUT" && nothing_started; then
  pass "a seed a stack manager started: refused, a second seed is never added"
else
  fail "other seed" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

reset_engine
run_installer timeout "${OK_ENV[@]}" MOCK_AGENT_LOG=''
if [ "$RC" -eq 3 ] && grep -q 'still connecting' <<<"$OUT" && grep -q 'docker logs -f quasar-node-agent' <<<"$OUT" && ! engine_empty; then
  pass "no verdict within the window: rc=3 with the follow command; the install is kept"
else
  fail "timeout" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

reset_engine
mk_root "$tmp/root"; printf '1\n' > "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
FIX=1 run_installer dry-with-fix "${OK_ENV[@]}" QUASAR_ENROLL_DRY_RUN=1
if [ "$RC" -eq 0 ] && grep -q 'Fix: sysctl -w kernel.apparmor_restrict_unprivileged_userns=0' <<<"$OUT" && [ -z "$FIX_LOG" ] && nothing_started; then
  pass "dry run with a failing check: prints the fix and applies nothing, even with QUASAR_ENROLL_FIX=1"
else
  fail "dry run fix" "rc=$RC fix=[$FIX_LOG] out=$(tail -4 <<<"$OUT")"
fi
reset_engine
EXTRA_ARGS="-s -- --fix-only" run_installer fix-only
if [ "$RC" -eq 0 ] && grep -q 'sysctl -w kernel.apparmor_restrict_unprivileged_userns=0' <<<"$FIX_LOG" \
   && [ "$(cat "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns")" = 0 ] \
   && grep -q 'host prepared' <<<"$OUT" && nothing_started && ! grep -q '^pull' <<<"$DOCKER_LOG"; then
  pass "--fix-only: no enrollment string needed; the host's fixes are applied, nothing pulled or started"
else
  fail "fix only" "rc=$RC fix=[$FIX_LOG] docker=[$DOCKER_LOG] out=$(tail -4 <<<"$OUT")"
fi
run_installer fix-only-dry QUASAR_ENROLL_FIX_ONLY=1 QUASAR_ENROLL_DRY_RUN=1
if [ "$RC" -eq 2 ] && grep -q 'use one' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "--fix-only with a dry run: refused as contradictory"
else
  fail "fix only dry" "rc=$RC out=$(tail -2 <<<"$OUT")"
fi
mk_root "$tmp/root"; reset_engine
run_installer dry "${OK_ENV[@]}" QUASAR_ENROLL_DRY_RUN=1
if [ "$RC" -eq 0 ] && grep -q 'dry run' <<<"$OUT" && grep -q "seed image:    $SEED_IMG" <<<"$OUT" && nothing_started; then
  pass "dry run: prints the plan, pulls and starts nothing"
else
  fail "dry run" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -4 <<<"$OUT")"
fi

# ── 7. rendering ─────────────────────────────────────────────────────────────
esc="$(printf '\033')"
mk_root "$tmp/root"; reset_engine
STYLE="tty" run_installer tty-ok "${OK_ENV[@]}" LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
if [ "$RC" -eq 0 ] && grep -q "${esc}\[32m✔${esc}\[0m engine: Docker, rootful (/var/run/docker.sock)" <<<"$OUT" \
   && grep -q "✔${esc}\[0m enrolled: this host is now 'gpu-b'" <<<"$OUT" \
   && grep -q "${esc}\[1m==> Host checks" <<<"$OUT"; then
  pass "tty: bold steps, green ticks"
else
  fail "tty rendering" "rc=$RC out=$(cat -v <<<"$OUT" | tail -5)"
fi
if ! sed 's/.*\r//' <<<"$OUT" | grep -q '[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]'; then
  pass "tty: no spinner frame survives on any line"
else
  fail "tty: stray spinner" "$(sed 's/.*\r//' <<<"$OUT" | grep '[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]' | head -3 | cat -v)"
fi
reset_engine
run_installer plain-ok "${OK_ENV[@]}"
stripped="$(sed 's/.*\r//' "$tmp/tty-ok.out" | sed -E "s/${esc}\[[0-9;]*[mK]//g; s/^  ✔ /  /" | grep -v '^$')"
if diff <(grep -v '^$' <<<"$OUT") <(printf '%s\n' "$stripped") >/dev/null; then
  pass "tty carries exactly the plain facts, decorated"
else
  fail "tty/plain facts differ" "$(diff <(grep -v '^$' <<<"$OUT") <(printf '%s\n' "$stripped") | head -12 | cat -v)"
fi
reset_engine
STYLE="tty" run_installer tty-ascii "${OK_ENV[@]}" LANG=C LC_ALL=C
if [ "$RC" -eq 0 ] && grep -q '\[ok\]' <<<"$OUT" && ! grep -q '✔' <<<"$OUT"; then
  pass "tty without a UTF-8 locale: ASCII glyphs"
else
  fail "tty ascii" "$(tail -3 <<<"$OUT" | cat -v)"
fi

# ── 8. the app-container AppArmor profile (#76) ──────────────────────────────
# The workstation's real apparmor_parser is shadowed by the stub for every run.
if diff -q <(sh "$script" --print-apparmor-profile) "$root/deploy/apparmor/quasar-app" >/dev/null; then
  pass "--print-apparmor-profile is byte-identical to deploy/apparmor/quasar-app"
else
  fail "apparmor profile drift" "$(diff <(sh "$script" --print-apparmor-profile) "$root/deploy/apparmor/quasar-app" | head -8)"
fi

mk_root "$tmp/root"; reset_engine
run_installer aa-absent "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && [ -z "$AA_LOG" ] && ! grep -qi 'apparmor' <<<"$OUT"; then
  pass "host without AppArmor: apparmor_parser never called"
else
  fail "apparmor on a non-apparmor host" "aa=[$AA_LOG] $(grep -i apparmor <<<"$OUT" | head -3)"
fi

mk_root "$tmp/root"; reset_engine
mkdir -p "$tmp/root/sys/module/apparmor/parameters" "$tmp/root/sys/kernel/security/apparmor"
printf 'Y\n' > "$tmp/root/sys/module/apparmor/parameters/enabled"
printf 'docker-default (enforce)\n' > "$tmp/root/sys/kernel/security/apparmor/profiles"
run_installer aa-load "${OK_ENV[@]}" MOCK_AA_COPY="$tmp/aa-loaded"
if [ "$RC" -eq 0 ] && grep -q '^-r -W /' <<<"$AA_LOG" && diff -q "$tmp/aa-loaded" "$root/deploy/apparmor/quasar-app" >/dev/null \
   && [ ! -e "${AA_LOG#-r -W }" ] && grep -q 'loaded the quasar-app AppArmor profile' <<<"$OUT" \
   && [ ! -e "$tmp/root/etc/apparmor.d/quasar-app" ]; then
  pass "AppArmor host: the profile is loaded from a temporary file that does not outlive the run; not persisted by default"
else
  fail "apparmor load" "rc=$RC aa=[$AA_LOG] out=$(grep -i apparmor <<<"$OUT" | head -3)"
fi
reset_engine
run_installer aa-persist "${OK_ENV[@]}" QUASAR_ENROLL_APPARMOR_PERSIST=1
if [ "$RC" -eq 0 ] && diff -q "$tmp/root/etc/apparmor.d/quasar-app" "$root/deploy/apparmor/quasar-app" >/dev/null \
   && [ "$(stat -c %a "$tmp/root/etc/apparmor.d/quasar-app")" = 644 ] && grep -q 'persisted /etc/apparmor.d/quasar-app' <<<"$OUT"; then
  pass "QUASAR_ENROLL_APPARMOR_PERSIST=1: the profile is also installed 0644 in /etc/apparmor.d"
else
  fail "apparmor persist" "rc=$RC $(grep -i apparmor <<<"$OUT" | head -3)"
fi
printf 'quasar-app (enforce)\n' >> "$tmp/root/sys/kernel/security/apparmor/profiles"
installed_machine "$ENROLLED_LOG"
run_installer aa-loaded QUASAR_ENROLLMENT="$WSS_BLOB"
if [ "$RC" -eq 0 ] && [ -z "$AA_LOG" ] && grep -q 'already loaded' <<<"$OUT"; then
  pass "a profile already loaded is left alone"
else
  fail "apparmor already loaded" "rc=$RC aa=[$AA_LOG]"
fi

# --help carries what the script documents and stops at its marker.
piped_help="$(sh -s -- --help < "$script")"
if grep -q 'QUASAR_ENROLL_FIX_ONLY' <<<"$piped_help" && grep -q -- '--pinnedpubkey' <<<"$piped_help"; then
  pass "--help prints when the script is piped into sh, as curl | sh -s -- --help does"
else
  fail "piped help" "$(head -3 <<<"$piped_help")"
fi
help_out="$(sh "$script" --help)"
if grep -q 'QUASAR_RESET_IDENTITY' <<<"$help_out" && grep -q 'QUASAR_ENROLL_FIX' <<<"$help_out" && grep -q 'QUASAR_TEMPLATE_ROOT' <<<"$help_out" \
   && grep -q -- '--pinnedpubkey' <<<"$help_out" && ! grep -q 'end-of-help' <<<"$help_out" && ! grep -q 'set -eu' <<<"$help_out"; then
  pass "--help carries the knobs and the --pinnedpubkey/-k pairing, and stops at its marker"
else
  fail "help text" "$(tail -5 <<<"$help_out")"
fi

# ── 9. engines and modes (RH-07, #406) ───────────────────────────────────────
ME="$(id -un)"
xdg="$tmp/xdg"
mk_xdg() { # mk_xdg [docker] [podman]: a runtime directory holding those rootless sockets
  rm -rf "$xdg"; mkdir -p "$xdg"
  local s; for s in "$@"; do
    case "$s" in
      docker) : > "$xdg/docker.sock" ;;
      podman) mkdir -p "$xdg/podman"; : > "$xdg/podman/podman.sock" ;;
    esac
  done
}
os_release() { printf '%s\n' "$@" > "$tmp/root/etc/os-release"; }
FEDORA=(ID=fedora VERSION_ID=43 'PRETTY_NAME="Fedora Linux 43 (Workstation Edition)"')
# What prepare-host.sh leaves on a rootless host, for the account running the tests.
prepared() { # prepared [podman]
  local r="$tmp/root"
  mkdir -p "$r/var/lib/systemd/linger" "$r/etc/udev/rules.d" "$r/etc/tmpfiles.d" "$r/var/lib/quasar/homes" "$r/var/lib/quasar/templates"
  : > "$r/var/lib/systemd/linger/$ME"
  printf '%s:524288:65536\n' "$ME" > "$r/etc/subuid"
  : > "$r/etc/udev/rules.d/70-quasar.rules"
  : > "$r/etc/tmpfiles.d/quasar.conf"
  printf '%s:x:%s:%s:Quasar:/home/%s:/bin/bash\n' "$ME" "$(id -u)" "$(id -g)" "$ME" > "$r/etc/passwd"
  if [ "${1:-}" = podman ]; then
    mkdir -p "$r/home/$ME/.config/systemd/user/default.target.wants"
    ln -sf /usr/lib/systemd/user/podman-restart.service "$r/home/$ME/.config/systemd/user/default.target.wants/podman-restart.service"
  fi
}
rootful_podman_root() { # the rootful Podman socket, podman-restart and the runtime directory, no Docker socket
  rm -f "$tmp/root/var/run/docker.sock"
  mkdir -p "$tmp/root/run/podman" "$tmp/root/etc/systemd/system/default.target.wants" "$tmp/root/etc/tmpfiles.d"
  : > "$tmp/root/run/podman/podman.sock"
  printf 'd /run/quasar-agent 0755 root root -\n' > "$tmp/root/etc/tmpfiles.d/quasar.conf"
  ln -sf /usr/lib/systemd/system/podman-restart.service "$tmp/root/etc/systemd/system/default.target.wants/podman-restart.service"
}
only_cli() { ! grep -qv "^$1 " <<<"$CLI_LOG" && [ -n "$CLI_LOG" ]; }
seed_run() { grep '^run -d --name quasar-seed' <<<"$DOCKER_LOG" || true; }

# Rootless Podman on Fedora: found as the user, never through sudo, the seed given the
# user's socket, and the profile is experimental (a warning, nothing blocked).
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg podman; reset_engine
XDG="$xdg" run_installer podman-rootless "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && [ -z "$SUDO_LOG" ] && only_cli podman \
   && grep -q -- "-v $xdg/podman/podman.sock:/var/run/docker.sock -v quasar-machine:/var/lib/quasar-machine:ro --env-file " <<<"$(seed_run)" \
   && grep -q "engine: Podman, rootless ($xdg/podman/podman.sock)" <<<"$OUT" \
   && grep -q 'engine profile: Podman rootless on Fedora (Fedora Linux 43 (Workstation Edition)): experimental' <<<"$OUT" \
   && grep -q 'host preparation: done' <<<"$OUT" && [ "$(cat "$state/seed.env.mode")" = 600 ] \
   && ! grep -q 'CONTAINER_HOST=[^ ]' <<<"$CLI_LOG" && grep -q 'podman logs quasar-node-agent' <<<"$OUT" \
   && grep -q "enrolled: this host is now 'gpu-b'" <<<"$OUT"; then
  pass "rootless Podman: its user socket, the podman CLI only, never sudo; the seed gets that socket at /var/run/docker.sock and a 0600 env file; experimental is a warning"
else
  fail "rootless podman" "rc=$RC sudo=[$SUDO_LOG] cli=[$(sort -u <<<"$CLI_LOG" | head -3)] run=[$(seed_run)] out=$(tail -6 <<<"$OUT")"
fi

# Rootless Docker: DOCKER_HOST points the CLI at the user's daemon, never sudo.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared; mk_xdg docker; reset_engine
XDG="$xdg" run_installer docker-rootless "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && [ -z "$SUDO_LOG" ] && only_cli docker \
   && ! grep -qv "DOCKER_HOST=unix://$xdg/docker.sock " <<<"$CLI_LOG" \
   && grep -q -- "-v $xdg/docker.sock:/var/run/docker.sock " <<<"$(seed_run)" \
   && grep -q 'engine: Docker, rootless' <<<"$OUT"; then
  pass "rootless Docker: every docker call carries DOCKER_HOST=unix://\$XDG_RUNTIME_DIR/docker.sock, never sudo; the seed gets that socket"
else
  fail "rootless docker" "rc=$RC sudo=[$SUDO_LOG] cli=[$(sort -u <<<"$CLI_LOG" | head -3)] run=[$(seed_run)] out=$(tail -5 <<<"$OUT")"
fi

# Rootful Podman: its system socket through sudo -n, the podman CLI.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; rootful_podman_root; reset_engine
run_installer podman-rootful "${OK_ENV[@]}" MOCK_ROOTLESS=0
if [ "$RC" -eq 0 ] && grep -q '^-n podman ' <<<"$SUDO_LOG" && only_cli podman \
   && grep -q -- "-v /run/podman/podman.sock:/var/run/docker.sock " <<<"$(seed_run)" \
   && grep -q 'engine: Podman, rootful (/run/podman/podman.sock)' <<<"$OUT"; then
  pass "rootful Podman: /run/podman/podman.sock through sudo -n; the seed gets that socket"
else
  fail "rootful podman" "rc=$RC sudo=[$(head -2 <<<"$SUDO_LOG")] run=[$(seed_run)] out=$(tail -5 <<<"$OUT")"
fi

# Rootful Docker is the path every earlier section ran; said explicitly here.
mk_root "$tmp/root"; reset_engine
run_installer docker-rootful "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && grep -q '^-n docker ' <<<"$SUDO_LOG" && only_cli docker \
   && grep -q -- "-v /var/run/docker.sock:/var/run/docker.sock " <<<"$(seed_run)" \
   && grep -q 'engine profile: Docker rootful on Ubuntu 24.04: supported' <<<"$OUT" && ! grep -q 'host preparation' <<<"$OUT"; then
  pass "rootful Docker: /var/run/docker.sock through sudo -n, supported, and no host-preparation check (D4: today's checks)"
else
  fail "rootful docker" "rc=$RC out=$(tail -5 <<<"$OUT")"
fi

# Both engines: the operator says which.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg docker podman; reset_engine
XDG="$xdg" run_installer both-rootless "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 2 ] && grep -q 'both Docker' <<<"$OUT" && grep -q 'QUASAR_ENGINE=podman' <<<"$OUT" && [ -z "$DOCKER_LOG" ] && [ -z "$SUDO_LOG" ]; then
  pass "both rootless engines: refused until QUASAR_ENGINE says which; no engine touched"
else
  fail "both rootless" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -3 <<<"$OUT")"
fi
XDG="$xdg" run_installer both-rootless-chosen "${OK_ENV[@]}" MOCK_ROOTLESS=1 QUASAR_ENGINE=podman
if [ "$RC" -eq 0 ] && only_cli podman && grep -q -- "-v $xdg/podman/podman.sock:" <<<"$(seed_run)"; then
  pass "both rootless engines with QUASAR_ENGINE=podman: Podman's socket"
else
  fail "both rootless chosen" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; rootful_podman_root; : > "$tmp/root/var/run/docker.sock"; reset_engine
run_installer both-rootful "${OK_ENV[@]}"
if [ "$RC" -eq 2 ] && grep -q 'both Docker (/var/run/docker.sock) and Podman (/run/podman/podman.sock) running rootful' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "both rootful engines: refused until QUASAR_ENGINE says which"
else
  fail "both rootful" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
rm -f "$tmp/root/var/run/docker.sock"; ln -s ../../run/podman/podman.sock "$tmp/root/var/run/docker.sock"; reset_engine
run_installer podman-docker "${OK_ENV[@]}"
if [ "$RC" -eq 0 ] && only_cli podman && grep -q 'engine: Podman, rootful' <<<"$OUT"; then
  pass "podman-docker's docker.sock is Podman's own socket: one engine, Podman"
else
  fail "podman-docker link" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# Rootless first for a user; only then the rootful sockets through sudo.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg podman; reset_engine
XDG="$xdg" run_installer rootless-first "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && [ -z "$SUDO_LOG" ] && only_cli podman; then
  pass "a user with a rootless engine and a rootful Docker socket on the machine: the rootless engine, never sudo"
else
  fail "rootless first" "rc=$RC sudo=[$SUDO_LOG]"
fi
XDG="$xdg" run_installer rootless-first-docker "${OK_ENV[@]}" QUASAR_ENGINE=docker
if [ "$RC" -eq 0 ] && only_cli docker && grep -q '^-n docker ' <<<"$SUDO_LOG" && grep -q 'engine: Docker, rootful' <<<"$OUT"; then
  pass "QUASAR_ENGINE=docker past a rootless Podman: the rootful Docker socket, through sudo"
else
  fail "rootless first, docker chosen" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# The engine's report decides the mode, and disagreeing with the socket stops.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared; mk_xdg docker; reset_engine
XDG="$xdg" run_installer mode-mismatch "${OK_ENV[@]}" MOCK_ROOTLESS=0
if [ "$RC" -eq 1 ] && grep -q 'reports that it runs rootful' <<<"$OUT" && nothing_started && [ -z "$SUDO_LOG" ]; then
  pass "a rootless socket whose engine reports rootful: refused, nothing started"
else
  fail "mode mismatch" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# DOCKER_HOST names the socket; only a local one will do.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared; mk_xdg; reset_engine
mkdir -p "$tmp/other-run"; : > "$tmp/other-run/docker.sock"
run_installer docker-host "${OK_ENV[@]}" DOCKER_HOST="unix://$tmp/other-run/docker.sock"
if [ "$RC" -eq 0 ] && ! grep -qv "DOCKER_HOST=unix://$tmp/other-run/docker.sock " <<<"$CLI_LOG" \
   && grep -q -- "-v $tmp/other-run/docker.sock:/var/run/docker.sock " <<<"$(seed_run)"; then
  pass "DOCKER_HOST=unix://…: that socket for the CLI and for the seed"
else
  fail "docker host" "rc=$RC cli=[$(head -2 <<<"$CLI_LOG")] out=$(tail -3 <<<"$OUT")"
fi
run_installer docker-host-tcp "${OK_ENV[@]}" DOCKER_HOST=tcp://10.0.0.1:2375
if [ "$RC" -eq 2 ] && grep -q 'not a local unix:// socket' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "DOCKER_HOST=tcp://…: refused, the seed needs a socket on this machine"
else
  fail "docker host tcp" "rc=$RC out=$(tail -2 <<<"$OUT")"
fi
mk_root "$tmp/root"; rm -f "$tmp/root/var/run/docker.sock"; reset_engine
run_installer no-engine "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'no container engine found' <<<"$OUT" && grep -q 'podman.socket' <<<"$OUT" && [ -z "$DOCKER_LOG" ] \
   && { [ "$(id -u)" -eq 0 ] || grep -q 'run this command as root' <<<"$OUT"; }; then
  pass "no engine socket anywhere: named, with where it looked, Podman's socket units, and (not root) that rootful Podman needs root"
else
  fail "no engine" "rc=$RC out=$(tail -2 <<<"$OUT")"
fi

# Missing host preparation on a rootless engine: what is missing, where the steps are, and nothing else.
NOFP_BLOB="qenr1..$(b64url 'wss://cp.example:8443/').$TOKEN"
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; mk_xdg podman; reset_engine
XDG="$xdg" run_installer unprepared QUASAR_ENROLLMENT="$NOFP_BLOB" MOCK_ROOTLESS=1 FIX=1
if [ "$RC" -eq 1 ] && grep -qF 'https://accreleus.github.io/quasar/install/rootless/' <<<"$OUT" \
   && ! grep -q 'prepare-host' <<<"$OUT" && ! grep -q 'curl ' <<<"$OUT" \
   && grep -q "lingering for $ME" <<<"$OUT" && grep -q '70-quasar.rules' <<<"$OUT" && grep -q 'tmpfiles.d/quasar.conf' <<<"$OUT" \
   && grep -q 'podman-restart.service' <<<"$OUT" && grep -q '/etc/subuid' <<<"$OUT" \
   && nothing_started && ! grep -q '^pull' <<<"$DOCKER_LOG" && [ -z "$SUDO_LOG" ] && [ -z "$FIX_LOG" ]; then
  pass "rootless without host preparation: names what is missing, points at the rootless steps (no script download), and stops (QUASAR_ENROLL_FIX=1 applies nothing)"
else
  fail "unprepared" "rc=$RC sudo=[$SUDO_LOG] out=$(tail -8 <<<"$OUT")"
fi
XDG="$xdg" run_installer unprepared-pinned "${OK_ENV[@]}" MOCK_ROOTLESS=1 QUASAR_HOME_ROOT=/srv/q/homes
if [ "$RC" -eq 1 ] && grep -q 'the homes root /srv/q/homes' <<<"$OUT" && grep -q 'the templates root /srv/q/templates' <<<"$OUT" \
   && ! grep -q 'prepare-host' <<<"$OUT" && nothing_started; then
  pass "a pinned control plane: the same pointer, no script download; the homes follow QUASAR_HOME_ROOT"
else
  fail "unprepared pinned" "rc=$RC out=$(tail -6 <<<"$OUT")"
fi
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; rootful_podman_root; rm "$tmp/root/etc/systemd/system/default.target.wants/podman-restart.service"; reset_engine
run_installer unprepared-rootful-podman "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -qxF '    systemctl enable podman-restart.service' <<<"$OUT" && ! grep -q 'prepare-host' <<<"$OUT" && nothing_started; then
  pass "rootful Podman without podman-restart enabled: the commands to run, nothing started"
else
  fail "unprepared rootful podman" "rc=$RC out=$(tail -4 <<<"$OUT")"
fi
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; rootful_podman_root; rm "$tmp/root/etc/tmpfiles.d/quasar.conf"; reset_engine
run_installer unprepared-rootful-podman-rundir "${OK_ENV[@]}"
if [ "$RC" -eq 1 ] && grep -q 'tmpfiles.d/quasar.conf' <<<"$OUT" \
   && grep -qxF "    echo 'd /run/quasar-agent 0755 root root -' > /etc/tmpfiles.d/quasar.conf" <<<"$OUT" && nothing_started; then
  pass "rootful Podman without the boot-time runtime directory: named with its tmpfiles line (Podman never creates a missing bind source), nothing started"
else
  fail "unprepared rootful podman rundir" "rc=$RC out=$(tail -6 <<<"$OUT")"
fi

# Rootless: a failing check's fix is printed as root's, never applied; AppArmor warns.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg podman; reset_engine
printf '1\n' > "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
XDG="$xdg" run_installer rootless-fix "${OK_ENV[@]}" MOCK_ROOTLESS=1 FIX=1
if [ "$RC" -eq 1 ] && grep -q 'Fix it as root' <<<"$OUT" && grep -q 'sysctl -w kernel.apparmor_restrict_unprivileged_userns=0' <<<"$OUT" \
   && [ -z "$FIX_LOG" ] && [ -z "$SUDO_LOG" ] && nothing_started; then
  pass "rootless with a failing host check: the fix is printed for root, never applied, even with QUASAR_ENROLL_FIX=1"
else
  fail "rootless fix" "rc=$RC fix=[$FIX_LOG] sudo=[$SUDO_LOG] out=$(tail -4 <<<"$OUT")"
fi
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg podman; reset_engine
mkdir -p "$tmp/root/sys/module/apparmor/parameters"; printf 'Y\n' > "$tmp/root/sys/module/apparmor/parameters/enabled"
XDG="$xdg" run_installer rootless-apparmor "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && [ -z "$AA_LOG" ] && [ -z "$SUDO_LOG" ] && grep -q 'WARN: the quasar-app AppArmor profile is not loaded' <<<"$OUT"; then
  pass "rootless on an AppArmor host: the profile load is a warning with the root command; apparmor_parser never runs"
else
  fail "rootless apparmor" "rc=$RC aa=[$AA_LOG] out=$(grep -i apparmor <<<"$OUT" | head -2)"
fi

# Re-runs on Podman: an installed machine is left alone, a leftover seed replaced.
mk_root "$tmp/root"; os_release "${FEDORA[@]}"; prepared podman; mk_xdg podman
installed_machine 'INFO quasar_node_agent::agent: reconnected as host 3f2c…'
before="$(find "$state" -type f | sort | xargs cat | md5sum)"
XDG="$xdg" run_installer podman-rerun QUASAR_ENROLLMENT="$WSS_BLOB" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && nothing_started && [ "$(find "$state" -type f | sort | xargs cat | md5sum)" = "$before" ] \
   && grep -q 'Already installed' <<<"$OUT" && [ -z "$SUDO_LOG" ]; then
  pass "rootless Podman re-run on an installed machine: nothing pulled, started, removed or changed"
else
  fail "podman rerun" "rc=$RC docker=[$(grep -E '^(run|pull|start|rm)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
reset_engine
container quasar-seed exited "/usr/local/bin/quasar-recovery seed"
XDG="$xdg" run_installer podman-resume-seed "${OK_ENV[@]}" MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && [ "$(grep -n '^rm -f quasar-seed' <<<"$DOCKER_LOG" | cut -d: -f1)" -lt "$(grep -n '^run -d' <<<"$DOCKER_LOG" | cut -d: -f1)" ] \
   && grep -q 'replaced the seed' <<<"$OUT" && grep -q 'enrolled' <<<"$OUT"; then
  pass "rootless Podman after an interrupted run: the leftover seed is replaced and the run completes"
else
  fail "podman resume seed" "rc=$RC docker=[$(grep -E '^(rm|run)' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi
installed_machine 'ERROR control plane rejected register: auth_failed: authentication failed'
XDG="$xdg" run_installer podman-reset "${OK_ENV[@]}" QUASAR_RESET_IDENTITY=1 MOCK_ROOTLESS=1
if [ "$RC" -eq 0 ] && grep -q "^run --rm --security-opt label=disable -v $xdg/podman/podman.sock:/var/run/docker.sock .* uninstall --purge --confirm inst-0\$" <<<"$DOCKER_LOG" \
   && grep -q 'enrolled' <<<"$OUT" && [ -z "$SUDO_LOG" ]; then
  pass "rootless Podman reset: the actor's uninstall is given the engine's own socket"
else
  fail "podman reset" "rc=$RC docker=[$(grep ' uninstall ' <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi

# Every platform sample in the table reads as its platform, as the agent reads it.
while IFS=$'\t' read -r want_platform want_label sample_name os_lines; do
  mk_root "$tmp/root"; prepared podman; mk_xdg podman; reset_engine
  printf '%b' "$os_lines" > "$tmp/root/etc/os-release"
  XDG="$xdg" run_installer "sample-$want_platform" "${OK_ENV[@]}" MOCK_ROOTLESS=1 QUASAR_ENROLL_DRY_RUN=1
  if grep -q "engine profile: Podman rootless on $want_label" <<<"$OUT"; then
    pass "os-release of $sample_name reads as $want_label"
  else
    fail "platform of $sample_name" "want $want_label: $(grep 'engine profile' <<<"$OUT")"
  fi
done < <(python3 - "$profiles" <<'PY_SAMPLES'
import json, sys
t = json.load(open(sys.argv[1]))
for pid, p in t['platforms'].items():
    for s in p['samples']:
        print('\t'.join([pid, p['label'], s['name'], '\\n'.join(s['osRelease']) + '\\n']))
PY_SAMPLES
)

# Every unsupported row is refused by name, with its alternatives, before any pull.
while IFS=$'\t' read -r u_platform u_label u_engine u_engine_label u_mode u_alts os_lines; do
  mk_root "$tmp/root"; reset_engine; mk_xdg
  printf '%b' "$os_lines" > "$tmp/root/etc/os-release"
  rootless=0
  case "$u_engine/$u_mode" in
    */rootless) mk_xdg "$u_engine"; prepared "$u_engine"; rootless=1 ;;
    podman/rootful) rootful_podman_root ;;
  esac
  XDG="$xdg" run_installer "unsupported-$u_platform-$u_engine-$u_mode" "${OK_ENV[@]}" MOCK_ROOTLESS="$rootless"
  alts_ok=1
  IFS=';' read -ra alt_list <<<"$u_alts"
  for a in "${alt_list[@]}"; do grep -qF "$a" <<<"$OUT" || alts_ok=0; done
  if [ "$RC" -eq 1 ] && grep -qF "engine profile: $u_engine_label $u_mode on $u_label" <<<"$OUT" && grep -q 'is unsupported' <<<"$OUT" \
     && [ "$alts_ok" = 1 ] && ! grep -qE '^(pull|run|start|rm) ' <<<"$DOCKER_LOG" && ! grep -qv '^info ' <<<"$DOCKER_LOG" \
     && { [ "$rootless" = 0 ] || [ -z "$SUDO_LOG" ]; }; then
    pass "unsupported: $u_engine_label $u_mode on $u_label is refused by name with its alternatives; the engine was only asked for its info"
  else
    fail "unsupported $u_platform/$u_engine/$u_mode" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -2 <<<"$OUT")"
  fi
done < <(python3 - "$profiles" <<'PY_ROWS'
import json, sys
t = json.load(open(sys.argv[1]))
P, E = t['platforms'], t['engines']
def status(pl, e, m):
    return next(r['status'] for r in t['profiles'] if (r['platform'], r['engine'], r['mode']) == (pl, e, m))
for r in t['profiles']:
    if r['status'] != 'unsupported':
        continue
    alts = []
    for a in r['alternatives']:
        to = a.get('platform', r['platform'])
        where = 'on this machine' if to == r['platform'] else 'on ' + P[to]['label']
        alts.append('%s %s %s (%s)' % (E[a['engine']]['label'], a['mode'], where, status(to, a['engine'], a['mode'])))
    os_lines = '\\n'.join(P[r['platform']]['samples'][0]['osRelease']) + '\\n'
    print('\t'.join([r['platform'], P[r['platform']]['label'], r['engine'], E[r['engine']]['label'], r['mode'], ';'.join(alts), os_lines]))
PY_ROWS
)

# ── 10. the engine-profile table is profiles.json ────────────────────────────
if diff <(embedded_profiles) <(generate_profiles) >/dev/null; then
  pass "deploy/enroll-host.sh's engine-profile table is testdata/engine-profiles/profiles.json, row for row"
else
  fail "engine-profile table drift" "run: bash deploy/test-enroll-host.sh --write-profiles — $(diff <(embedded_profiles) <(generate_profiles) | head -6)"
fi

# Every run above must have kept the token off stdout/stderr.
for f in "$tmp"/*.out; do
  if grep -qF "$TOKEN" "$f"; then fail "token leaked to output in $(basename "$f")"; fi
done

printf '\n%d passed, %d failed\n' "$PASS_N" "$FAIL_N"
[ "$FAIL_N" -eq 0 ]
