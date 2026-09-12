#!/usr/bin/env bash
# Offline contract tests for deploy/enroll-host.sh — the one-line second-host
# installer (#100). A mock docker on PATH records every invocation and a fake
# root directory stands in for /proc, /sys, /dev and /etc/os-release, so these
# assert the installer's contract without a daemon, a GPU or a network:
#
#   1. the enrollment string is required, parsed, and never allowed to point the
#      agent at a cleartext ws:// control plane;
#   2. the token appears in no docker argv and no stdout line — it reaches the
#      agent only through the 0600 env file;
#   3. the host preflights that mirror node-agent/src/readiness.rs refuse BEFORE
#      anything is written or started, naming the fix (#76's AppArmor sysctl);
#   4. re-running updates the one installed agent instead of adding a second;
#   5. the image is pinned from the ref the script was fetched at;
#   6. the quasar-app AppArmor profile is written and loaded on an AppArmor host,
#      and on no other (#76);
#   7. the saved agent identity (the project's quasar-agent-data volume) is left
#      alone by default and cleared only on request (#199).
#
# Run: bash deploy/test-enroll-host.sh
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/deploy/enroll-host.sh"
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
CA_BLOB="qenr1..$(b64url 'wss://play.example.com').$TOKEN"

# A fake host root. Defaults describe a healthy AMD box on a Debian-family
# distro; tests mutate it.
mk_root() { # mk_root <dir> [vendor]
  local r="$1" vendor="${2:-0x1002}"
  rm -rf "$r"
  mkdir -p "$r/sys/class/drm/renderD128/device" "$r/dev/dri" "$r/proc/sys/kernel" "$r/proc/sys/user" "$r/etc"
  printf '%s\n' "$vendor" > "$r/sys/class/drm/renderD128/device/vendor"
  : > "$r/dev/dri/renderD128"
  : > "$r/dev/uinput"
  printf '15000\n' > "$r/proc/sys/user/max_user_namespaces"
  printf 'ID=ubuntu\nVERSION_ID="24.04"\n' > "$r/etc/os-release"
}

# The mock docker. Records argv, answers just enough of compose/pull/inspect
# for the installer to reach its verdict.
mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
case "$1" in
  info) exit 0 ;;
  pull) [ "${MOCK_PULL_OK:-1}" = 1 ] || { echo "mock: pull refused: $2" >&2; exit 1; }; exit 0 ;;
  image)
    [ "$2" = inspect ] || exit 99
    [ "${MOCK_LOCAL_IMAGE:-0}" = 1 ] || { echo "mock: no such image" >&2; exit 1; }
    exit 0 ;;
  volume)
    case "$2" in
      inspect) [ "${MOCK_VOLUME_EXISTS:-0}" = 1 ] || { echo "mock: no such volume" >&2; exit 1; }; exit 0 ;;
      rm)      [ "${MOCK_VOLUME_RM_OK:-1}" = 1 ] || { echo "mock: volume is in use" >&2; exit 1; }; exit 0 ;;
    esac
    exit 0 ;;
  compose)
    shift
    for a in "$@"; do
      case "$a" in
        version) echo "Docker Compose version v2.29.0"; exit 0 ;;
        up)      exit 0 ;;
        ps)      printf '%s\n' "${MOCK_PS:-running}"; exit 0 ;;
        logs)    printf '%s\n' "${MOCK_AGENT_LOG:-}"; exit 0 ;;
      esac
    done
    exit 0 ;;
  *) exit 0 ;;
esac
MOCK
chmod +x "$tmp/bin/docker"
# sudo shim: the tests never run as root; the installer prefixes privileged
# commands with sudo, which here just runs them.
# It refuses to run without -n (the installer must never let sudo prompt from
# inside a pipe), and MOCK_SUDO_PASSWORD=1 makes it behave like a host whose
# sudo wants a password.
cat >"$tmp/bin/sudo" <<'MOCK'
#!/usr/bin/env bash
[ "${1:-}" = "-n" ] || { echo "mock sudo: invoked without -n (would prompt)" >&2; exit 97; }
shift
[ "${MOCK_SUDO_PASSWORD:-0}" = 1 ] && { echo "sudo: a password is required" >&2; exit 1; }
exec "$@"
MOCK
chmod +x "$tmp/bin/sudo"
# apparmor_parser stub: records argv so the load call can be asserted without
# touching the workstation's kernel policy. MOCK_AA_MISSING=1 removes it from PATH.
cat >"$tmp/bin/apparmor_parser" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${MOCK_AA_LOG:-/dev/null}"
exit 0
MOCK
chmod +x "$tmp/bin/apparmor_parser"

# run_installer <label> [ENV=val ...] — runs the script piped through sh the way
# the one-liner does (stdin is the script), captures stdout+stderr and rc.
run_installer() {
  local label="$1"; shift
  local log="$tmp/$label.docker.log"
  local aalog="$tmp/$label.aa.log"
  : > "$log"; : > "$aalog"
  set +e
  env PATH="$tmp/bin:$PATH" MOCK_DOCKER_LOG="$log" MOCK_AA_LOG="$aalog" \
      QUASAR_ENROLL_ROOT="${ROOT_DIR:-$tmp/root}" \
      QUASAR_DIR="${INSTALL_DIR:-$tmp/install}" QUASAR_ENROLL_TAIL_SECS=1 \
      "$@" sh ${EXTRA_ARGS:-} < "$script" > "$tmp/$label.out" 2>&1
  RC=$?
  set -e
  OUT="$(cat "$tmp/$label.out")"
  DOCKER_LOG="$(cat "$log")"
  AA_LOG="$(cat "$aalog")"
}

# ── 1. the string is required and never cleartext ────────────────────────────
mk_root "$tmp/root"

run_installer no-string
if [ "$RC" -eq 2 ] && grep -q 'QUASAR_ENROLLMENT' <<<"$OUT" && grep -q 'Enroll host' <<<"$OUT" && [ -z "$DOCKER_LOG" ]; then
  pass "no enrollment string: rc=2, points at Admin → Fleet → Enroll host, touches no docker"
else
  fail "no enrollment string" "rc=$RC docker=[$DOCKER_LOG] out=$(head -3 <<<"$OUT")"
fi

run_installer ws-blob QUASAR_ENROLLMENT="$WS_BLOB"
if [ "$RC" -eq 2 ] && grep -qi 'cleartext' <<<"$OUT" && grep -q 'ws://' <<<"$OUT" && [ -z "$DOCKER_LOG" ] && [ ! -e "$tmp/install" ]; then
  pass "ws:// string: refused as cleartext before anything is written"
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


# ── 2. preflights refuse before anything is written ──────────────────────────
mk_root "$tmp/root"
run_installer sudo-pw QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_SUDO_PASSWORD=1
if [ "$RC" -eq 1 ] && grep -q 'password' <<<"$OUT" && grep -q 'sudo -i' <<<"$OUT" && [ -z "$DOCKER_LOG" ] && [ ! -e "$tmp/install" ]; then
  pass "sudo wants a password: refused up front with the root-shell way out; never prompts, touches nothing"
else
  fail "sudo password" "rc=$RC docker=[$DOCKER_LOG] out=$(head -4 <<<"$OUT")"
fi

mk_root "$tmp/root"
printf '1\n' > "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
run_installer apparmor QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"
if [ "$RC" -eq 1 ] && grep -q 'kernel.apparmor_restrict_unprivileged_userns=0' <<<"$OUT" && grep -q '#76' <<<"$OUT" \
   && ! grep -q ' up ' <<<"$DOCKER_LOG" && ! grep -q '^pull' <<<"$DOCKER_LOG" && [ ! -e "$tmp/install" ]; then
  pass "Ubuntu 24.04 AppArmor knob: refused up front, names the sysctl, writes and starts nothing"
else
  fail "apparmor preflight" "rc=$RC docker=[$DOCKER_LOG] out=$(head -4 <<<"$OUT")"
fi

mk_root "$tmp/root"
rm "$tmp/root/dev/uinput"
run_installer uinput QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"
if [ "$RC" -eq 1 ] && grep -q 'modprobe uinput' <<<"$OUT" && [ ! -e "$tmp/install" ]; then
  pass "missing /dev/uinput: refused with the modprobe line"
else
  fail "uinput preflight" "rc=$RC out=$(head -4 <<<"$OUT")"
fi

mk_root "$tmp/root"
rm -r "$tmp/root/sys/class/drm/renderD128"
run_installer nogpu QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"
if [ "$RC" -eq 1 ] && grep -qi 'render node' <<<"$OUT" && [ ! -e "$tmp/install" ]; then
  pass "no render node: refused"
else
  fail "gpu preflight" "rc=$RC out=$(head -4 <<<"$OUT")"
fi

# ── 3. the happy path: AMD host, release ref ─────────────────────────────────
mk_root "$tmp/root"
ENROLLED_LOG='2026-09-04T10:00:00Z INFO quasar_node_agent::agent: enrolled as host 3f2c…; node_secret saved to /var/lib/quasar-agent/node-secret'
run_installer amd-ok QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
envf="$tmp/install/.env"
if [ "$RC" -eq 0 ] && [ -f "$envf" ] && [ "$(stat -c %a "$envf")" = 600 ] \
   && grep -qxF "QUASAR_ENROLLMENT=$WSS_BLOB" "$envf" \
   && grep -qxF "NODE_NAME=gpu-b" "$envf" \
   && grep -qxF "QUASAR_AGENT_IMAGE=ghcr.io/accreleus/quasar/quasar-node-agent:1.2.3" "$envf" \
   && grep -qxF "QUASAR_RENDER_NODE=/dev/dri/renderD128" "$envf" \
   && grep -qxF "COMPOSE_FILE=docker-compose.yml" "$envf" \
   && grep -qxF "QUASAR_HOME_ROOT=$tmp/homes" "$envf" && [ -d "$tmp/homes" ] \
   && grep -qxF "QUASAR_STACK_DIR=$tmp/install" "$envf" \
   && grep -qxF "QUASAR_UPDATER_IMAGE=ghcr.io/accreleus/quasar/quasar-updater:1.2.3" "$envf" \
   && grep -q "^pull ghcr.io/accreleus/quasar/quasar-node-agent:1.2.3$" <<<"$DOCKER_LOG" \
   && grep -q "^pull ghcr.io/accreleus/quasar/quasar-updater:1.2.3$" <<<"$DOCKER_LOG" \
   && grep -q 'up -d quasar-node-agent' <<<"$DOCKER_LOG" \
   && grep -q 'up -d quasar-updater' <<<"$DOCKER_LOG" \
   && [ "$(grep -n 'up -d quasar-updater' <<<"$DOCKER_LOG" | cut -d: -f1)" -lt \
        "$(grep -n 'up -d quasar-node-agent' <<<"$DOCKER_LOG" | cut -d: -f1)" ] \
   && grep -q "enrolled: this host is now 'gpu-b'" <<<"$OUT"; then
  # Ordering matters: the agent reads updater presence once at boot, so an agent
  # started first registers updater_present=false and stays that way.
  pass "AMD host + release tag: 0600 env, both images pinned to :1.2.3, updater started BEFORE the agent, enrolled"
else
  fail "happy path" "rc=$RC docker=[$DOCKER_LOG] env=[$(cat "$envf" 2>/dev/null)] out=$(tail -5 <<<"$OUT")"
fi
if ! grep -qF "$TOKEN" <<<"$DOCKER_LOG" && ! grep -qF "$TOKEN" <<<"$OUT" \
   && ! grep -q "$TOKEN" "$tmp/install/docker-compose.yml"; then
  pass "the token reaches only the .env: no docker argv, no output line, not the compose file"
else
  fail "token containment" "leaked into docker argv, output or compose"
fi
cf="$tmp/install/docker-compose.yml"
if [ -f "$cf" ] && ! grep -q -E 'depends_on|ENROLLMENT_TOKEN|CONTROL_PLANE_URL' "$cf" \
   && grep -q 'QUASAR_ENROLLMENT: ${QUASAR_ENROLLMENT:?}' "$cf" && grep -q 'network_mode: host' "$cf" \
   && [ ! -e "$tmp/install/docker-compose.nvidia.yml" ]; then
  pass "compose file: the agent service without the local-stack coupling; no NVIDIA overlay on AMD"
else
  fail "compose file" "$(grep -n -E 'depends_on|ENROLLMENT_TOKEN|CONTROL_PLANE_URL' "$cf" 2>&1 | head -3)"
fi

# ── 4. re-running updates the one agent ──────────────────────────────────────
echo 'QUASAR_ENCODER=vulkan' >> "$envf"
NEW_BLOB="qenr1.$FP.$(b64url 'wss://cp.example:8443').second-token"
run_installer rerun QUASAR_ENROLLMENT="$NEW_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && [ "$(grep -c '^QUASAR_ENROLLMENT=' "$envf")" = 1 ] && grep -qxF "QUASAR_ENROLLMENT=$NEW_BLOB" "$envf" \
   && [ "$(grep -c '^COMPOSE_PROJECT_NAME=' "$envf")" = 1 ] && grep -qxF 'QUASAR_ENCODER=vulkan' "$envf" \
   && grep -q 'updating existing' <<<"$OUT" && grep -q -- '--project-name quasar-agent' <<<"$DOCKER_LOG" \
   && [ "$(stat -c %a "$envf")" = 600 ]; then
  pass "re-run: managed keys replaced once, operator line kept, same compose project (no second agent)"
else
  fail "idempotent re-run" "rc=$RC env=[$(cat "$envf")] out=$(tail -3 <<<"$OUT")"
fi

# ── 5. NVIDIA host ───────────────────────────────────────────────────────────
rm -rf "$tmp/install"
mk_root "$tmp/root" 0x10de
run_installer nv-notoolkit QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"
if [ "$RC" -eq 1 ] && grep -q 'NVIDIA Container Toolkit' <<<"$OUT" && [ ! -e "$tmp/install" ]; then
  pass "NVIDIA without the container toolkit: refused, names it"
else
  fail "nvidia toolkit preflight" "rc=$RC out=$(head -6 <<<"$OUT")"
fi
printf '#!/usr/bin/env bash\nexit 0\n' > "$tmp/bin/nvidia-ctk"; chmod +x "$tmp/bin/nvidia-ctk"
run_installer nv-ok QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && [ -f "$tmp/install/docker-compose.nvidia.yml" ] && grep -q 'gpus: all' "$tmp/install/docker-compose.nvidia.yml" \
   && grep -qxF 'COMPOSE_FILE=docker-compose.yml:docker-compose.nvidia.yml' "$envf" \
   && grep -q -- "-f $tmp/install/docker-compose.nvidia.yml" <<<"$DOCKER_LOG" && grep -q 'CDI' <<<"$OUT"; then
  pass "NVIDIA host: overlay written and passed to compose; missing CDI spec is a named warning"
else
  fail "nvidia happy path" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -6 <<<"$OUT")"
fi
rm -f "$tmp/bin/nvidia-ctk"

# ── 6. image resolution ──────────────────────────────────────────────────────
rm -rf "$tmp/install"; mk_root "$tmp/root"
SHA=0123456789abcdef0123456789abcdef01234567
run_installer sha-fallback QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=$SHA NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_PULL_OK=0 MOCK_LOCAL_IMAGE=1
if [ "$RC" -eq 0 ] && grep -q '^pull ghcr.io/accreleus/quasar/quasar-node-agent:sha-0123456$' <<<"$DOCKER_LOG" \
   && grep -q 'image: quasar-node-agent:latest (local build; ghcr.io/accreleus/quasar/quasar-node-agent:sha-0123456 is not published)' <<<"$OUT" \
   && grep -qxF 'QUASAR_AGENT_IMAGE=quasar-node-agent:latest' "$envf"; then
  pass "commit ref: tries :sha-<7>, falls back to the local build and says which tag is unpublished"
else
  fail "sha fallback" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -4 <<<"$OUT")"
fi
rm -rf "$tmp/install"
run_installer no-image QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=feat/x NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_PULL_OK=0 MOCK_LOCAL_IMAGE=0
if [ "$RC" -eq 1 ] && grep -q 'no agent image' <<<"$OUT" && grep -q '^pull ghcr.io/accreleus/quasar/quasar-node-agent:feat-x$' <<<"$DOCKER_LOG" && [ ! -e "$envf" ]; then
  pass "branch ref, nothing pullable, no local build: refused with instructions, nothing written"
else
  fail "no image" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -3 <<<"$OUT")"
fi
run_installer explicit QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_AGENT_IMAGE=ghcr.io/x/y@sha256:abc QUASAR_REF=v9.9.9 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
# QUASAR_AGENT_IMAGE overrides the AGENT's ref-derived tag and nothing else: the
# updater has its own override and still follows QUASAR_REF here.
if [ "$RC" -eq 0 ] && grep -q '^pull ghcr.io/x/y@sha256:abc$' <<<"$DOCKER_LOG" \
   && ! grep -q 'quasar-node-agent.*9\.9\.9' <<<"$DOCKER_LOG" \
   && grep -q '^pull ghcr.io/accreleus/quasar/quasar-updater:9.9.9$' <<<"$DOCKER_LOG"; then
  pass "QUASAR_AGENT_IMAGE overrides the ref-derived agent tag; the updater still follows the ref"
else
  fail "explicit image" "docker=[$DOCKER_LOG]"
fi

# ── 7. verdicts, dry run, sub-commands ───────────────────────────────────────
run_installer authfail QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG='WARN auth_failed: authentication failed'
if [ "$RC" -eq 1 ] && grep -q 'Mint a fresh string' <<<"$OUT" && grep -q 'live agent' <<<"$OUT"; then
  pass "auth_failed in the log: rc=1, names the four causes"
else
  fail "auth_failed verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
run_installer pinfail QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG='ERROR token="cp-tls-pin-mismatch" expected=… observed=…'
if [ "$RC" -eq 1 ] && grep -q 'CONTROL_PLANE_FINGERPRINT' <<<"$OUT"; then
  pass "pin mismatch in the log: rc=1, names the rotation path"
else
  fail "pin mismatch verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
run_installer stale QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG='config error: CONTROL_PLANE_URL is required (e.g. ws://localhost:8080)'
if [ "$RC" -eq 1 ] && grep -q 'predates the enrollment string' <<<"$OUT" && grep -q 'QUASAR_AGENT_IMAGE' <<<"$OUT"; then
  pass "an agent image older than the enrollment string: named as stale, with the way to a current one"
else
  fail "stale image verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
run_installer timeout QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG=''
if [ "$RC" -eq 3 ] && grep -q 'still connecting' <<<"$OUT" && grep -q 'logs -f quasar-node-agent' <<<"$OUT"; then
  pass "no verdict within the window: rc=3 with the follow command"
else
  fail "timeout verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi
rm -rf "$tmp/install"
run_installer dry QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" QUASAR_ENROLL_DRY_RUN=1
if [ "$RC" -eq 0 ] && grep -q 'dry run' <<<"$OUT" && grep -q 'would use: ghcr.io/accreleus/quasar/quasar-node-agent:1.2.3' <<<"$OUT" \
   && [ ! -e "$tmp/install" ] && ! grep -q -E '^pull| up ' <<<"$DOCKER_LOG"; then
  pass "dry run: prints the plan, pulls nothing, writes nothing"
else
  fail "dry run" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -4 <<<"$OUT")"
fi
printed="$(sh "$script" --print-compose)"
if grep -q '^  quasar-node-agent:$' <<<"$printed" && grep -q 'QUASAR_ENROLLMENT' <<<"$printed" && ! grep -q 'depends_on' <<<"$printed" \
   && grep -q '^  quasar-updater:$' <<<"$printed" && grep -q '^  quasar-updater-run:$' <<<"$printed" \
   && grep -q 'gpus: all' <<<"$(sh "$script" --print-nvidia-overlay)"; then
  pass "--print-compose / --print-nvidia-overlay: the manual path needs no enrollment string"
else
  fail "print sub-commands"
fi

# ── 8. tty rendering: same facts, live lines; plain stays the log form ───────
rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer tty-ok QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" QUASAR_ENROLL_STYLE=tty LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
esc="$(printf '\033')"
if [ "$RC" -eq 0 ] && grep -q "${esc}\[32m✔${esc}\[0m docker + compose: ok" <<<"$OUT" \
   && grep -q "✔${esc}\[0m enrolled: this host is now 'gpu-b'" <<<"$OUT" \
   && grep -q "${esc}\[1m==> Host preflights" <<<"$OUT" \
   && grep -q 'logs:   docker compose --project-directory' <<<"$OUT"; then
  pass "tty: bold steps, green ticks, a live wait line, and the where-things-are summary"
else
  fail "tty rendering" "rc=$RC out=$(cat -v <<<"$OUT")"
fi
# A rewritten line must leave nothing behind: after the last carriage return on
# every line, no spinner frame may remain.
if ! sed 's/.*\r//' <<<"$OUT" | grep -q '[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]'; then
  pass "tty: no spinner frame survives on any line"
else
  fail "tty: stray spinner" "$(sed 's/.*\r//' <<<"$OUT" | grep '[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]' | head -3 | cat -v)"
fi
# Same facts in both styles: strip the tty decoration and compare to plain.
rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer plain-ok QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" QUASAR_ENROLL_STYLE=plain
PLAIN="$OUT"
stripped="$(sed 's/.*\r//' "$tmp/tty-ok.out" | sed -E "s/${esc}\[[0-9;]*[mK]//g; s/^  ✔ /  /; s/^  (files|logs|update):.*$//" | grep -v '^$')"
if diff <(grep -v '^$' <<<"$PLAIN") <(printf '%s\n' "$stripped") >/dev/null; then
  pass "tty carries exactly the plain facts, decorated"
else
  fail "tty/plain facts differ" "$(diff <(grep -v '^$' <<<"$PLAIN") <(printf '%s\n' "$stripped") | head -12 | cat -v)"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer tty-wait QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG='' QUASAR_ENROLL_STYLE=tty LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
if [ "$RC" -eq 3 ] && grep -q 'waiting for the agent to enroll… 0s' <<<"$OUT" && grep -q 'still connecting' <<<"$OUT" \
   && ! sed 's/.*\r//' <<<"$OUT" | grep -q 'waiting for the agent'; then
  pass "tty: the wait is a live line that is cleared before the verdict"
else
  fail "tty wait line" "rc=$RC $(tail -3 <<<"$OUT" | cat -v)"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer tty-ascii QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" QUASAR_ENROLL_STYLE=tty LANG=C LC_ALL=C
if [ "$RC" -eq 0 ] && grep -q '\[ok\]' <<<"$OUT" && ! grep -q '✔' <<<"$OUT"; then
  pass "tty without a UTF-8 locale: ASCII glyphs"
else
  fail "tty ascii" "$(tail -3 <<<"$OUT" | cat -v)"
fi

mk_root "$tmp/root"; printf '1\n' > "$tmp/root/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
run_installer tty-fail QUASAR_ENROLLMENT="$WSS_BLOB" NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" QUASAR_ENROLL_STYLE=tty LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8
if [ "$RC" -eq 1 ] && grep -q "${esc}\[31m✘ enroll-host: the host restricts" <<<"$OUT" && grep -q 'apparmor_restrict_unprivileged_userns=0' <<<"$OUT"; then
  pass "tty: a failed preflight is a red cross with the remediation"
else
  fail "tty failure" "rc=$RC $(tail -3 <<<"$OUT" | cat -v)"
fi

# ── 9. the app-container AppArmor profile (#76) ──────────────────────────────
# The workstation's real apparmor_parser is shadowed by the stub in $tmp/bin for
# every run here — nothing below can load policy on this machine.
if diff -q <(sh "$script" --print-apparmor-profile) "$root/deploy/apparmor/quasar-app" >/dev/null; then
  pass "--print-apparmor-profile is byte-identical to deploy/apparmor/quasar-app"
else
  fail "apparmor profile drift" "$(diff <(sh "$script" --print-apparmor-profile) "$root/deploy/apparmor/quasar-app" | head -8)"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer aa-absent QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && [ ! -e "$tmp/install/apparmor" ] && [ -z "$AA_LOG" ] && ! grep -qi 'apparmor' <<<"$OUT"; then
  pass "host without AppArmor: no profile written, apparmor_parser never called"
else
  fail "apparmor on a non-apparmor host" "aa=[$AA_LOG] $(grep -i apparmor <<<"$OUT" | head -3)"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
mkdir -p "$tmp/root/sys/module/apparmor/parameters"; printf 'Y\n' > "$tmp/root/sys/module/apparmor/parameters/enabled"
run_installer aa-load QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
prof="$tmp/install/apparmor/quasar-app"
if [ "$RC" -eq 0 ] && [ -f "$prof" ] && diff -q "$prof" "$root/deploy/apparmor/quasar-app" >/dev/null \
   && grep -qxF -- "-r -W $prof" <<<"$AA_LOG" && grep -q 'loaded the quasar-app AppArmor profile' <<<"$OUT" \
   && [ ! -e "$tmp/root/etc/apparmor.d/quasar-app" ]; then
  pass "AppArmor host: profile written beside the compose, loaded with apparmor_parser -r -W, not persisted by default"
else
  fail "apparmor profile install" "rc=$RC aa=[$AA_LOG] prof=$( [ -f "$prof" ] && echo present || echo MISSING) out=$(grep -i apparmor <<<"$OUT" | head -3)"
fi

rm -rf "$tmp/install"
run_installer aa-persist QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" QUASAR_ENROLL_APPARMOR_PERSIST=1
if [ "$RC" -eq 0 ] && diff -q "$tmp/root/etc/apparmor.d/quasar-app" "$root/deploy/apparmor/quasar-app" >/dev/null \
   && [ "$(stat -c %a "$tmp/root/etc/apparmor.d/quasar-app")" = 644 ] && grep -q 'persisted /etc/apparmor.d/quasar-app' <<<"$OUT"; then
  pass "QUASAR_ENROLL_APPARMOR_PERSIST=1: the profile is also installed 0644 in /etc/apparmor.d"
else
  fail "apparmor persist" "rc=$RC $(grep -i apparmor <<<"$OUT" | head -3)"
fi

# ── 10. the saved agent identity (#199) ──────────────────────────────────────
# The node secret lives in a project-scoped named volume that outlives a re-run,
# and the agent presents it in preference to the enrollment token. A machine
# enrolled to a DIFFERENT control plane therefore arrives holding a credential
# the new one has never seen — the agent recovers from that itself, so this
# script must NOT go behind its back and delete the volume. Clearing it is opt-in,
# and the only thing the default does is say the volume is there.

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-fresh QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && ! grep -q 'volume rm' <<<"$DOCKER_LOG" && ! grep -q 'saved agent identity' <<<"$OUT"; then
  pass "a machine with no saved identity: nothing is said about one and no volume is removed"
else
  fail "fresh identity" "rc=$RC docker=[$(grep volume <<<"$DOCKER_LOG")] out=$(grep -i identity <<<"$OUT")"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-keep QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_VOLUME_EXISTS=1
if [ "$RC" -eq 0 ] && ! grep -q 'volume rm' <<<"$DOCKER_LOG"    && grep -q 'quasar-agent_quasar-agent-data' <<<"$OUT" && grep -q 'QUASAR_RESET_IDENTITY=1' <<<"$OUT"; then
  pass "an existing identity volume: kept, named, and the way to clear it offered"
else
  fail "existing identity kept" "rc=$RC docker=[$(grep volume <<<"$DOCKER_LOG")] out=$(grep -i identity <<<"$OUT")"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-reset QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_VOLUME_EXISTS=1 QUASAR_RESET_IDENTITY=1
if [ "$RC" -eq 0 ] && grep -qxF 'volume rm quasar-agent_quasar-agent-data' <<<"$DOCKER_LOG"    && grep -q 'rm -sf quasar-node-agent' <<<"$DOCKER_LOG" && grep -q 'cleared' <<<"$OUT"    && [ "$(grep -c 'volume rm' <<<"$DOCKER_LOG")" -eq 1 ]; then
  pass "QUASAR_RESET_IDENTITY=1: the agent container goes first, then its identity volume, exactly once"
else
  fail "reset identity" "rc=$RC docker=[$(grep -E 'volume|rm -sf' <<<"$DOCKER_LOG")] out=$(grep -i identity <<<"$OUT")"
fi

# The container is removed BEFORE the volume: a volume a container still holds
# cannot be removed, and getting this order wrong fails only on a real daemon.
if [ "$(grep -n 'rm -sf quasar-node-agent' "$tmp/id-reset.docker.log" | head -1 | cut -d: -f1)"    -lt "$(grep -n 'volume rm' "$tmp/id-reset.docker.log" | head -1 | cut -d: -f1)" ]; then
  pass "reset identity: the container is removed before the volume"
else
  fail "reset ordering" "$(grep -E 'volume|rm -sf' "$tmp/id-reset.docker.log")"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
EXTRA_ARGS="-s -- --reset-identity"   run_installer id-reset-argv QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_VOLUME_EXISTS=1
if [ "$RC" -eq 0 ] && grep -qxF 'volume rm quasar-agent_quasar-agent-data' <<<"$DOCKER_LOG"; then
  pass "--reset-identity as an argument does the same as the environment variable"
else
  fail "--reset-identity argv" "rc=$RC docker=[$(grep volume <<<"$DOCKER_LOG")] out=$(tail -3 <<<"$OUT")"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-reset-busy QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_VOLUME_EXISTS=1 QUASAR_RESET_IDENTITY=1 MOCK_VOLUME_RM_OK=0
if [ "$RC" -eq 1 ] && grep -q 'could not remove the agent identity volume' <<<"$OUT" && grep -q 'down' <<<"$OUT"    && ! grep -q ' up -d quasar-node-agent' <<<"$DOCKER_LOG"; then
  pass "a volume that cannot be removed: refused with the remedy, and no agent is started on a half-reset host"
else
  fail "reset identity blocked" "rc=$RC docker=[$DOCKER_LOG] out=$(tail -3 <<<"$OUT")"
fi

rm -rf "$tmp/install2"; mk_root "$tmp/root"
INSTALL_DIR="$tmp/install2" run_installer id-project QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes" MOCK_AGENT_LOG="$ENROLLED_LOG" MOCK_VOLUME_EXISTS=1 QUASAR_RESET_IDENTITY=1 QUASAR_PROJECT=quasar-agent-b
if [ "$RC" -eq 0 ] && grep -qxF 'volume rm quasar-agent-b_quasar-agent-data' <<<"$DOCKER_LOG"    && grep -qxF 'COMPOSE_PROJECT_NAME=quasar-agent-b' "$tmp/install2/.env"; then
  pass "QUASAR_PROJECT: the project name and its identity volume move together"
else
  fail "project override" "rc=$RC docker=[$(grep volume <<<"$DOCKER_LOG")] env=[$(grep COMPOSE_PROJECT "$tmp/install2/.env" || true)]"
fi
rm -rf "$tmp/install2"

# The unrecoverable half of #199: the agent held a stale secret and had no token
# to fall back on. The RECOVERABLE log line must not end the wait — the agent is
# one attempt away from enrolling when it prints that.
rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-stale QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"   MOCK_AGENT_LOG='ERROR token="cp-register-stale-identity-unresolvable" the node secret saved at /var/lib/quasar-agent/node-secret identifies no host'
if [ "$RC" -eq 1 ] && grep -q 'QUASAR_RESET_IDENTITY=1' <<<"$OUT" && grep -q 'quasar-agent_quasar-agent-data' <<<"$OUT"; then
  pass "a stale identity the agent cannot resolve: rc=1, names the volume and the reset flag"
else
  fail "stale identity verdict" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

rm -rf "$tmp/install"; mk_root "$tmp/root"
run_installer id-recovering QUASAR_ENROLLMENT="$WSS_BLOB" QUASAR_REF=v1.2.3 NODE_NAME=gpu-b QUASAR_HOME_ROOT="$tmp/homes"   MOCK_AGENT_LOG='WARN token="cp-register-stale-identity" registering again with the configured enrollment token
'"$ENROLLED_LOG"
if [ "$RC" -eq 0 ] && grep -q 'enrolled: this host is now' <<<"$OUT"; then
  pass "the agent re-enrolling after a stale secret is a success, not a verdict"
else
  fail "recoverable stale identity" "rc=$RC out=$(tail -3 <<<"$OUT")"
fi

# --help must keep carrying what the script now documents; usage() prints a fixed
# line range of the header, and a range that drifts silently drops knobs.
help_out="$(sh "$script" --help)"
if grep -q 'QUASAR_RESET_IDENTITY' <<<"$help_out" && grep -q -- '--reset-identity' <<<"$help_out"    && grep -q 'QUASAR_PROJECT' <<<"$help_out" && grep -q 'self-signed' <<<"$help_out"    && grep -q -- '--pinnedpubkey' <<<"$help_out"; then
  pass "--help carries the identity knobs and the --pinnedpubkey/-k pairing"
else
  fail "help text" "$(tail -5 <<<"$help_out")"
fi

# Every run above must have kept the token off stdout/stderr.
for f in "$tmp"/*.out; do
  if grep -qF "$TOKEN" "$f"; then fail "token leaked to output in $(basename "$f")"; fi
done

printf '\n%d passed, %d failed\n' "$PASS_N" "$FAIL_N"
[ "$FAIL_N" -eq 0 ]
