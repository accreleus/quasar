#!/usr/bin/env bash
# run-readiness-faults.sh — RH-02 #264 readiness fault-injection acceptance harness.
#
# Purpose: exercise the evidence-gated readiness gate (protocol/control-api.md
# "Evidence-gated readiness", ADR 0005) end to end against a disposable,
# harness-owned compose stack: every fault in docs/superpowers/plans/
# 2026-09-19-rh02-264-harness-matrix.md (THE SPEC — read it first; where this
# script and the spec differ, the spec wins) is injected against a REAL control
# plane + REAL node agent, observed through the public API, and cleared again.
#
# Where it runs: on a docker host directly (like run-admission.sh) — it starts
# and stops compose stacks, mutates a tmpfs it owns, and (scenario 1) drives a
# nested dind engine. It is NOT meant to run via `scripts/dev/dev.sh run`,
# which mounts only the repo with no docker socket — see the docker-unavailable
# guard below, which reports every scenario `unperformed` and exits 3 rather
# than silently passing in that mode.
#
# Usage:
#   scripts/harness/run-readiness-faults.sh \
#     --control-image=REF --agent-image=REF --role=ROLE \
#     [--only=1,2b,7] [--keep] [--control-port=N] [--results-dir=DIR] \
#     [--allow-cohabit]
#
# Wire facts this script relies on (verified against the repo, not guessed):
#   - agent WebSocket route: /agent/ws (node-agent connects to
#     CONTROL_PLANE_URL + "/agent/ws"; deploy/docker-compose.yml's
#     CONTROL_PLANE_URL carries no path).
#   - QUASAR_ALLOW_PLAINTEXT_AGENT=1 is required only for a non-loopback
#     plaintext control-plane URL (node-agent/src/enrollment.rs) — the real
#     agent stays on loopback (dials the relay at 127.0.0.1) and needs it not;
#     the nested agent (scenario 1, dials a gateway IP) does.
#   - image source-commit label: org.quasar.source.commit (deploy/Dockerfile.*).
#   - agent-owned container name prefixes: quasar-sess-, quasar-pulse-,
#     quasar-probe- (node-agent/src/container_ownership.rs,
#     node-agent/src/session/audio.rs).
#   - compose services: quasar-postgres, quasar-control-plane,
#     quasar-node-agent, quasar-updater — this harness starts only the first
#     three (never quasar-updater, which touches nothing under test).
#   - NVIDIA hosts need -f deploy/docker-compose.nvidia.yml (driver volume
#     quasar-nvidia-driver, gpus: all, LD_LIBRARY_PATH, …).
#   - schema version: control-plane's own Postgres schema_migrations.version
#     (golang-migrate default table/column; control-plane/internal/migrate).
#   - GET list endpoints wrap results as {"items": [...]} (control-plane/internal/crud/handler.go);
#     GET /v1/admin/activity wraps as {"items": [...]} with per-row
#     "actor_user_id" (control-plane/internal/audit/store.go).
#
# Ownership (spec "Fixtures"): run id RID=rh02h-<8 hex>; compose project $RID;
# label quasar.harness.owner=$RID on everything the harness creates directly;
# every host path under /var/lib/$RID; scripted/nested node names $RID-real /
# $RID-nested / $RID-a / $RID-b. Cleanup (trap EXIT, idempotent) stops sessions
# via the API, deletes every $RID-* host row via DELETE /v1/hosts/{id} (agent
# offline first), tears the compose project down with -v, removes fixture
# containers/volumes/network by label, unmounts and removes /var/lib/$RID, then
# verifies scenario 9 by label/path/name-prefix.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/harness/lib/harness.sh
source "$ROOT/scripts/harness/lib/harness.sh"

# ── Args ─────────────────────────────────────────────────────────────────────
CONTROL_IMAGE=""
AGENT_IMAGE=""
ROLE=""
ONLY=""
KEEP=0
CONTROL_PORT="${CONTROL_PORT:-18080}"
ALLOW_COHABIT=0

for a in "$@"; do
  case "$a" in
    --control-image=*) CONTROL_IMAGE="${a#*=}" ;;
    --agent-image=*) AGENT_IMAGE="${a#*=}" ;;
    --role=*) ROLE="${a#*=}" ;;
    --only=*) ONLY="${a#*=}" ;;
    --keep) KEEP=1 ;;
    --control-port=*) CONTROL_PORT="${a#*=}" ;;
    --results-dir=*) HARNESS_RESULTS_DIR="${a#*=}" ;;
    --allow-cohabit) ALLOW_COHABIT=1 ;;
    -h | --help)
      sed -n '2,54p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown arg: $a" >&2
      exit 2
      ;;
  esac
done
export HARNESS_RESULTS_DIR

harness_init "readiness-faults"

# ── unperformed: thin wrapper over skip (spec "Result vocabulary") ──────────
HARNESS_UNPERFORMED=0
unperformed() {
  HARNESS_UNPERFORMED=$((HARNESS_UNPERFORMED + 1))
  skip "UNPERFORMED $*"
}

# only_run <id> — true if --only was not given, or names this id
only_run() {
  local id="$1" tok
  [ -z "$ONLY" ] && return 0
  local IFS=,
  for tok in $ONLY; do
    [ "$tok" = "$id" ] && return 0
    # A bare scenario number selects all of its rows: --only=7 runs 7a..7h.
    case "$tok" in
      *[!0-9]*) ;;
      *) case "$id" in "$tok"[a-z]*) return 0 ;; esac ;;
    esac
  done
  return 1
}
any_of() { # any_of id... — true if any is selected
  local id
  for id in "$@"; do
    if only_run "$id"; then return 0; fi
  done
  return 1
}

# ── Sanitizer (spec "Report"): hostname, short hostname, IPv4/IPv6 literals
# (except 127.0.0.1/::1/0.0.0.0), any host:port/ registry prefix, and the
# absolute home path — replaced with role placeholders. Deliberately does NOT
# touch the bare invoking username as a standalone word: `quasar` is both the
# system account and product vocabulary (quasar-node-agent, …), so a blind
# substring replace would corrupt legitimate mentions. The $HOME path check
# already covers the username-in-a-path case, which is the real leak risk.
sanitize_text() {
  python3 -c "
import re, sys

s = sys.stdin.read()
hostname = '$(hostname -s 2>/dev/null || echo unknown-host)'
fqdn = '$(hostname -f 2>/dev/null || echo unknown-host)'

for h in sorted({fqdn, hostname}, key=len, reverse=True):
    if h and h != 'unknown-host':
        s = re.sub(re.escape(h), '<role-host>', s, flags=re.I)

def ipv4_sub(m):
    ip = m.group(0)
    return ip if ip in ('127.0.0.1', '0.0.0.0') else '<role-ipv4>'
s = re.sub(r'\b(?:\d{1,3}\.){3}\d{1,3}\b', ipv4_sub, s)

def ipv6_sub(m):
    ip = m.group(0)
    return ip if ip == '::1' else '<role-ipv6>'
s = re.sub(r'\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}\b', ipv6_sub, s)

s = re.sub(r'\b[\w.-]+:\d{2,5}/(?=[\w./-]+:)', '<registry>/', s)

home = '$HOME'
if home:
    s = s.replace(home, '<role-home>')

print(s, end='')
"
}

# finish_run <base_rc> — write the JSON report (via lib/harness.sh's own
# writer), render the human-readable Markdown summary, sanitize BOTH files,
# grep them for survivors (fail the run if any leak through — spec "Report"),
# then exit with the matrix's own exit-code contract: 1 on any fail, 4 when
# nothing failed but something was unperformed, 0 only when everything
# passed. lib/harness.sh's own harness_report() cannot be reused for the
# final exit here because its idempotency guard makes a second call return 0
# rather than exit — so this computes and exits directly instead.
finish_run() {
  local base_rc="${1:-0}"
  local verdict
  if [ "$HARNESS_FAIL" -gt 0 ]; then
    verdict=fail
  elif [ "$HARNESS_UNPERFORMED" -gt 0 ]; then
    verdict=incomplete
  else
    verdict=pass
  fi
  harness_note "verdict" "$verdict"

  _harness_write_report

  local md_file="${HARNESS_RESULTS_FILE%.json}.md"
  local i=0
  {
    echo "# RH-02 #264 readiness-faults report"
    echo
    echo "| field | value |"
    echo "| --- | --- |"
    echo "| role | ${ROLE:-} |"
    echo "| rid | ${RID:-} |"
    echo "| gpu vendor | ${GPU_VENDOR:-unknown} |"
    echo "| gpu index | ${GPU_INDEX:-unknown} |"
    echo "| control image | ${CONTROL_IMAGE:-} |"
    echo "| agent image | ${AGENT_IMAGE:-} |"
    echo "| control image source commit | ${CONTROL_IMAGE_COMMIT:-unknown} |"
    echo "| agent image source commit | ${AGENT_IMAGE_COMMIT:-unknown} |"
    echo "| schema_migrations version | ${SCHEMA_MIGRATIONS_VERSION:-unknown} |"
    echo "| pass | $HARNESS_PASS |"
    echo "| fail | $HARNESS_FAIL |"
    echo "| unperformed | $HARNESS_UNPERFORMED |"
    echo "| verdict | $verdict |"
    echo
    echo "## Matrix rows"
    echo
    echo "| # | result | message |"
    echo "| --- | --- | --- |"
    while [ "$i" -lt "${#HARNESS_CHECK_RESULTS[@]}" ]; do
      local result="${HARNESS_CHECK_RESULTS[$i]}" msgtxt="${HARNESS_CHECK_MESSAGES[$i]}"
      local mid="${msgtxt%%:*}"
      echo "| $mid | $result | ${msgtxt//|/\\|} |"
      i=$((i + 1))
    done
  } >"$md_file"

  printf '%s' "$(sanitize_text <"$HARNESS_RESULTS_FILE")" >"$HARNESS_RESULTS_FILE"
  printf '%s' "$(sanitize_text <"$md_file")" >"$md_file"

  local leaked=0
  if ! python3 -c "
import re, sys

paths = ['$HARNESS_RESULTS_FILE', '$md_file']
hostname = '$(hostname -s 2>/dev/null || echo unknown-host)'
fqdn = '$(hostname -f 2>/dev/null || echo unknown-host)'
home = '$HOME'
ok = True
for p in paths:
    with open(p, encoding='utf-8', errors='replace') as fh:
        s = fh.read()
    for h in (hostname, fqdn):
        if h and h != 'unknown-host' and h in s:
            print(f'leak: hostname {h!r} survives in {p}', file=sys.stderr); ok = False
    for m in re.finditer(r'\b(?:\d{1,3}\.){3}\d{1,3}\b', s):
        if m.group(0) not in ('127.0.0.1', '0.0.0.0'):
            print(f'leak: IPv4 {m.group(0)!r} survives in {p}', file=sys.stderr); ok = False
    for m in re.finditer(r'\b(?:[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{0,4}\b', s):
        if m.group(0) != '::1':
            print(f'leak: IPv6 {m.group(0)!r} survives in {p}', file=sys.stderr); ok = False
    if home and home in s:
        print(f'leak: home path {home!r} survives in {p}', file=sys.stderr); ok = False
sys.exit(0 if ok else 1)
"; then
    leaked=1
  fi
  if [ "$leaked" = "1" ]; then
    echo "FAIL: report: sanitizer leak — a hostname/IP/path survived in $HARNESS_RESULTS_FILE or $md_file" >&2
    base_rc=1
    verdict=fail
  fi

  echo "report: $HARNESS_RESULTS_FILE"
  echo "report: $md_file"
  printf 'PASS: %s FAIL: %s SKIP: %s\n' "$HARNESS_PASS" "$HARNESS_FAIL" "$HARNESS_SKIP"

  local final_rc=0
  if [ "$verdict" = "fail" ]; then
    final_rc=1
  elif [ "$verdict" = "incomplete" ]; then
    final_rc=4
  fi
  [ "$base_rc" -gt "$final_rc" ] && final_rc="$base_rc"
  _HARNESS_REPORTED=1
  exit "$final_rc"
}

CONTROL_IMAGE_COMMIT=""
AGENT_IMAGE_COMMIT=""
SCHEMA_MIGRATIONS_VERSION=""
GPU_VENDOR=""
GPU_INDEX=""

# ── docker unavailable guard — never silently pass ──────────────────────────
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  for id in 1a 1b 1c 1d 1e 2a 2b 3 4a 4a\' 4b 4c 5a 5b 6a 6b 6c 6d 7a 7b 7c 7d 7e 7f 7g 7h 8 9; do
    unperformed "$id: docker is unavailable in this environment"
  done
  echo "docker unavailable — every scenario unperformed" >&2
  finish_run 3
fi

# rand_hex <bytes> — no openssl dependency: a fresh test host may not carry it.
rand_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d " \n"; }
require curl jq python3 base64

[ -n "$CONTROL_IMAGE" ] || { echo "FATAL: --control-image is required" >&2; exit 2; }
[ -n "$AGENT_IMAGE" ] || { echo "FATAL: --agent-image is required" >&2; exit 2; }
[ -n "$ROLE" ] || { echo "FATAL: --role is required" >&2; exit 2; }

RID="rh02h-$(rand_hex 4)"
OWNER_LABEL="quasar.harness.owner=$RID"
RID_ROOT="/var/lib/$RID"

harness_note "rid" "$RID"
harness_note "role" "$ROLE"
harness_note "control_image" "$CONTROL_IMAGE"
harness_note "agent_image" "$AGENT_IMAGE"
harness_note "control_port" "$CONTROL_PORT"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/${RID}-work.XXXXXX")"
API="http://127.0.0.1:${CONTROL_PORT}"
ENROLLMENT_TOKEN="${RID}-enroll-$(rand_hex 8)"
ADMIN_EMAIL="${RID}-admin@quasar.local"
ADMIN_PASS="Rh02Harness!$(rand_hex 4)"

# ── GPU vendor pre-detection — BEFORE the stack comes up, so the right
# compose overlay is included from the first `up`. Only NVIDIA needs its own
# overlay; everything else (AMD, Intel, none) uses the base file unmodified,
# so "not nvidia" is all this needs to decide, and is confirmed against the
# API once the real host registers (see "confirm vendor/index" below).
if [ -e /dev/nvidiactl ] || command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
  PRE_VENDOR="nvidia"
else
  PRE_VENDOR="non-nvidia"
fi
harness_note "pre_detected_vendor" "$PRE_VENDOR"

COMPOSE_FILES=(-f "$ROOT/deploy/docker-compose.yml")
[ "$PRE_VENDOR" = "nvidia" ] && COMPOSE_FILES+=(-f "$ROOT/deploy/docker-compose.nvidia.yml")

NESTED_ENGINE_STARTED=0
STACK_UP=0
FIXTURE_IMAGE_TAG="${RID}-fixture:local"
RELAY_STARTED=0
RELAY_LISTEN_PORT=$((CONTROL_PORT + 2000))
RELAY_CONTROL_PORT=$((CONTROL_PORT + 3000))
RELAY_CONTROL="http://127.0.0.1:${RELAY_CONTROL_PORT}"
ADMIN_TOK=""
NONADMIN_TOK=""
ALL_CHECK_IDS_FILE="$WORKDIR/all-check-ids.txt"
: >"$ALL_CHECK_IDS_FILE"
NEEDS_DEVICE_OVERRIDE=0
CREATED_DEV_INPUT=0
PREFLIGHT_FOREIGN_NAMES_FILE="$WORKDIR/preflight-foreign-names.txt"
PREFLIGHT_VOLUMES_FILE="$WORKDIR/preflight-volumes.txt"
: >"$PREFLIGHT_FOREIGN_NAMES_FILE"
# Agent host runtime path (compose line: - /run/quasar-agent:/run/quasar-agent,
# a fixed bind mount, not a variable). Root-owned on every host seen so far —
# reuse the sudo this script already runs everything else through rather than
# adding a container-mount fallback. RUNTIME_DIR_READABLE tracks whether BOTH
# the preflight and the post-teardown listing succeeded; row 9 reports
# unperformed (never a pass) when either read fails.
AGENT_RUNTIME_DIR="/run/quasar-agent"
PREFLIGHT_RUNTIME_DIR_FILE="$WORKDIR/preflight-runtime-dir.txt"
RUNTIME_DIR_READABLE=1

# ── HTTP helpers (status+body split, header capture for the Retry-After
#    assertions — same shape as run-admission.sh's http_json). ──────────────
http_raw() { # $1 METHOD $2 /v1-relative-path $3 bearer-or-empty [$4 JSON body]
  local method="$1" path="$2" tok="$3" body="${4:-}"
  local hdrfile
  hdrfile="$(mktemp "$WORKDIR/hdr.XXXXXX")"
  local out status
  if [ -n "$body" ]; then
    out=$(curl -sS --connect-timeout 5 --max-time 20 -D "$hdrfile" \
      -w '\n__STATUS__%{http_code}' -X "$method" "$API/v1/$path" \
      ${tok:+-H "Authorization: Bearer $tok"} -H 'Content-Type: application/json' -d "$body")
  else
    out=$(curl -sS --connect-timeout 5 --max-time 20 -D "$hdrfile" \
      -w '\n__STATUS__%{http_code}' -X "$method" "$API/v1/$path" \
      ${tok:+-H "Authorization: Bearer $tok"})
  fi
  status="${out##*__STATUS__}"
  printf '%s\t%s\t%s\n' "$status" "$hdrfile" "${out%$'\n'__STATUS__*}"
}
http_status() { echo "$1" | cut -f1; }
http_hdrfile() { echo "$1" | cut -f2; }
http_body() { echo "$1" | cut -f3-; }
http_has_header() { grep -qi "^$2:" "$(http_hdrfile "$1")" 2>/dev/null; } # $1 raw-triple $2 header
json_get() {
  python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    for k in '$1'.split('.'):
        d = d[k] if not k.isdigit() else d[int(k)]
    print(d if d is not None else '')
except Exception:
    print('')
"
}

# poll_until <seconds> <interval> <cmd...> — bounded poll, never a blind sleep
# longer than the poll interval (spec "Polling").
poll_until() {
  local bound="$1" interval="$2"
  shift 2
  local waited=0
  while [ "$waited" -lt "$bound" ]; do
    if "$@"; then
      return 0
    fi
    sleep "$interval"
    waited=$((waited + interval))
  done
  return 1
}

# ══════════════════════════════════════════════════════════════════════════
# Small jq-based helpers factoring out the repeated inline
# `bash -c "curl … | python3 -c …"` polls, and freshness of
# readiness_reported_at relative to a recreate/injection time so a stale
# stored report from before it cannot satisfy a wait.
# ══════════════════════════════════════════════════════════════════════════
host_json() { http_body "$(http_raw GET "hosts/$1" "$ADMIN_TOK")"; } # $1 host_id -> host body JSON (top-level "host")
hosts_list_json() { http_body "$(http_raw GET hosts "$ADMIN_TOK")"; }

host_id_by_node_name() { # $1 node_name -> id or empty
  hosts_list_json | jq -r --arg n "$1" '.items[]? | select(.node_name==$n) | .id' | head -1
}
host_status() { host_json "$1" | jq -r '.host.status // ""'; }
iso_to_epoch() { python3 -c "
import sys, datetime
v = sys.stdin.read().strip()
try:
    print(int(datetime.datetime.fromisoformat(v.replace('Z', '+00:00')).timestamp()) if v else 0)
except Exception:
    print(0)
"; }
host_reported_at_epoch() { host_json "$1" | jq -r '.host.readiness_reported_at // empty' | iso_to_epoch; }
check_field() { host_json "$1" | jq -r --arg id "$2" --arg f "$3" '.host.readiness[]? | select(.id==$id) | (.[$f] // "" | tostring)'; } # $1 host $2 check_id $3 field
gate_state() { host_json "$1" | jq -r '.host.readiness_gate.state // ""'; }
# "absent" (never a number) when the host body carries no readiness_gate: a
# control plane without the gate must not read as one with nothing blocking.
blocking_len() { host_json "$1" | jq -r '.host.readiness_gate.blocking | if type == "array" then length else "absent" end'; }
# blocking_count_of <host> <check_id> — entries for one check, "absent" without a gate
blocking_count_of() { host_json "$1" | jq -r --arg id "$2" '.host.readiness_gate.blocking | if type == "array" then ([.[] | select(.check_id==$id)] | length) else "absent" end'; }

# wait_check_status <host_id> <check_id> <status> <bound> [since_epoch]
# Polls until the named check reports the given status AND (if since_epoch
# given) readiness_reported_at is strictly newer than it, so a report stored
# before a fault injection/recovery cannot satisfy the wait.
wait_check_status() {
  local host="$1" check="$2" want="$3" bound="$4" since="${5:-0}"
  local waited=0 st ra
  [ "$host" = "${REAL_HOST_ID:-}" ] && [ "$RECREATED_AT" -gt "$since" ] && since="$RECREATED_AT"
  while [ "$waited" -lt "$bound" ]; do
    st=$(check_field "$host" "$check" status)
    ra=$(host_reported_at_epoch "$host")
    if [ "$st" = "$want" ] && [ "$ra" -gt "$since" ]; then
      return 0
    fi
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
wait_gate_state() { # $1 host $2 state $3 bound
  local waited=0
  while [ "$waited" -lt "$3" ]; do
    [ "$(gate_state "$1")" = "$2" ] && return 0
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
wait_blocking_empty() { # $1 host $2 bound
  local waited=0
  while [ "$waited" -lt "$2" ]; do
    [ "$(blocking_len "$1")" = "0" ] && return 0
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
wait_blocking_has() { # $1 host $2 check_id $3 bound
  local waited=0
  while [ "$waited" -lt "$3" ]; do
    [ "$(blocking_len "$1")" != "0" ] && host_json "$1" | jq -e --arg id "$2" '.host.readiness_gate.blocking[]? | select(.check_id==$id)' >/dev/null 2>&1 && return 0
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
wait_host_online() { # $1 host $2 bound
  local waited=0
  while [ "$waited" -lt "$2" ]; do
    [ "$(host_status "$1")" = "online" ] && return 0
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
wait_host_registered() { # $1 node_name $2 bound -> echoes host id on success
  local waited=0 hid
  while [ "$waited" -lt "$2" ]; do
    hid=$(host_id_by_node_name "$1")
    if [ -n "$hid" ]; then
      echo "$hid"
      return 0
    fi
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
now_epoch() { date -u +%s; }

launch_app() { local raw; raw=$(http_raw POST sessions "$2" "{\"app_id\":\"$1\"}"); printf '%s\t%s\n' "$(http_status "$raw")" "$(http_body "$raw")"; } # $1 app_id $2 token
launch_diag() { # $1 launch result (status\tbody) -> " code=<error.code> blocking=[ids]" for a failure message
  local code blocking=""
  code=$(printf '%s' "${1#*$'\t'}" | json_get error.code)
  [ -n "${REAL_HOST_ID:-}" ] && blocking=$(host_json "$REAL_HOST_ID" | jq -c '[.host.readiness_gate.blocking[]? | .check_id]' 2>/dev/null || true)
  local hoststate=""
  [ -n "${REAL_HOST_ID:-}" ] && hoststate=$(host_json "$REAL_HOST_ID" | jq -c '.host | {status, gate: .readiness_gate.state, capacity_detection}' 2>/dev/null || true)
  printf ' code=%s real_host_blocking=%s real_host=%s gpus=%s' "${code:-none}" "${blocking:-unknown}" "${hoststate:-unknown}" \
    "$(http_body "$(http_raw GET "hosts/${REAL_HOST_ID:-none}/gpus" "$ADMIN_TOK")" | jq -c '[.items[]? | {gpu_index, slots_total, slots_reserved, vram_mb_total}]' 2>/dev/null || true)"
}
launch_app_raw() { http_raw POST sessions "$2" "{\"app_id\":\"$1\"}"; } # keeps headers, for Retry-After assertions
LAST_SESSION_DIAG=""
wait_session_state() { # $1 session_id $2 state $3 bound_secs — on timeout leaves the session's last facts in LAST_SESSION_DIAG
  local waited=0 body=""
  while [ "$waited" -lt "$3" ]; do
    body=$(http_body "$(http_raw GET "sessions/$1" "$ADMIN_TOK")")
    [ "$(printf '%s' "$body" | json_get session.state)" = "$2" ] && return 0
    sleep 3
    waited=$((waited + 3))
  done
  LAST_SESSION_DIAG=$(printf '%s' "$body" | jq -c '.session | {state, state_detail, error_message, failure_code}' 2>/dev/null || true)
  return 1
}
stop_and_wait() { # $1 session_id
  curl -sS -X DELETE "$API/v1/sessions/$1" -H "Authorization: Bearer $ADMIN_TOK" >/dev/null 2>&1 || true
  wait_session_state "$1" stopped 90 || true
}

collect_readiness_checks() { # $1 host_id — appends every readiness check this host reports (scenario 8)
  local hid="$1"
  [ -n "$hid" ] || return 0
  host_json "$hid" | jq -c '.host.readiness[]?' >>"$ALL_CHECK_IDS_FILE" 2>/dev/null || true
}
# A scan over the collected checks proves nothing when the collection is empty
# or thin: fewer ids than any real agent reports means the collector failed.
MIN_CHECK_CORPUS=10
check_corpus_size() { jq -r '.id // empty' "$ALL_CHECK_IDS_FILE" 2>/dev/null | sort -u | wc -l; }

# assert_names_no_check <label> <text> — pass/fail/unperformed for "this text
# names no readiness check", against every check id any host reported so far.
assert_names_no_check() {
  local label="$1" text="$2" n named
  n=$(check_corpus_size)
  if [ "$n" -lt "$MIN_CHECK_CORPUS" ]; then
    unperformed "$label: only $n check id(s) were collected, too few to judge the message against"
    return
  fi
  named=$(jq -r '.id // empty' "$ALL_CHECK_IDS_FILE" | sort -u | while IFS= read -r cid; do
    case "$text" in *"$cid"*) printf '%s ' "$cid" ;; esac
  done)
  if [ -z "$named" ]; then
    pass "$label: the message names none of the $n check ids reported in this run"
  else
    fail "$label: the message names check id(s): $named"
  fi
}

dedupe_check_ids_file() {
  sort -u "$ALL_CHECK_IDS_FILE" -o "$ALL_CHECK_IDS_FILE" 2>/dev/null || true
}

# recreate_agent_with_override [override files...] — recreate ONLY the agent and
# block until the NEW process has registered. RECREATED_AT is then the floor
# for every freshness wait: the old agent can send one last report between a
# caller's `since` and its own death, and a launch fired on that report lands
# while no agent is connected (seen live as a spurious no_host_available).
RECREATED_AT=0
recreate_agent_with_override() {
  local args=(-p "$RID" --env-file "$ENV_FILE" "${COMPOSE_FILES[@]}" -f "$BASE_OVERRIDE")
  local f t0 waited=0 reg
  for f in "$@"; do
    args+=(-f "$f")
  done
  t0=$(now_epoch)
  docker compose "${args[@]}" up -d --force-recreate --no-deps quasar-node-agent || return 1
  [ -n "${REAL_HOST_ID:-}" ] || return 0
  while [ "$waited" -lt 120 ]; do
    reg=$(host_json "$REAL_HOST_ID" | jq -r '.host.last_registered_at // empty' | iso_to_epoch)
    # #288: registered != reported — capacity_detection only flips to "ok" once
    # the new process's first capacity report lands, so a wait on registration
    # alone can launch into the pre-report window and see a spurious
    # no_host_available.
    if [ "${reg:-0}" -ge "$t0" ] && [ "$(host_status "$REAL_HOST_ID")" = "online" ] \
      && [ "$(host_json "$REAL_HOST_ID" | jq -r '.host.capacity_detection // empty')" = "ok" ]; then
      RECREATED_AT=$(now_epoch)
      return 0
    fi
    sleep 3
    waited=$((waited + 3))
  done
  return 1
}
# start_real_agent — bring the stopped real agent back and do not return until
# the NEW process has registered and nothing blocks it, so the next scenario
# never launches into an agent that is still booting (on NVIDIA the first
# ~20 s are driver-volume adoption). Sets RECREATED_AT like a recreate does.
start_real_agent() {
  local t0 waited=0 reg
  t0=$(now_epoch)
  compose_cmd start quasar-node-agent >/dev/null 2>&1 || true
  while [ "$waited" -lt 120 ]; do
    reg=$(host_json "$REAL_HOST_ID" | jq -r '.host.last_registered_at // empty' | iso_to_epoch)
    # #288: registered != reported — see recreate_agent_with_override.
    if [ "${reg:-0}" -ge "$t0" ] && [ "$(host_status "$REAL_HOST_ID")" = "online" ] \
      && [ "$(host_json "$REAL_HOST_ID" | jq -r '.host.capacity_detection // empty')" = "ok" ]; then
      RECREATED_AT=$(now_epoch)
      break
    fi
    sleep 3
    waited=$((waited + 3))
  done
  wait_blocking_empty "$REAL_HOST_ID" 180 || true
}
compose_cmd() { docker compose -p "$RID" --env-file "$ENV_FILE" "${COMPOSE_FILES[@]}" -f "$BASE_OVERRIDE" "$@"; }

build_fixture_image() {
  cat >"$WORKDIR/Dockerfile.fixture" <<'DOCKER'
FROM golang:1.25
WORKDIR /src
COPY . .
RUN go build -o /usr/local/bin/readiness-fixture .
ENTRYPOINT ["/usr/local/bin/readiness-fixture"]
DOCKER
  docker build --label "$OWNER_LABEL" -t "$FIXTURE_IMAGE_TAG" \
    -f "$WORKDIR/Dockerfile.fixture" "$ROOT/scripts/harness/readiness-fixture" >/dev/null 2>&1
}

relay_rule() { curl -sS -X PUT "$RELAY_CONTROL/rule" -H 'Content-Type: application/json' -d "$1" >/dev/null 2>&1; } # $1 JSON rule body
relay_stats() { curl -sS "$RELAY_CONTROL/stats" 2>/dev/null || echo '{}'; } # diagnostic only — never used to judge a wait

# The fixture image is built once, up front, and is fatal-on-failure for
# the whole run — so by the time any scenario runs, `readiness-fixture host`
# is known to exist. This helper is kept only as a defensive per-call guard
# in case a scenario runs against an image built from a tree whose fixture
# predates the `host` subcommand.
fixture_supports_host() {
  local rc=0
  docker run --rm --entrypoint /usr/local/bin/readiness-fixture "$FIXTURE_IMAGE_TAG" host --help >/dev/null 2>&1 || rc=$?
  # `host` with no required flags exits non-zero on `--help` too (flag package
  # prints usage to stderr and returns a non-zero status) — treat "the binary
  # recognizes the subcommand at all" (rc != 127, not "command not found") as
  # supported, since 127 is docker's own exec-failure code.
  [ "$rc" != "127" ]
}

# ── Delete every $RID-* host row via the API, asserting 204 each,
# rather than relying only on locally-tracked ids (a scripted/nested host
# that this run created may not be in a local array on every code path). ────
delete_all_rid_hosts() {
  local ids
  ids=$(hosts_list_json | jq -r --arg p "$RID-" '.items[]? | select(.node_name | startswith($p)) | "\(.id)\t\(.node_name)"' 2>/dev/null || true)
  [ -n "$ids" ] || return 0
  local hid name raw
  while IFS=$'\t' read -r hid name; do
    [ -n "$hid" ] || continue
    raw=$(http_raw DELETE "hosts/$hid" "$ADMIN_TOK")
    if [ "$(http_status "$raw")" = "204" ]; then
      pass "9: host row $name deleted 204"
    else
      fail "9: host row $name delete got HTTP $(http_status "$raw") (want 204)"
    fi
  done <<<"$ids"
}

# ── Preflight (spec "Preflight before ANY mutation") ────────────────────────
preflight_cohabit_check() {
  local foreign
  foreign=$(docker ps --format '{{.Label "com.docker.compose.project"}}' 2>/dev/null \
    | { grep -v '^$' || true; } | sort -u | while read -r p; do
      # an empty engine is the state this harness wants: under pipefail a grep
      # that matches nothing must not end the run
      if docker inspect "$(docker ps -q --filter "label=com.docker.compose.project=$p" | head -1)" 2>/dev/null \
        | jq -r '.[0].Config.Image // ""' | grep -qi 'quasar'; then
        echo "$p"
      fi
    done | wc -l)
  harness_note "preflight_foreign_quasar_projects" "$foreign"
  if [ "${foreign:-0}" -gt 0 ] && [ "$ALLOW_COHABIT" != "1" ]; then
    fail "preflight: $foreign foreign Quasar-looking compose project(s) already on this engine (rerun with --allow-cohabit)"
    exit 1
  fi
  if ss -ltn 2>/dev/null | grep -q ":${CONTROL_PORT} "; then
    fail "preflight: port $CONTROL_PORT is already bound"
    exit 1
  fi
  if [ -e "$RID_ROOT" ]; then
    fail "preflight: $RID_ROOT already exists (RID collision?)"
    exit 1
  fi
  # Snapshot of pre-existing container names, so scenario 9's sweep for
  # quasar-sess-*/quasar-pulse-*/quasar-probe-* under --allow-cohabit compares
  # against this baseline instead of assuming the engine started empty.
  docker ps -a --format '{{.Names}}' 2>/dev/null >"$PREFLIGHT_FOREIGN_NAMES_FILE" || true
  # Volumes too: an anonymous volume (docker:dind declares one) carries no
  # owner label, so only a before/after comparison can prove none was left.
  docker volume ls -q 2>/dev/null | sort >"$PREFLIGHT_VOLUMES_FILE" || true
  # Agent host runtime path: killed-agent scenarios (mid-session, mid-probe)
  # can leave udev-<session id> / quasar-media-probe-* directories under the
  # fixed bind mount that no label or volume-diff check sees. Snapshot its
  # entries now so row 9 can tell what the run added. A host that has never
  # run the agent may not have created the directory yet — that is a clean
  # (empty) starting point, not an unreadable one; compose creates it on
  # first `up`.
  if [ ! -e "$AGENT_RUNTIME_DIR" ]; then
    : >"$PREFLIGHT_RUNTIME_DIR_FILE"
  elif sudo find "$AGENT_RUNTIME_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null \
    | sort >"$PREFLIGHT_RUNTIME_DIR_FILE"; then
    :
  else
    RUNTIME_DIR_READABLE=0
    : >"$PREFLIGHT_RUNTIME_DIR_FILE"
    harness_note "preflight_runtime_dir_unreadable" "$AGENT_RUNTIME_DIR exists but could not be listed at preflight (root required)"
  fi
  pass "preflight: engine clear (foreign_quasar_projects=$foreign, port $CONTROL_PORT free, $RID_ROOT free)"
}
preflight_cohabit_check

# ── Assert the agent image carries no fixture leakage (spec "Why the
#    synthetic check cannot reach production", point 3). ───────────────────
assert_agent_image_clean() {
  local cid strings_out
  cid=$(docker create "$AGENT_IMAGE" true 2>/dev/null) || {
    unperformed "image-clean: could not create a container from $AGENT_IMAGE to inspect it"
    return
  }
  if docker export "$cid" 2>/dev/null | tar -tv 2>/dev/null | grep -qi 'readiness-fixture'; then
    fail "image-clean: agent image $AGENT_IMAGE contains a readiness-fixture path"
  else
    pass "image-clean: agent image carries no readiness-fixture path"
  fi
  local bin
  bin=$(docker run --rm --entrypoint sh "$AGENT_IMAGE" -c 'command -v quasar-node-agent || echo /usr/local/bin/quasar-node-agent' 2>/dev/null | tail -1)
  # grep -a reads the binary directly: no dependence on `strings` being in the
  # image, and an unreadable binary is unperformed, never an empty-output pass.
  strings_out=$(docker run --rm --entrypoint sh "$AGENT_IMAGE" -c "[ -r '$bin' ] && { grep -a -c harness_synthetic_ '$bin' || true; }" 2>/dev/null || echo "")
  if [ -z "$strings_out" ]; then
    unperformed "image-clean: could not read the agent binary at '$bin' inside $AGENT_IMAGE"
  elif [ "$strings_out" = "0" ]; then
    pass "image-clean: agent binary has no harness_synthetic_ string"
  else
    fail "image-clean: agent binary contains harness_synthetic_ ($strings_out matching line(s))"
  fi
  docker rm -f "$cid" >/dev/null 2>&1 || true
}
assert_agent_image_clean

# ── /dev/kmsg / /dev/input auto-detect (LXC test hosts lack both) —
# only remove /dev/input in cleanup if THIS run created it, and only if empty.
if [ ! -e /dev/kmsg ]; then
  NEEDS_DEVICE_OVERRIDE=1
  if [ ! -e /dev/input ]; then
    sudo mkdir -p /dev/input
    CREATED_DEV_INPUT=1
    harness_note "host_residue" "/dev/input directory (created by this run; removed in cleanup if still empty)"
  fi
  harness_note "device_override" "true (no /dev/kmsg on this host)"
else
  harness_note "device_override" "false"
fi

# ── Homes root: harness-owned size-limited tmpfs under /var/lib/$RID ────────
sudo mkdir -p "$RID_ROOT"
HOMES_ROOT="$RID_ROOT/homes"
sudo mkdir -p "$HOMES_ROOT"
sudo mount -t tmpfs -o size=256m "$OWNER_LABEL" "$HOMES_ROOT" 2>/dev/null \
  || sudo mount -t tmpfs -o size=256m tmpfs "$HOMES_ROOT"
TEMPLATE_ROOT="$RID_ROOT/templates"
sudo mkdir -p "$TEMPLATE_ROOT"
sudo chown -R "$(id -u):$(id -g)" "$RID_ROOT" 2>/dev/null || true

# ── Build the fixture image and start the relay BEFORE `compose up` —
# fatal for the whole run if the image cannot be built (not per-scenario
# unperformed): every scenario from 2 onward launches through it once the
# base override below points the real agent at it from first boot. ─────────
echo "== building the readiness-fixture image =="
if ! build_fixture_image; then
  fail "fixture: could not build the readiness-fixture image — fatal, cannot proceed"
  finish_run 1
fi
pass "fixture: readiness-fixture image built"

echo "== starting the relay (whole-run, rule off = transparent) =="
if ! docker run -d --label "$OWNER_LABEL" --name "${RID}-relay" --network host \
  "$FIXTURE_IMAGE_TAG" relay --listen "127.0.0.1:${RELAY_LISTEN_PORT}" \
  --upstream "ws://127.0.0.1:${CONTROL_PORT}/agent/ws" \
  --control "127.0.0.1:${RELAY_CONTROL_PORT}" >/dev/null 2>&1; then
  fail "fixture: could not start the relay — fatal, cannot proceed"
  finish_run 1
fi
RELAY_STARTED=1
if ! poll_until 30 2 curl -sS -o /dev/null "$RELAY_CONTROL/healthz"; then
  fail "fixture: relay never answered /healthz — fatal, cannot proceed"
  finish_run 1
fi
relay_rule '{"mode":"off"}'
pass "fixture: relay running, rule off (transparent)"

# ── Base compose override: ownership labels, homes/template roots, device
#    fix, content-addressed images, and the real agent pointed at the
#    relay from first boot. ──────────────────────────────────────────────────
BASE_OVERRIDE="$WORKDIR/override.base.yml"
cat >"$BASE_OVERRIDE" <<YAML
services:
  quasar-postgres:
    labels: ["$OWNER_LABEL"]
  quasar-control-plane:
    labels: ["$OWNER_LABEL"]
  quasar-node-agent:
    labels: ["$OWNER_LABEL"]
    environment:
      NODE_NAME: "${RID}-real"
      CONTROL_PLANE_URL: "ws://127.0.0.1:${RELAY_LISTEN_PORT}"
      QUASAR_HOME_ROOT: "$HOMES_ROOT"
      QUASAR_TEMPLATE_ROOT: "$TEMPLATE_ROOT"
YAML
if [ "$NEEDS_DEVICE_OVERRIDE" = "1" ]; then
  cat >>"$BASE_OVERRIDE" <<'YAML'
    devices: !override
      - /dev/dri
      - /dev/uinput
YAML
fi

ENV_FILE="$WORKDIR/env"
cat >"$ENV_FILE" <<ENV
CONTROL_PORT=$CONTROL_PORT
QUASAR_TLS=off
QUASAR_CONTROL_IMAGE=$CONTROL_IMAGE
QUASAR_AGENT_IMAGE=$AGENT_IMAGE
POSTGRES_PASSWORD=${RID}-pg
ENROLLMENT_TOKEN=$ENROLLMENT_TOKEN
BOOTSTRAP_ADMIN_EMAIL=$ADMIN_EMAIL
BOOTSTRAP_ADMIN_USERNAME=${RID}-admin
BOOTSTRAP_ADMIN_PASSWORD=$ADMIN_PASS
QUASAR_HOME_ROOT=$HOMES_ROOT
QUASAR_TEMPLATE_ROOT=$TEMPLATE_ROOT
ENV

# ── Cleanup (idempotent; verified by scenario 9) ────────────────────────────
# Order: stop sessions and wait stopped -> stop every agent/scripted
# host/nested agent -> delete every $RID-* host row via the API (asserting
# 204 each) -> compose down -v -> label sweep -> umount/rm -> verify.
CREATED_SESSION_IDS=()
cleanup() {
  local rc=$?
  echo ""
  echo "== cleanup (RID=$RID) =="
  if [ "$KEEP" = "1" ]; then
    echo "  --keep given: skipping teardown; scenario 9 is unperformed"
    unperformed "9: --keep given, teardown skipped"
    finish_run "$rc"
  fi

  local sid
  for sid in "${CREATED_SESSION_IDS[@]:-}"; do
    [ -n "$sid" ] || continue
    curl -sS --connect-timeout 5 --max-time 15 -X DELETE "$API/v1/sessions/$sid" \
      -H "Authorization: Bearer ${ADMIN_TOK:-}" >/dev/null 2>&1 || true
  done
  for sid in "${CREATED_SESSION_IDS[@]:-}"; do
    [ -n "$sid" ] || continue
    poll_until 60 2 bash -c "curl -sS -H 'Authorization: Bearer ${ADMIN_TOK:-}' '$API/v1/sessions/$sid' | grep -q '\"stopped\"'" || true
  done

  # Stop every agent/scripted-host/nested-agent container this run may have
  # started, BEFORE deleting host rows (an online host's row delete is
  # refused by the contract).
  docker ps -aq --filter "label=$OWNER_LABEL" --filter "name=${RID}-host-" 2>/dev/null | xargs -r docker stop >/dev/null 2>&1 || true
  if [ "$NESTED_ENGINE_STARTED" = "1" ]; then
    docker exec "${RID}-dind" docker stop "${RID}-nested-agent" >/dev/null 2>&1 || true
  fi
  if [ "$STACK_UP" = "1" ]; then
    compose_cmd stop quasar-node-agent >/dev/null 2>&1 || true
  fi

  if [ -n "${ADMIN_TOK:-}" ]; then
    delete_all_rid_hosts
  fi

  if [ "$NESTED_ENGINE_STARTED" = "1" ]; then
    docker rm -f -v "${RID}-dind" >/dev/null 2>&1 || true
  fi
  if [ "$RELAY_STARTED" = "1" ]; then
    docker rm -f "${RID}-relay" >/dev/null 2>&1 || true
  fi
  if [ "$STACK_UP" = "1" ]; then
    compose_cmd down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  docker ps -aq --filter "label=$OWNER_LABEL" 2>/dev/null | xargs -r docker rm -f -v >/dev/null 2>&1 || true
  docker volume ls -q --filter "label=$OWNER_LABEL" 2>/dev/null | xargs -r docker volume rm -f >/dev/null 2>&1 || true
  docker network ls -q --filter "label=$OWNER_LABEL" 2>/dev/null | xargs -r docker network rm >/dev/null 2>&1 || true
  docker rmi -f "$FIXTURE_IMAGE_TAG" >/dev/null 2>&1 || true

  sudo umount "$HOMES_ROOT" >/dev/null 2>&1 || true
  sudo rm -rf "$RID_ROOT" >/dev/null 2>&1 || true
  # Remove /dev/input only if THIS run created it and it is still empty.
  if [ "$CREATED_DEV_INPUT" = "1" ] && [ -d /dev/input ] && [ -z "$(ls -A /dev/input 2>/dev/null)" ]; then
    sudo rmdir /dev/input 2>/dev/null || true
  fi
  local new_volumes
  new_volumes=$(docker volume ls -q 2>/dev/null | sort | comm -13 "$PREFLIGHT_VOLUMES_FILE" - 2>/dev/null | wc -l)

  # ── Agent host runtime path (compose's fixed /run/quasar-agent bind mount,
  # not a volume — no label or volume-diff check above reaches it). Killed-
  # agent scenarios can leave udev-<session id> / quasar-media-probe-*
  # directories there. Must run BEFORE $WORKDIR is removed: the preflight
  # snapshot lives under it. Reads need root, like everything else this
  # script mutates under $RID_ROOT — reuse sudo rather than adding a
  # container-mount fallback.
  if [ "$RUNTIME_DIR_READABLE" = "1" ]; then
    local post_runtime_dir_file new_runtime_entries entry sid attributable created remaining
    post_runtime_dir_file="$(mktemp "$WORKDIR/post-runtime-dir.XXXXXX" 2>/dev/null || mktemp)"
    if sudo find "$AGENT_RUNTIME_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null \
      | sort >"$post_runtime_dir_file"; then
      mapfile -t new_runtime_entries < <(comm -13 "$PREFLIGHT_RUNTIME_DIR_FILE" "$post_runtime_dir_file")
      for entry in "${new_runtime_entries[@]:-}"; do
        [ -n "$entry" ] || continue
        attributable=0
        if [ "$ALLOW_COHABIT" != "1" ]; then
          # preflight proved the harness's stack was the only Quasar stack on
          # this engine, so every entry created since is ours.
          attributable=1
        else
          case "$entry" in
            udev-*)
              sid="${entry#udev-}"
              for created in "${CREATED_SESSION_IDS[@]:-}"; do
                [ "$sid" = "$created" ] && attributable=1 && break
              done
              ;;
          esac
        fi
        if [ "$attributable" = "1" ]; then
          sudo rm -rf "${AGENT_RUNTIME_DIR:?}/${entry:?}" 2>/dev/null || true
        fi
      done
      if sudo find "$AGENT_RUNTIME_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null \
        | sort | comm -13 "$PREFLIGHT_RUNTIME_DIR_FILE" - >"$post_runtime_dir_file.final" 2>/dev/null; then
        remaining=$(wc -l <"$post_runtime_dir_file.final")
        if [ "$remaining" = "0" ]; then
          pass "9: no entry under $AGENT_RUNTIME_DIR that was not there at preflight (after removing harness-attributable leftovers)"
        elif [ "$ALLOW_COHABIT" = "1" ]; then
          # Another Quasar stack shares this engine and this fixed path, so an
          # entry this run cannot attribute to itself may be that stack's.
          unperformed "9: $remaining new entry/entries under $AGENT_RUNTIME_DIR that this run cannot attribute to itself under --allow-cohabit"
        else
          fail "9: $remaining entry/entries under $AGENT_RUNTIME_DIR not there at preflight"
        fi
        rm -f "$post_runtime_dir_file.final"
      else
        unperformed "9: $AGENT_RUNTIME_DIR became unreadable while verifying cleanup"
      fi
    else
      unperformed "9: $AGENT_RUNTIME_DIR could not be read to verify cleanup"
    fi
  else
    unperformed "9: $AGENT_RUNTIME_DIR was not readable at preflight, cannot verify runtime-path cleanup"
  fi

  rm -rf "$WORKDIR" >/dev/null 2>&1 || true

  # ── Scenario 9 verification ────────────────────────────────────────────
  local leftover_c leftover_v leftover_n
  leftover_c=$(docker ps -aq --filter "label=$OWNER_LABEL" 2>/dev/null | wc -l)
  leftover_v=$(docker volume ls -q --filter "label=$OWNER_LABEL" 2>/dev/null | wc -l)
  leftover_n=$(docker network ls -q --filter "label=$OWNER_LABEL" 2>/dev/null | wc -l)
  if [ "$leftover_c" = "0" ] && [ "$leftover_v" = "0" ] && [ "$leftover_n" = "0" ] && [ ! -e "$RID_ROOT" ]; then
    pass "9: ownership cleanup — zero labelled containers/volumes/networks, $RID_ROOT gone"
  else
    fail "9: ownership cleanup incomplete (containers=$leftover_c volumes=$leftover_v networks=$leftover_n path_exists=$([ -e "$RID_ROOT" ] && echo yes || echo no))"
  fi

  if [ "${new_volumes:-0}" = "0" ]; then
    pass "9: no volume exists that was not there at preflight (labelled or anonymous)"
  else
    fail "9: $new_volumes volume(s) exist that were not there at preflight"
  fi

  # Agent-created session/pulse/probe containers: valid to assert zero
  # survive because preflight proved (or, under --allow-cohabit, snapshotted)
  # the engine's pre-existing container names.
  local survivors
  if [ "$ALLOW_COHABIT" = "1" ]; then
    survivors=$(docker ps -a --format '{{.Names}}' 2>/dev/null \
      | grep -E '^(quasar-sess-|quasar-pulse-|quasar-probe-)' \
      | grep -vFc -f "$PREFLIGHT_FOREIGN_NAMES_FILE" 2>/dev/null || true)
  else
    survivors=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -cE '^(quasar-sess-|quasar-pulse-|quasar-probe-)' || true)
  fi
  if [ "${survivors:-0}" = "0" ]; then
    pass "9: zero quasar-sess-*/quasar-pulse-*/quasar-probe-* containers remain"
  else
    fail "9: $survivors quasar-sess-*/quasar-pulse-*/quasar-probe-* container(s) still present"
  fi

  finish_run "$rc"
}
trap cleanup EXIT

# ── Bring the stack up — ONLY the three services under test: never
# quasar-updater, which touches nothing this harness exercises. ────────────
echo "== starting stack (RID=$RID, compose files: ${COMPOSE_FILES[*]}) =="
compose_cmd up -d quasar-postgres quasar-control-plane quasar-node-agent
STACK_UP=1

wait_for_control_plane() { curl -sS --connect-timeout 2 --max-time 5 -o /dev/null "$API/health"; }
if ! poll_until 120 3 wait_for_control_plane; then
  fail "stack: control plane never answered /health within 120s"
  exit 1
fi
pass "stack: control plane answering /health"

# ── Login ────────────────────────────────────────────────────────────────────
LOGIN_RAW=$(http_raw POST auth/login "" "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}")
ADMIN_TOK=$(http_body "$LOGIN_RAW" | json_get access_token)
[ -n "$ADMIN_TOK" ] || { fail "login: admin token not obtained"; exit 1; }
pass "login: admin token obtained"

# Image source-commit labels (org.quasar.source.commit) and the real
# schema version (control-plane's own Postgres schema_migrations.version) —
# kept as two separate, correctly-named facts rather than one guessed field.
CONTROL_IMAGE_COMMIT=$(docker inspect "$CONTROL_IMAGE" -f '{{index .Config.Labels "org.quasar.source.commit"}}' 2>/dev/null || echo "")
AGENT_IMAGE_COMMIT=$(docker inspect "$AGENT_IMAGE" -f '{{index .Config.Labels "org.quasar.source.commit"}}' 2>/dev/null || echo "")
[ -n "$CONTROL_IMAGE_COMMIT" ] || CONTROL_IMAGE_COMMIT="unknown"
[ -n "$AGENT_IMAGE_COMMIT" ] || AGENT_IMAGE_COMMIT="unknown"
PG_CID=$(compose_cmd ps -q quasar-postgres 2>/dev/null || echo "")
if [ -n "$PG_CID" ]; then
  SCHEMA_MIGRATIONS_VERSION=$(docker exec -e PGPASSWORD="${RID}-pg" "$PG_CID" \
    psql -v ON_ERROR_STOP=1 -U quasar -d quasar -tAc "SELECT version FROM schema_migrations;" 2>/dev/null | tr -d ' \n' || echo "")
fi
[ -n "$SCHEMA_MIGRATIONS_VERSION" ] || SCHEMA_MIGRATIONS_VERSION="unknown"
harness_note "control_image_commit" "$CONTROL_IMAGE_COMMIT"
harness_note "agent_image_commit" "$AGENT_IMAGE_COMMIT"
harness_note "schema_migrations_version" "$SCHEMA_MIGRATIONS_VERSION"

# ── Non-admin user (invite-gated registration — same recipe as run-admission.sh) ─
provision_nonadmin() {
  local settings reg_mode invite_code
  settings=$(http_body "$(http_raw GET admin/settings "$ADMIN_TOK")")
  reg_mode=$(printf '%s' "$settings" | json_get settings.registration_mode)
  if [ "$reg_mode" != "invite_only" ] && [ "$reg_mode" != "open" ]; then
    http_raw PATCH admin/settings "$ADMIN_TOK" '{"registration_mode":"invite_only"}' >/dev/null
  fi
  invite_code=$(http_body "$(http_raw POST admin/invites "$ADMIN_TOK" '{"role":"user","max_uses":1}')" | json_get invite.code)
  local reg_raw
  reg_raw=$(http_raw POST auth/register "" "{\"email\":\"${RID}-user@quasar.local\",\"username\":\"${RID}user\",\"password\":\"Rh02User!123\",\"invite_code\":\"$invite_code\"}")
  [ "$(http_status "$reg_raw")" = "201" ] || { fail "nonadmin: register -> HTTP $(http_status "$reg_raw") $(http_body "$reg_raw")"; return 1; }
  NONADMIN_TOK=$(http_body "$(http_raw POST auth/login "" "{\"email\":\"${RID}-user@quasar.local\",\"password\":\"Rh02User!123\"}")" | json_get access_token)
  [ -n "$NONADMIN_TOK" ] || { fail "nonadmin: login did not return a token"; return 1; }
  pass "nonadmin: user provisioned"
}
provision_nonadmin || true

# ── Fixture apps: app-nohome, app-home (fedora:43 + sleep, default_vram_mb=256)
# Non-subshell: fail() called inside a `$(...)` subshell is invisible to
# the parent's counters, so this sets globals directly instead and treats a
# create failure as fatal (nothing downstream is meaningful without both apps).
APP_NOHOME_ID=""
APP_HOME_ID=""
create_app() { # $1 name $2 managed_home(true|false) -> sets REPLY_APP_ID
  local name="$1" managed_home="$2" body raw
  body="{\"name\":\"$name\",\"runtime_spec\":{\"image\":\"fedora:43\",\"args\":[\"sleep\",\"infinity\"],\"env\":{},\"mounts\":[],\"gpu\":true},\"managed_home\":$managed_home,\"default_encode_slots\":1,\"default_width\":1280,\"default_height\":720,\"default_fps\":30,\"default_bitrate_kbps\":4000,\"default_vram_mb\":256}"
  raw=$(http_raw POST apps "$ADMIN_TOK" "$body")
  if [ "$(http_status "$raw")" != "201" ]; then
    fail "seed: failed to create app '$name': $(http_body "$raw")"
    REPLY_APP_ID=""
    return 1
  fi
  REPLY_APP_ID=$(http_body "$raw" | json_get app.id)
  pass "seed: app '$name' ready ($REPLY_APP_ID)"
}
create_app "${RID}: app-nohome" false && APP_NOHOME_ID="$REPLY_APP_ID"
create_app "${RID}: app-home" true && APP_HOME_ID="$REPLY_APP_ID"
if [ -z "$APP_NOHOME_ID" ] || [ -z "$APP_HOME_ID" ]; then
  fail "seed: fixture app creation failed — cannot proceed with any launch-dependent scenario"
  exit 1
fi

# ── Baseline: require the REAL host to exist, be online, gate active,
# and blocking empty — an empty host list must never satisfy this (ensure all
# checks pass before launching, not just when the list is empty). ──────────────────────────────────────────────────────────
BASELINE_OK=0
REAL_HOST_ID=""
if REAL_HOST_ID=$(wait_host_registered "${RID}-real" 120); then
  if wait_host_online "$REAL_HOST_ID" 60 && wait_gate_state "$REAL_HOST_ID" active 60 && wait_blocking_empty "$REAL_HOST_ID" 180; then
    # A healthy pre-state is what gives every later fault its meaning: each
    # host probe must have RUN and passed, not merely be absent from blocking.
    for probe_id in input_probe audio_probe; do
      if wait_check_status "$REAL_HOST_ID" "$probe_id" pass 180; then
        pass "baseline: $probe_id passes before any fault"
      else
        fail "baseline: $probe_id is '$(check_field "$REAL_HOST_ID" "$probe_id" status)' before any fault (want pass)"
      fi
    done
    RES=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK")
    ST="${RES%%$'\t'*}"; BODY="${RES#*$'\t'}"
    if [ "$ST" = "201" ]; then
      SID=$(printf '%s' "$BODY" | json_get session.id)
      CREATED_SESSION_IDS+=("$SID")
      if wait_session_state "$SID" running 90; then
        pass "baseline: launch reached running"
        BASELINE_OK=1
        stop_and_wait "$SID"
      else
        fail "baseline: launch did not reach running within 90s — $LAST_SESSION_DIAG"
      fi
    else
      fail "baseline: launch failed HTTP $ST: $BODY"
    fi
  else
    fail "baseline: host never reached online+active-gate+empty-blocking within bound (pre-#264 image, or a real fault)"
  fi
else
  fail "baseline: real host (${RID}-real) never registered within 120s"
fi
harness_note "baseline_ok" "$BASELINE_OK"

# ── Confirm GPU vendor/index from the real host body (never assume index 0;
# cross-check against the pre-stack-up detection used to pick the compose
# overlay). ──────────────────────────────────────────────────────────────────
if [ -n "$REAL_HOST_ID" ]; then
  GPU_JSON=$(http_body "$(http_raw GET "hosts/$REAL_HOST_ID/gpus" "$ADMIN_TOK")" 2>/dev/null || echo '{}')
  GPU_VENDOR=$(printf '%s' "$GPU_JSON" | jq -r '.items[0].vendor // ""' 2>/dev/null || echo "")
  GPU_INDEX=$(printf '%s' "$GPU_JSON" | jq -r '(.items[0].index // .items[0].gpu_index // 0)' 2>/dev/null || echo 0)
fi
harness_note "gpu_vendor" "${GPU_VENDOR:-unknown}"
harness_note "gpu_index" "${GPU_INDEX:-0}"
if [ "$PRE_VENDOR" = "nvidia" ] && [ "${GPU_VENDOR:-}" != "nvidia" ]; then
  fail "vendor: pre-detected nvidia but the API reports vendor='${GPU_VENDOR:-}' — compose overlay mismatch"
fi

# ══════════════════════════════════════════════════════════════════════════
# Scenario 2 — homes root unwritable / exhausted
# ══════════════════════════════════════════════════════════════════════════
scenario_2a() {
  only_run 2a || return 0
  local since; since=$(now_epoch)
  sudo mount -o remount,ro "$HOMES_ROOT" || { unperformed "2a: could not remount $HOMES_ROOT read-only"; return; }
  if wait_check_status "$REAL_HOST_ID" homes_root_writable fail 90 "$since"; then
    collect_readiness_checks "$REAL_HOST_ID"
    local scope; scope=$(check_field "$REAL_HOST_ID" homes_root_writable blocks.scope 2>/dev/null)
    scope=$(host_json "$REAL_HOST_ID" | jq -r '.host.readiness[] | select(.id=="homes_root_writable") | .blocks.scope // ""')
    if [ "$scope" = "homes" ]; then pass "2a: homes_root_writable fails with scope homes"; else fail "2a: homes_root_writable scope='$scope' (want homes)"; fi
  else
    fail "2a: homes_root_writable never reported fail within 90s"
  fi
  local res st code
  res=$(launch_app "$APP_HOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"; code=$(printf '%s' "${res#*$'\t'}" | json_get error.code)
  if [ "$st" = "503" ] && [ "$code" = "host_not_ready" ]; then pass "2a: app-home launch refused 503 host_not_ready"; else fail "2a: app-home launch got HTTP $st code=$code (want 503 host_not_ready)"; fi
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 90; then pass "2a: app-nohome launch reaches running"; else fail "2a: app-nohome launch did not reach running — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "2a: app-nohome launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
  since=$(now_epoch)
  sudo mount -o remount,rw "$HOMES_ROOT" || true
  if wait_check_status "$REAL_HOST_ID" homes_root_writable pass 90 "$since"; then pass "2a: homes_root_writable recovers to pass"; else fail "2a: homes_root_writable did not recover to pass"; fi
  res=$(launch_app "$APP_HOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid2; sid2=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid2")
    if wait_session_state "$sid2" running 90; then pass "2a: app-home launch reaches running after recovery"; else fail "2a: app-home launch did not reach running after recovery — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid2"
  else
    fail "2a: app-home launch after recovery got HTTP $st (want 201)$(launch_diag "$res")"
  fi
}

scenario_2b() {
  only_run 2b || return 0
  local fill="$HOMES_ROOT/${RID}-fill" since; since=$(now_epoch)
  dd if=/dev/zero of="$fill" bs=1M count=512 2>/dev/null || true
  # dd exits non-zero on ENOSPC — that IS the expected way to reach zero
  # free on a tmpfs; judge readiness by actual free space, not dd's exit code.
  local avail
  avail=$(df --output=avail "$HOMES_ROOT" 2>/dev/null | tail -1 | tr -d ' ')
  if [ "${avail:-1}" != "0" ]; then
    unperformed "2b: could not fill homes tmpfs to zero free (avail=${avail:-unknown})"
    rm -f "$fill"
    return
  fi
  if wait_check_status "$REAL_HOST_ID" homes_free_space fail 90 "$since"; then
    collect_readiness_checks "$REAL_HOST_ID"
    local scope; scope=$(host_json "$REAL_HOST_ID" | jq -r '.host.readiness[] | select(.id=="homes_free_space") | .blocks.scope // ""')
    if [ "$scope" = "homes" ]; then pass "2b: homes_free_space fails with scope homes"; else fail "2b: homes_free_space scope='$scope' (want homes)"; fi
  else
    fail "2b: homes_free_space never reported fail within 90s"
  fi
  local res st code
  res=$(launch_app "$APP_HOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"; code=$(printf '%s' "${res#*$'\t'}" | json_get error.code)
  if [ "$st" = "503" ] && [ "$code" = "host_not_ready" ]; then pass "2b: app-home refused 503 host_not_ready"; else fail "2b: app-home launch got HTTP $st code=$code (want 503 host_not_ready)"; fi
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 90; then pass "2b: app-nohome runs"; else fail "2b: app-nohome did not reach running — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "2b: app-nohome launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
  since=$(now_epoch)
  rm -f "$fill"
  # The harness tmpfs is far below the free-space floor, so the recovered status
  # is `warn`, not `pass`. An ABSENT check is not a recovery: name the status.
  if wait_check_status "$REAL_HOST_ID" homes_free_space warn 90 "$since" || wait_check_status "$REAL_HOST_ID" homes_free_space pass 15 "$since"; then
    pass "2b: homes_free_space no longer fails after removing the fill file (status '$(check_field "$REAL_HOST_ID" homes_free_space status)')"
  else
    fail "2b: homes_free_space is '$(check_field "$REAL_HOST_ID" homes_free_space status)' after removing the fill file (want warn or pass)"
  fi
  res=$(launch_app "$APP_HOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid2; sid2=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid2")
    if wait_session_state "$sid2" running 90; then pass "2b: app-home launch reaches running after recovery"; else fail "2b: app-home launch did not reach running after recovery — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid2"
  else
    fail "2b: app-home launch after recovery got HTTP $st (want 201)$(launch_diag "$res")"
  fi
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 3 — input unavailable (/dev/uinput removed)
# ══════════════════════════════════════════════════════════════════════════
scenario_3() {
  only_run 3 || return 0
  local ov="$WORKDIR/override.no-uinput.yml" since; since=$(now_epoch)
  cat >"$ov" <<'YAML'
services:
  quasar-node-agent:
    devices: !override
      - /dev/dri
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || { unperformed "3: could not recreate agent without /dev/uinput"; return; }
  if wait_check_status "$REAL_HOST_ID" input_probe fail 90 "$since"; then
    collect_readiness_checks "$REAL_HOST_ID"
    local scope; scope=$(host_json "$REAL_HOST_ID" | jq -r '.host.readiness[] | select(.id=="input_probe") | .blocks.scope // ""')
    if [ "$scope" = "host" ]; then pass "3: input_probe fails with scope host"; else fail "3: input_probe scope='$scope' (want host)"; fi
  else
    fail "3: input_probe never reported fail within 90s"
  fi
  local res st code
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"; code=$(printf '%s' "${res#*$'\t'}" | json_get error.code)
  if [ "$st" = "503" ] && [ "$code" = "host_not_ready" ]; then pass "3: launch refused 503 host_not_ready"; else fail "3: launch expected 503 host_not_ready, got $st code=$code"; fi
  since=$(now_epoch)
  recreate_agent_with_override >/dev/null 2>&1 || true
  if wait_check_status "$REAL_HOST_ID" input_probe pass 90 "$since"; then pass "3: input_probe recovers to pass"; else fail "3: input_probe did not recover to pass"; fi
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 90; then pass "3: launch runs after recovery"; else fail "3: launch did not reach running after recovery — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "3: recovery launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 4 — GPU path broken (+ 4c placed-elsewhere, run INSIDE the fault
# window of 4a/4b, before it clears).
# ══════════════════════════════════════════════════════════════════════════
gpu_probe_check_ids() { echo "media_probe_gpu${GPU_INDEX}"; echo "application_gpu_probe_gpu${GPU_INDEX}"; }

assert_4_common() { # $1 label $2 since_epoch
  local label="$1" since="$2" hb
  hb=$(host_json "$REAL_HOST_ID")
  collect_readiness_checks "$REAL_HOST_ID"
  local id
  for id in $(gpu_probe_check_ids); do
    local status scope gidx
    status=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .status // ""')
    if [ -z "$status" ]; then
      fail "$label: $id is not reported at all under the fault"
      continue
    fi
    scope=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .blocks.scope // ""')
    gidx=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | (.blocks.gpu_index // "" | tostring)')
    if [ "$status" = "fail" ]; then
      if [ "$scope" = "gpu" ] && [ "$gidx" = "$GPU_INDEX" ]; then
        pass "$label: $id fail carries blocks.scope=gpu gpu_index=$GPU_INDEX"
      else
        fail "$label: $id fail carries scope=$scope gpu_index=$gidx (want gpu/$GPU_INDEX)"
      fi
    fi
  done
  local media_status; media_status=$(printf '%s' "$hb" | jq -r --arg id "media_probe_gpu${GPU_INDEX}" '.host.readiness[]? | select(.id==$id) | .status // ""')
  if [ "$media_status" = "fail" ]; then pass "$label: media_probe_gpu${GPU_INDEX} is fail"; else fail "$label: media_probe_gpu${GPU_INDEX} status='$media_status' (want fail)"; fi
  local res st code
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"; code=$(printf '%s' "${res#*$'\t'}" | json_get error.code)
  if [ "$st" = "503" ] && [ "$code" = "host_not_ready" ]; then pass "$label: launch refused host_not_ready"; else fail "$label: launch expected 503 host_not_ready, got $st code=$code"; fi
}

assert_4_recovery() { # $1 label $2 since_epoch
  local label="$1" since="$2"
  if wait_check_status "$REAL_HOST_ID" "media_probe_gpu${GPU_INDEX}" pass 90 "$since"; then pass "$label: probes recover to pass"; else fail "$label: probes did not recover to pass"; fi
  local res st
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 150; then pass "$label: launch runs after recovery"; else fail "$label: launch did not reach running — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "$label: recovery launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
}

# scenario_4c_inside_fault <label> — run 4c inside the caller's fault
# window: start a ready scripted host with a free slot, wait for it online
# with an active gate, launch, assert 201 + session.host_id == it, tear it
# down. Called from 4a/4b before they clear their own fault.
scenario_4c_inside_fault() {
  local label="$1"
  only_run 4c || return 0
  if ! fixture_supports_host; then
    unperformed "4c: readiness-fixture 'host' subcommand unavailable"
    return
  fi
  local name="${RID}-c" readiness_file="$WORKDIR/4c-ready.json"
  echo '[]' >"$readiness_file"
  if ! docker run -d --label "$OWNER_LABEL" --name "${RID}-host-c" --network host -v "$WORKDIR:$WORKDIR:ro" \
    "$FIXTURE_IMAGE_TAG" host --control-plane "ws://127.0.0.1:${CONTROL_PORT}/agent/ws" \
    --node-name "$name" --enrollment-token "$ENROLLMENT_TOKEN" --slots 1 --readiness-file "$readiness_file" >/dev/null 2>&1; then
    unperformed "4c: could not start the free scripted host"
    return
  fi
  local hid
  if ! hid=$(wait_host_registered "$name" 60); then
    unperformed "4c ($label): scripted host never registered"
    docker rm -f "${RID}-host-c" >/dev/null 2>&1 || true
    return
  fi
  if ! wait_host_online "$hid" 60 || ! wait_gate_state "$hid" active 60; then
    fail "4c ($label): scripted host never reached online+active-gate"
  else
    local res st sid
    res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
    if [ "$st" = "201" ]; then
      sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
      local placed_host; placed_host=$(http_body "$(http_raw GET "sessions/$sid" "$ADMIN_TOK")" | json_get session.host_id)
      if [ "$placed_host" = "$hid" ]; then
        pass "4c ($label): launch placed on the free scripted host (session.host_id matches)"
      else
        fail "4c ($label): launch placed on host_id=$placed_host, want the scripted host $hid"
      fi
      stop_and_wait "$sid"
    else
      fail "4c ($label): launch got HTTP $st (want 201, a free host was online)$(launch_diag "$res")"
    fi
  fi
  docker stop "${RID}-host-c" >/dev/null 2>&1 || true
  local del; del=$(http_raw DELETE "hosts/$hid" "$ADMIN_TOK")
  if [ "$(http_status "$del")" = "204" ]; then
    pass "4c ($label): scripted host row deleted 204"
  else
    fail "4c ($label): scripted host row delete got $(http_status "$del") (want 204)"
  fi
  docker rm -f "${RID}-host-c" >/dev/null 2>&1 || true
}

scenario_4a() {
  any_of 4a 4c || return 0
  if [ "$GPU_VENDOR" != "amd" ]; then
    if only_run 4a; then unperformed "4a: host GPU vendor is '$GPU_VENDOR', not amd"; fi
    return
  fi
  local ov="$WORKDIR/override.4a.yml" since; since=$(now_epoch)
  cat >"$ov" <<'YAML'
services:
  quasar-node-agent:
    tmpfs:
      - /usr/share/vulkan/icd.d
    environment:
      LIBVA_DRIVERS_PATH: /nonexistent
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || {
    if only_run 4a; then unperformed "4a: could not recreate agent with broken GPU path"; fi
    if only_run 4c; then unperformed "4c: could not inject the 4a fault to host it under"; fi
    return
  }
  wait_check_status "$REAL_HOST_ID" "media_probe_gpu${GPU_INDEX}" fail 90 "$since" || true
  if only_run 4a; then assert_4_common "4a" "$since"; fi
  if only_run 4c; then scenario_4c_inside_fault "4a"; fi
  since=$(now_epoch)
  recreate_agent_with_override >/dev/null 2>&1 || true
  if only_run 4a; then assert_4_recovery "4a" "$since"; fi
}

scenario_4a_prime() {
  only_run "4a'" || return 0
  if [ "$GPU_VENDOR" != "amd" ]; then
    unperformed "4a': host GPU vendor is '$GPU_VENDOR', not amd"
    return
  fi
  local ov="$WORKDIR/override.4aprime.yml"
  cat >"$ov" <<'YAML'
services:
  quasar-node-agent:
    tmpfs:
      - /usr/share/vulkan/icd.d
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || { unperformed "4a': could not recreate agent with ICD dir alone tmpfs'd"; return; }
  if wait_check_status "$REAL_HOST_ID" "media_probe_gpu${GPU_INDEX}" pass 60 0; then
    pass "4a': media_probe_gpu${GPU_INDEX} still pass with only Vulkan ICD gone (VA fallback)"
  else
    fail "4a': media_probe_gpu${GPU_INDEX} not pass with only Vulkan ICD gone"
  fi
  local blocking; blocking=$(blocking_len "$REAL_HOST_ID")
  if [ "$blocking" = "0" ]; then pass "4a': nothing in blocking"; else fail "4a': blocking has $blocking entries (want 0)"; fi
  local res st
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 90; then pass "4a': launch runs"; else fail "4a': launch did not reach running — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "4a': launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
  recreate_agent_with_override >/dev/null 2>&1 || true
}

scenario_4b() {
  any_of 4b 4c || return 0
  if [ "$GPU_VENDOR" != "nvidia" ]; then
    if only_run 4b; then unperformed "4b: host GPU vendor is '$GPU_VENDOR', not nvidia"; fi
    if only_run 4c && [ "$GPU_VENDOR" != "amd" ]; then
      unperformed "4c: host GPU vendor is '$GPU_VENDOR' (neither amd nor nvidia) — no GPU-path fault scenario to host it under"
    fi
    return
  fi
  # Override the driver volume to a fresh, harness-labelled empty volume
  # by NAME (the nvidia overlay's own volume is `quasar-nvidia-driver`,
  # mounted at /opt/quasar/nvidia-driver — see deploy/docker-compose.nvidia.yml).
  local fresh_vol="${RID}-empty-driver" since; since=$(now_epoch)
  docker volume create --label "$OWNER_LABEL" "$fresh_vol" >/dev/null 2>&1 || true
  local ov="$WORKDIR/override.4b.yml"
  cat >"$ov" <<YAML
services:
  quasar-node-agent:
    environment:
      QUASAR_NVIDIA_DRIVER_VOLUME: "0"
      QUASAR_CUDA_RUNTIME: "0"
    volumes:
      - type: volume
        source: ${fresh_vol}
        target: /opt/quasar/nvidia-driver
volumes:
  ${fresh_vol}:
    external: true
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || {
    if only_run 4b; then unperformed "4b: could not recreate agent on the fresh empty driver volume"; fi
    if only_run 4c; then unperformed "4c: could not inject the 4b fault to host it under"; fi
    docker volume rm -f "$fresh_vol" >/dev/null 2>&1 || true
    return
  }
  wait_check_status "$REAL_HOST_ID" "media_probe_gpu${GPU_INDEX}" fail 90 "$since" || true
  if only_run 4b; then assert_4_common "4b" "$since"; fi
  if only_run 4c; then scenario_4c_inside_fault "4b"; fi
  since=$(now_epoch)
  recreate_agent_with_override >/dev/null 2>&1 || true
  if only_run 4b; then assert_4_recovery "4b" "$since"; fi
  docker volume rm -f "$fresh_vol" >/dev/null 2>&1 || true
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 5 — proxy / indeterminate checks never block
# ══════════════════════════════════════════════════════════════════════════
scenario_5a() {
  only_run 5a || return 0
  # The host's own /dev/dri nodes are a SHARED device and are never chmod'ed.
  # The fault is a harness-owned device node (same major:minor, mode 0600
  # root:root) under $RID_ROOT, bound over the render node path inside the
  # fixture agent container only. App and probe containers get their devices
  # from the engine's host, so the real path keeps working: that is the point.
  local node shadow major minor
  node=$(find /dev/dri -maxdepth 1 -name 'renderD*' 2>/dev/null | sort | head -1)
  if [ -z "$node" ]; then
    unperformed "5a: no render node on this host to shadow"
    return
  fi
  major=$((16#$(stat -c '%t' "$node"))); minor=$((16#$(stat -c '%T' "$node")))
  shadow="$RID_ROOT/dev/$(basename "$node")"
  sudo mkdir -p "$RID_ROOT/dev"
  if ! sudo mknod -m 0600 "$shadow" c "$major" "$minor" 2>/dev/null; then
    unperformed "5a: could not create a harness-owned device node (mknod refused on this host)"
    return
  fi
  local ov="$WORKDIR/override.5a.yml" since; since=$(now_epoch)
  cat >"$ov" <<YAML
services:
  quasar-node-agent:
    volumes:
      - "$shadow:$node"
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || { unperformed "5a: could not recreate the agent with the shadow render node"; return; }
  if wait_check_status "$REAL_HOST_ID" dri_node_app_access fail 60 "$since"; then
    collect_readiness_checks "$REAL_HOST_ID"
    local has_blocks blocking
    has_blocks=$(host_json "$REAL_HOST_ID" | jq -r '.host.readiness[] | select(.id=="dri_node_app_access") | (.blocks == null)')
    if [ "$has_blocks" = "true" ]; then pass "5a: dri_node_app_access fail carries no blocks"; else fail "5a: dri_node_app_access carries blocks"; fi
    blocking=$(blocking_count_of "$REAL_HOST_ID" dri_node_app_access)
    if [ "$blocking" = "0" ]; then pass "5a: dri_node_app_access absent from blocking"; else fail "5a: dri_node_app_access in readiness_gate.blocking: $blocking (want 0)"; fi
  else
    unperformed "5a: dri_node_app_access never reported fail (the shadow node was not seen as unopenable by the app identity)"
  fi
  local res st
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    if wait_session_state "$sid" running 90; then pass "5a: launch still reaches running"; else fail "5a: launch did not reach running — $LAST_SESSION_DIAG"; fi
    stop_and_wait "$sid"
  else
    fail "5a: launch got HTTP $st (want 201, dri_node_app_access must never block)$(launch_diag "$res")"
  fi
  recreate_agent_with_override >/dev/null 2>&1 || true
}

scenario_5b() {
  only_run 5b || return 0
  # `unknown` must be CAUSED by the fault: a host whose audio probe is not
  # passing beforehand would make every assertion below hollow.
  if ! wait_check_status "$REAL_HOST_ID" audio_probe pass 150; then
    fail "5b: audio_probe is not pass before the fault (status='$(check_field "$REAL_HOST_ID" audio_probe status)'), so an unknown would prove nothing"
    return
  fi
  local ov="$WORKDIR/override.5b.yml" since; since=$(now_epoch)
  cat >"$ov" <<YAML
services:
  quasar-node-agent:
    environment:
      QUASAR_PULSE_IMAGE: "${RID}/does-not-exist:none"
YAML
  recreate_agent_with_override "$ov" >/dev/null 2>&1 || { unperformed "5b: could not recreate agent with a missing pulse image"; return; }
  if wait_check_status "$REAL_HOST_ID" audio_probe unknown 60 "$since"; then
    pass "5b: audio_probe reports unknown"
  else
    # The image is certainly missing, so this is not a fault that could not be
    # injected: it is the wrong answer. Nothing below may be asserted on it.
    fail "5b: audio_probe is '$(check_field "$REAL_HOST_ID" audio_probe status)' with its sidecar image missing (want unknown)"
    recreate_agent_with_override >/dev/null 2>&1 || true
    return
  fi
  local blocking; blocking=$(blocking_count_of "$REAL_HOST_ID" audio_probe)
  if [ "$blocking" = "0" ]; then pass "5b: audio_probe absent from blocking"; else fail "5b: audio_probe in readiness_gate.blocking: $blocking (want 0)"; fi
  local res st
  res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
  if [ "$st" = "201" ]; then
    local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
    CREATED_SESSION_IDS+=("$sid")
    pass "5b: launch is placed (201)"
    stop_and_wait "$sid"
  else
    fail "5b: launch got HTTP $st (want 201)$(launch_diag "$res")"
  fi
  since=$(now_epoch)
  recreate_agent_with_override >/dev/null 2>&1 || true
  if wait_check_status "$REAL_HOST_ID" audio_probe pass 150 "$since"; then pass "5b: audio_probe recovers to pass"; else fail "5b: audio_probe did not recover to pass within 150s (status='$(check_field "$REAL_HOST_ID" audio_probe status)' summary='$(check_field "$REAL_HOST_ID" audio_probe summary)')"; fi
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 6 — capacity vs readiness precedence, via two scripted hosts.
# Both host ids are tracked; each wait requires online AND gate active
# (deliberate — this is what fails on a pre-RH-02 control plane, per the
# matrix's "Baseline proof"); B's own check keeps the harness_synthetic_
# prefix (scripts/verify's guard keys on it) even though it is delivered via
# the scripted host's --readiness-file, not the relay.
# ══════════════════════════════════════════════════════════════════════════
scenario_6() {
  any_of 6a 6b 6c 6d || return 0
  if ! fixture_supports_host; then
    for id in 6a 6b 6c 6d; do unperformed "$id: readiness-fixture 'host' subcommand unavailable"; done
    return
  fi
  local name_a="${RID}-a" name_b="${RID}-b"
  local ready_file="$WORKDIR/6-ready.json" blocked_file="$WORKDIR/6-blocked.json"
  echo '[]' >"$ready_file"
  echo '[{"id":"harness_synthetic_blocked","status":"fail","summary":"scripted host B blocked","remediation":"n/a","source":"host_probe","blocks":{"scope":"host","enforced_by":"control_plane"}}]' >"$blocked_file"

  local hid_a="" hid_b=""
  start_host_a() {
    docker run -d --label "$OWNER_LABEL" --name "${RID}-host-a" --network host -v "$WORKDIR:$WORKDIR:ro" \
      "$FIXTURE_IMAGE_TAG" host --control-plane "ws://127.0.0.1:${CONTROL_PORT}/agent/ws" \
      --node-name "$name_a" --enrollment-token "$ENROLLMENT_TOKEN" --slots 1 --readiness-file "$ready_file" >/dev/null 2>&1
  }
  start_host_b() {
    docker run -d --label "$OWNER_LABEL" --name "${RID}-host-b" --network host -v "$WORKDIR:$WORKDIR:ro" \
      "$FIXTURE_IMAGE_TAG" host --control-plane "ws://127.0.0.1:${CONTROL_PORT}/agent/ws" \
      --node-name "$name_b" --enrollment-token "$ENROLLMENT_TOKEN" --slots 1 --readiness-file "$blocked_file" >/dev/null 2>&1
  }

  compose_cmd stop quasar-node-agent >/dev/null 2>&1 || true

  if only_run 6a; then
    if start_host_a && hid_a=$(wait_host_registered "$name_a" 60) \
      && wait_host_online "$hid_a" 60 && wait_gate_state "$hid_a" active 60; then
      start_host_b || true
      if hid_b=$(wait_host_registered "$name_b" 60) && wait_host_online "$hid_b" 60 \
        && wait_gate_state "$hid_b" active 60 && wait_blocking_has "$hid_b" harness_synthetic_blocked 60; then
        collect_readiness_checks "$hid_b"
        local res1 st1 sid1
        res1=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st1="${res1%%$'\t'*}"
        if [ "$st1" = "201" ]; then
          sid1=$(printf '%s' "${res1#*$'\t'}" | json_get session.id)
          CREATED_SESSION_IDS+=("$sid1")
          if [ "$(printf '%s' "${res1#*$'\t'}" | json_get session.host_id)" = "$hid_a" ]; then pass "6a: the first launch fills the ready host"; else fail "6a: the first launch was not placed on the ready host"; fi
          local res2 st2 code2
          res2=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st2="${res2%%$'\t'*}"; code2=$(printf '%s' "${res2#*$'\t'}" | json_get error.code)
          if [ "$st2" = "503" ] && [ "$code2" = "capacity_exhausted" ]; then
            pass "6a: second launch refused 503 capacity_exhausted with the blocked host also online"
          else
            fail "6a: second launch expected 503 capacity_exhausted, got $st2 code=$code2"
          fi
          stop_and_wait "$sid1"
        else
          fail "6a: first launch (against the ready scripted host) got HTTP $st1 (want 201)$(launch_diag "$res1")"
        fi
      else
        fail "6a: scripted host B is '$(host_status "${hid_b:-none}")' with gate '$(gate_state "${hid_b:-none}")' and never listed its failing check in blocking"
      fi
    else
      if [ -z "$hid_a" ]; then
        unperformed "6a: scripted host A never registered"
      else
        fail "6a: scripted host A is '$(host_status "$hid_a")' but its readiness gate never became active (state '$(gate_state "$hid_a")')"
      fi
    fi
  fi

  if only_run 6b; then
    [ -n "$hid_a" ] || hid_a=$(host_id_by_node_name "$name_a")
    [ -n "$hid_b" ] || hid_b=$(host_id_by_node_name "$name_b")
    docker stop "${RID}-host-a" "${RID}-host-b" >/dev/null 2>&1 || true
    if poll_until 60 3 bash -c "! (curl -sS -H 'Authorization: Bearer $ADMIN_TOK' '$API/v1/hosts' | jq -e '.items[]? | select(.status==\"online\")' >/dev/null 2>&1)"; then
      local res st code
      res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"; code=$(printf '%s' "${res#*$'\t'}" | json_get error.code)
      if [ "$st" = "503" ] && [ "$code" = "no_host_available" ]; then pass "6b: launch refused 503 no_host_available with nothing online"; else fail "6b: expected 503 no_host_available, got $st code=$code"; fi
    else
      unperformed "6b: could not confirm every host offline within bound"
    fi
  fi

  if any_of 6c 6d; then
    [ -n "$hid_b" ] || hid_b=$(host_id_by_node_name "$name_b")
    if ! docker start "${RID}-host-b" >/dev/null 2>&1; then start_host_b || true; fi
    if hid_b=$(wait_host_registered "$name_b" 60) && wait_host_online "$hid_b" 60 && wait_gate_state "$hid_b" active 60 && wait_blocking_has "$hid_b" harness_synthetic_blocked 60; then
      collect_readiness_checks "$hid_b"
      if only_run 6c; then
        local raw st body
        raw=$(http_raw POST sessions "$ADMIN_TOK" "{\"app_id\":\"$APP_NOHOME_ID\"}")
        st=$(http_status "$raw"); body=$(http_body "$raw")
        if [ "$st" = "503" ] && [ "$(printf '%s' "$body" | json_get error.code)" = "host_not_ready" ]; then pass "6c: launch refused host_not_ready with only the blocked host online"; else fail "6c: expected 503 host_not_ready, got $st"; fi
        if http_has_header "$raw" "Retry-After"; then fail "6c: response carried a Retry-After header"; else pass "6c: no Retry-After header"; fi
        # The real agent is stopped, but its stored report still serves every
        # id a real host uses: that is the list the message is judged against.
        collect_readiness_checks "$REAL_HOST_ID"
        assert_names_no_check "6c" "$(printf '%s' "$body" | json_get error.message)"
      fi
      # 6d MUST run inside this same state (only the blocked host online)
      # — running it after the real agent is back online would let the
      # non-admin launch succeed and leak a session, proving nothing about
      # the no-detail rule.
      if only_run 6d; then
        if [ -n "$NONADMIN_TOK" ]; then
          local raw st body
          raw=$(http_raw POST sessions "$NONADMIN_TOK" "{\"app_id\":\"$APP_NOHOME_ID\"}")
          st=$(http_status "$raw"); body=$(http_body "$raw")
          if [ "$st" = "503" ] && [ "$(printf '%s' "$body" | json_get error.code)" = "host_not_ready" ]; then
            collect_readiness_checks "$REAL_HOST_ID"
            assert_names_no_check "6d" "$body"
          else
            fail "6d: the non-admin launch got $st code=$(printf '%s' "$body" | json_get error.code) (want 503 host_not_ready)"
          fi
          local h1 h2
          h1=$(http_raw GET hosts "$NONADMIN_TOK")
          h2=$(http_raw GET "hosts/$hid_b" "$NONADMIN_TOK")
          if [ "$(http_status "$h1")" = "403" ]; then pass "6d: GET /v1/hosts is 403 for non-admin"; else fail "6d: GET /v1/hosts got $(http_status "$h1") (want 403)"; fi
          if [ "$(http_status "$h2")" = "403" ]; then pass "6d: GET /v1/hosts/{id} is 403 for non-admin"; else fail "6d: GET /v1/hosts/{id} got $(http_status "$h2") (want 403)"; fi
        else
          unperformed "6d: no non-admin token available (registration provisioning failed earlier)"
        fi
      fi
    else
      if [ -z "$hid_b" ]; then
        unperformed "6c/6d: scripted host B never registered"
      else
        fail "6c/6d: scripted host B is '$(host_status "$hid_b")' with gate '$(gate_state "$hid_b")' and never listed its failing check in blocking"
      fi
    fi
  fi

  docker stop "${RID}-host-a" "${RID}-host-b" >/dev/null 2>&1 || true
  [ -n "$hid_a" ] && { local d1; d1=$(http_raw DELETE "hosts/$hid_a" "$ADMIN_TOK"); [ "$(http_status "$d1")" = "204" ] || fail "6: scripted host A row delete got $(http_status "$d1") (want 204)"; }
  [ -n "$hid_b" ] && { local d2; d2=$(http_raw DELETE "hosts/$hid_b" "$ADMIN_TOK"); [ "$(http_status "$d2")" = "204" ] || fail "6: scripted host B row delete got $(http_status "$d2") (want 204)"; }
  docker rm -f "${RID}-host-a" "${RID}-host-b" >/dev/null 2>&1 || true
  start_real_agent
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 7 — control-plane readiness override, via the whole-run relay.
# Relay is already running and the real agent already points at it from
# first boot — no per-scenario relay start or agent recreate here.
# Waits are state-based on the host body, never on a cumulative counter.
# ══════════════════════════════════════════════════════════════════════════
scenario_7() {
  any_of 7a 7b 7c 7d 7e 7f 7g 7h || return 0
  local check_id="harness_synthetic_gate" override_was_set=0

  if ! poll_until 60 3 bash -c "curl -sS '$RELAY_CONTROL/stats' | jq -e '.agent_connections>0' >/dev/null 2>&1"; then
    for id in 7a 7b 7c 7d 7e 7f 7g 7h; do unperformed "$id: the real agent never connected through the relay (diagnostic: $(relay_stats))"; done
    return
  fi

  if only_run 7a; then
    relay_rule "{\"mode\":\"inject\",\"check\":{\"id\":\"$check_id\",\"status\":\"fail\",\"summary\":\"harness synthetic gate\",\"remediation\":\"clear via relay rule\",\"source\":\"host_probe\",\"blocks\":{\"scope\":\"host\",\"enforced_by\":\"control_plane\"}}}"
    if wait_check_status "$REAL_HOST_ID" "$check_id" fail 90 0; then
      collect_readiness_checks "$REAL_HOST_ID"
      local res st; res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
      if [ "$st" = "503" ] && [ "$(printf '%s' "${res#*$'\t'}" | json_get error.code)" = "host_not_ready" ]; then
        pass "7a: launch refused host_not_ready before the override"
      else
        fail "7a: expected 503 host_not_ready before override, got $st"
      fi
      local put1 put2 audit_before audit_after
      put1=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
      if [ "$(http_status "$put1")" = "200" ]; then pass "7a: override PUT -> 200"; else fail "7a: override PUT got $(http_status "$put1") (want 200)"; fi
      audit_before=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.set" "$ADMIN_TOK")" | jq -r '.items | length' 2>/dev/null || echo "")
      put2=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
      audit_after=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.set" "$ADMIN_TOK")" | jq -r '.items | length' 2>/dev/null || echo "")
      if [ "$(http_status "$put2")" = "200" ] && [ "$audit_before" = "$audit_after" ]; then
        pass "7a: repeat PUT -> 200, no second audit row"
      else
        fail "7a: repeat PUT status $(http_status "$put2"), audit $audit_before -> $audit_after"
      fi
      # Select the row by check_id in details, not by index 0 — the feed
      # is newest-first and this run may not be the only actor.
      local sev
      sev=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.set" "$ADMIN_TOK")" \
        | jq -r --arg cid "$check_id" '[.items[] | select((.details.check_id // "")==$cid)][0].severity // ""')
      if [ "$sev" = "warn" ]; then pass "7a: audit host.readiness_override.set severity warn"; else fail "7a: audit severity '$sev' (want warn)"; fi
      local audit_node
      audit_node=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.set" "$ADMIN_TOK")" \
        | jq -r --arg cid "$check_id" '[.items[] | select((.details.check_id // "")==$cid)][0].details.node_name // ""')
      if [ "$audit_node" = "${RID}-real" ]; then pass "7a: the audit detail carries check_id and node_name"; else fail "7a: audit detail node_name='$audit_node' (want ${RID}-real)"; fi
      if [ "$(http_status "$put1")" = "200" ]; then override_was_set=1; fi
    else
      unperformed "7a: relay rule never reached the control plane's stored readiness (diagnostic: $(relay_stats))"
    fi
  fi

  if only_run 7b; then
    local hb blocking overridden readiness_fail
    hb=$(host_json "$REAL_HOST_ID")
    blocking=$(printf '%s' "$hb" | jq -r --arg id "$check_id" '[.host.readiness_gate.blocking[]? | select(.check_id==$id)] | length')
    overridden=$(printf '%s' "$hb" | jq -r --arg id "$check_id" '.host.readiness_gate.blocking[]? | select(.check_id==$id) | .overridden')
    readiness_fail=$(printf '%s' "$hb" | jq -r --arg id "$check_id" '.host.readiness[] | select(.id==$id) | .status')
    if [ "$blocking" != "0" ] && [ "$overridden" = "true" ] && [ "$readiness_fail" = "fail" ]; then
      pass "7b: still visible — blocking lists it, overridden true, readiness[] still fail"
    else
      fail "7b: blocking=$blocking overridden=$overridden readiness=$readiness_fail"
    fi
  fi

  if only_run 7c; then
    local res st
    res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
    if [ "$st" = "201" ]; then
      local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
      CREATED_SESSION_IDS+=("$sid")
      if wait_session_state "$sid" running 90; then pass "7c: launch succeeds and reaches running with the override"; else fail "7c: launch did not reach running — $LAST_SESSION_DIAG"; fi
      stop_and_wait "$sid"
    else
      fail "7c: launch got HTTP $st (want 201, override in effect)$(launch_diag "$res")"
    fi
  fi

  if only_run 7d && [ "$override_was_set" != "1" ]; then
    # "Gone" and "actor null" are both true of an override that never existed.
    fail "7d: no override was set by 7a, so a lapse cannot be shown"
  elif only_run 7d; then
    relay_rule "{\"mode\":\"inject\",\"check\":{\"id\":\"$check_id\",\"status\":\"pass\",\"summary\":\"cleared\",\"source\":\"host_probe\"}}"
    if wait_check_status "$REAL_HOST_ID" "$check_id" pass 90 0; then
      pass "7d: check reads pass in readiness[]"
      if poll_until 60 3 bash -c "curl -sS -H 'Authorization: Bearer $ADMIN_TOK' '$API/v1/hosts/$REAL_HOST_ID' | jq -e '[.host.readiness_overrides[]? | select(.check_id==\"$check_id\")] | length == 0' >/dev/null 2>&1"; then
        pass "7d: override lapsed (gone from readiness_overrides)"
        local lapsed_rows actor
        lapsed_rows=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.lapsed" "$ADMIN_TOK")" \
          | jq -c --arg cid "$check_id" '[.items[] | select((.details.check_id // "")==$cid)]')
        actor=$(printf '%s' "$lapsed_rows" | jq -r '.[0].actor_user_id // "null"')
        if [ "$(printf '%s' "$lapsed_rows" | jq -r 'length')" = "0" ]; then
          fail "7d: no host.readiness_override.lapsed audit row for $check_id"
        elif [ "$actor" = "null" ]; then
          pass "7d: audit .lapsed exists with actor null"
        else
          fail "7d: audit .lapsed actor='$actor' (want null)"
        fi
      else
        fail "7d: override did not lapse after the check reported pass"
      fi
    else
      fail "7d: check never reported pass after the relay cleared it"
    fi
  fi

  if only_run 7e; then
    relay_rule "{\"mode\":\"inject\",\"check\":{\"id\":\"$check_id\",\"status\":\"fail\",\"summary\":\"harness synthetic gate\",\"remediation\":\"clear via relay rule\",\"source\":\"host_probe\",\"blocks\":{\"scope\":\"host\",\"enforced_by\":\"control_plane\"}}}"
    wait_check_status "$REAL_HOST_ID" "$check_id" fail 90 0 || true
    local put_e; put_e=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
    if [ "$(http_status "$put_e")" = "200" ]; then pass "7e: override set again -> 200"; else fail "7e: override PUT got $(http_status "$put_e") (want 200)"; fi
    local d1 d2
    d1=$(http_raw DELETE "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
    if [ "$(http_status "$d1")" = "204" ]; then pass "7e: DELETE -> 204"; else fail "7e: DELETE got $(http_status "$d1") (want 204)"; fi
    d2=$(http_raw DELETE "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
    if [ "$(http_status "$d2")" = "204" ]; then pass "7e: repeat DELETE -> 204 (idempotent)"; else fail "7e: repeat DELETE got $(http_status "$d2") (want 204)"; fi
    local cleared
    cleared=$(http_body "$(http_raw GET "admin/activity?action=host.readiness_override.cleared" "$ADMIN_TOK")" \
      | jq -r --arg cid "$check_id" '[.items[] | select((.details.check_id // "")==$cid)] | length')
    if [ "${cleared:-0}" = "1" ]; then
      pass "7e: exactly one .cleared audit row for this check (the idempotent repeat wrote none)"
    else
      fail "7e: ${cleared:-0} .cleared audit row(s) for $check_id (want exactly 1)"
    fi
    local res st
    res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
    if [ "$st" = "503" ] && [ "$(printf '%s' "${res#*$'\t'}" | json_get error.code)" = "host_not_ready" ]; then
      pass "7e: launch refused host_not_ready again after clearing"
    else
      fail "7e: launch after clearing got $st (want 503 host_not_ready)$(launch_diag "$res")"
    fi
  fi

  if only_run 7f; then
    local put_f; put_f=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK")
    if [ "$(http_status "$put_f")" = "200" ]; then pass "7f: override set before the rename -> 200"; else fail "7f: override PUT got $(http_status "$put_f") (want 200)"; fi
    relay_rule "{\"mode\":\"inject\",\"check\":{\"id\":\"${check_id}_v2\",\"status\":\"fail\",\"summary\":\"renamed\",\"remediation\":\"n/a\",\"source\":\"host_probe\",\"blocks\":{\"scope\":\"host\",\"enforced_by\":\"control_plane\"}}}"
    # Two separate facts: the renamed check ARRIVED (else the fault was not
    # injected: unperformed), and the gate LISTS it (else the product is wrong).
    if ! wait_check_status "$REAL_HOST_ID" "${check_id}_v2" fail 90 0; then
      unperformed "7f: the renamed check never reached the control plane's stored readiness (diagnostic: $(relay_stats))"
    elif ! wait_blocking_has "$REAL_HOST_ID" "${check_id}_v2" 30; then
      fail "7f: the renamed check is reported as fail with blocks but readiness_gate.blocking does not list it"
    else
      local hb inert overridden_new
      hb=$(host_json "$REAL_HOST_ID")
      inert=$(printf '%s' "$hb" | jq -r --arg id "$check_id" '.host.readiness_overrides[]? | select(.check_id==$id) | .inert')
      overridden_new=$(printf '%s' "$hb" | jq -r --arg id "${check_id}_v2" '.host.readiness_gate.blocking[]? | select(.check_id==$id) | .overridden')
      if [ "$inert" = "true" ]; then pass "7f: old override listed inert"; else fail "7f: old override inert='$inert' (want true)"; fi
      if [ "$overridden_new" = "false" ]; then pass "7f: new id in blocking, overridden false"; else fail "7f: new id overridden='$overridden_new' (want false)"; fi
      local res st
      res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
      if [ "$st" = "503" ] && [ "$(printf '%s' "${res#*$'\t'}" | json_get error.code)" = "host_not_ready" ]; then
        pass "7f: launch refused host_not_ready for the renamed check"
      else
        fail "7f: expected 503 host_not_ready, got $st"
      fi
    fi
    relay_rule '{"mode":"off"}'
    http_raw DELETE "admin/hosts/$REAL_HOST_ID/readiness-overrides/$check_id" "$ADMIN_TOK" >/dev/null
    http_raw DELETE "admin/hosts/$REAL_HOST_ID/readiness-overrides/${check_id}_v2" "$ADMIN_TOK" >/dev/null
    if wait_blocking_empty "$REAL_HOST_ID" 90; then
      pass "7f: blocking empty after rule off + override delete"
      local res st
      res=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st="${res%%$'\t'*}"
      if [ "$st" = "201" ]; then
        local sid; sid=$(printf '%s' "${res#*$'\t'}" | json_get session.id)
        CREATED_SESSION_IDS+=("$sid")
        if wait_session_state "$sid" running 90; then pass "7f: launch runs after full clear"; else fail "7f: launch did not reach running — $LAST_SESSION_DIAG"; fi
        stop_and_wait "$sid"
      else
        fail "7f: launch got HTTP $st (want 201)$(launch_diag "$res")"
      fi
    else
      fail "7f: blocking not empty after clearing"
    fi
  fi

  if only_run 7g; then
    if [ -n "$NONADMIN_TOK" ]; then
      local raw
      raw=$(http_raw PUT "admin/hosts/00000000-0000-0000-0000-000000000000/readiness-overrides/$check_id" "$NONADMIN_TOK")
      if [ "$(http_status "$raw")" = "403" ]; then pass "7g: non-admin PUT on unknown host -> 403 (not 404)"; else fail "7g: got $(http_status "$raw") (want 403)"; fi
    else
      unperformed "7g: no non-admin token available"
    fi
  fi

  if only_run 7h; then
    local bad_id="bad%20id" long_id raw1 raw2
    long_id=$(printf 'a%.0s' $(seq 1 129))
    raw1=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$bad_id" "$ADMIN_TOK")
    raw2=$(http_raw PUT "admin/hosts/$REAL_HOST_ID/readiness-overrides/$long_id" "$ADMIN_TOK")
    if [ "$(http_status "$raw1")" = "400" ] && [ "$(printf '%s' "$(http_body "$raw1")" | json_get error.code)" = "validation_failed" ]; then
      pass "7h: bad check_id -> 400 validation_failed"
    else
      fail "7h: bad check_id got $(http_status "$raw1") (want 400 validation_failed)"
    fi
    if [ "$(http_status "$raw2")" = "400" ] && [ "$(printf '%s' "$(http_body "$raw2")" | json_get error.code)" = "validation_failed" ]; then
      pass "7h: 129-byte check_id -> 400 validation_failed"
    else
      fail "7h: 129-byte check_id got $(http_status "$raw2") (want 400 validation_failed)"
    fi
  fi

  relay_rule '{"mode":"off"}'
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 8 — host-local honesty scan over every readiness check seen.
# collect_readiness_checks is called from every scenario at the moment
# its fault is visible (inline above), not only once at the end; this final
# pass just adds one more sweep of the current real/known hosts and
# de-duplicates before scanning.
# ══════════════════════════════════════════════════════════════════════════
scenario_8() {
  collect_readiness_checks "$REAL_HOST_ID"
  dedupe_check_ids_file
  only_run 8 || return 0
  local allow_list_re='cannot tell (whether|if) (a )?browser|no evidence (that|of) (a )?browser|never claims? (a )?browser|is not evidence (a )?browser'
  local hit=""
  hit=$(python3 -c "
import json, re, sys
allow = re.compile(r'$allow_list_re', re.I)
bad = re.compile(r'browser|internet|external(ly)?\\b|public(ly)?\\b|port[- ]forward|\\bwan\\b|remote client|reachable from|reachability', re.I)
leaks = []
with open('$ALL_CHECK_IDS_FILE', encoding='utf-8') as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            c = json.loads(line)
        except Exception:
            continue
        for field in ('id', 'summary', 'remediation'):
            v = c.get(field) or ''
            # The spec keeps this one id and rewords the check as the host's
            # inbound firewall posture (RH-02 spec, host readiness is not
            # browser reachability), so the id alone is exempt. Its summary
            # and remediation are scanned like every other check's.
            if field == 'id' and v == 'media_reachability':
                continue
            if bad.search(v) and not allow.search(v):
                leaks.append(f'{c.get(\"id\")}.{field}: {v!r}')
print('\n'.join(leaks))
")
  if [ "$(check_corpus_size)" -lt "$MIN_CHECK_CORPUS" ]; then
    unperformed "8: only $(check_corpus_size) check id(s) were collected in this run, too few to scan"
  elif [ -z "$hit" ]; then
    pass "8: no readiness check id/summary/remediation implies browser-reachability without an allow-listed negation"
  else
    fail "8: host-local honesty violation(s): $hit"
  fi
}

# ══════════════════════════════════════════════════════════════════════════
# Scenario 1 — nested engine (dind), following the #256 recipe: dockerd not
# PID 1, --live-restore, bridge network (never --network host for the
# ENGINE itself), the nested agent mirroring the real compose service's
# mounts/env as closely as a nested engine allows. Best-effort throughout:
# any setup step failing marks the remaining 1a-1e unperformed with the
# reason, per the matrix.
# ══════════════════════════════════════════════════════════════════════════
dind_exec() { docker exec "${RID}-dind" "$@"; } # $@ command to run against the NESTED engine's shell
dind_docker() { docker exec "${RID}-dind" docker "$@"; }
dind_load() { docker save "$1" | docker exec -i "${RID}-dind" docker load >/dev/null 2>&1; } # $1 image — exec needs -i or the piped archive never arrives

scenario_1() {
  any_of 1a 1b 1c 1d 1e || return 0
  local net="${RID}-dindnet"
  if ! docker network create --label "$OWNER_LABEL" -d bridge "$net" >/dev/null 2>&1; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: could not create the dind bridge network"; done
    return
  fi
  local gw
  gw=$(docker network inspect "$net" -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo "")
  if [ -z "$gw" ]; then
    for id in 1a 1b 1c 1d 1e; do unperformed "1: dind bridge network has no gateway address"; done
    docker network rm "$net" >/dev/null 2>&1 || true
    return
  fi
  if ! docker run -d --name "${RID}-dind" --label "$OWNER_LABEL" --privileged \
    --network "$net" \
    docker:dind sh -c 'dockerd --live-restore >/var/log/dockerd.log 2>&1 & sleep infinity' >/dev/null 2>&1; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: dind container failed to start"; done
    docker network rm "$net" >/dev/null 2>&1 || true
    return
  fi
  NESTED_ENGINE_STARTED=1
  if ! poll_until 60 2 dind_docker info; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: nested dockerd never became ready"; done
    return
  fi
  if ! dind_exec test -e /dev/dri || ! dind_exec test -e /dev/uinput; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: /dev/dri or /dev/uinput does not exist inside the privileged nested engine"; done
    return
  fi
  if ! dind_load "$AGENT_IMAGE"; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: could not load the agent image into the nested engine"; done
    return
  fi
  # The pulse sidecar defaults to the agent image (deploy/docker-compose.yml
  # QUASAR_PULSE_IMAGE), so that one load covers both; the fixture app needs
  # fedora:43 for the recovery launch.
  dind_load fedora:43 || true

  # Mirror the real compose service's mounts as closely as a nested engine
  # allows: the NESTED engine's own docker.sock (agent-owned session
  # containers), XDG_RUNTIME_DIR + the agent's data/secret volume as paths
  # inside the dind container's own filesystem (bind-mounted at the SAME
  # path the production service uses), NET_ADMIN+SYSLOG caps, init.
  dind_docker volume create quasar-agent-data >/dev/null 2>&1 || true
  dind_exec mkdir -p /run/quasar-agent /var/lib/quasar/homes /var/lib/quasar/templates
  # The engine socket reaches the agent through its DIRECTORY: a restarted
  # dockerd unlinks and recreates the socket, and a file bind mount would leave
  # the agent holding the dead inode for ever (seen live: no recovery).
  # PID 1 of the agent container is a respawn loop (under --init), as in the
  # #256 record: the agent enters diagnostic mode at PROCESS start, so the
  # fault is "engine stopped, then the agent process dies and comes back" with
  # the container itself never restarting.
  # The nested agent's environment IS the compose service's, rendered by compose
  # itself, so the two cannot drift (a hand-copied list left QUASAR_RENDER_NODE
  # unset instead of empty, which the agent reads as software rendering). The
  # explicit -e flags after --env-file win for the few values that must differ.
  compose_cmd config --format json 2>/dev/null \
    | jq -r '.services["quasar-node-agent"].environment // {} | to_entries[] | "\(.key)=\(.value // "")"' \
    | docker exec -i "${RID}-dind" sh -c 'cat >/tmp/nested-agent.env'
  if ! dind_exec test -s /tmp/nested-agent.env; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: could not render the agent service environment for the nested agent"; done
    return
  fi
  if ! dind_docker run -d --name "${RID}-nested-agent" --network host --init \
    --env-file /tmp/nested-agent.env \
    --cap-add=NET_ADMIN --cap-add=SYSLOG \
    --device /dev/dri --device /dev/uinput \
    -v /var/run:/run/nested-engine \
    -e "DOCKER_HOST=unix:///run/nested-engine/docker.sock" \
    -v /run/quasar-agent:/run/quasar-agent \
    -v quasar-agent-data:/var/lib/quasar-agent \
    -v /var/lib/quasar/homes:/var/lib/quasar/homes \
    -v /var/lib/quasar/templates:/var/lib/quasar/templates \
    -e "CONTROL_PLANE_URL=ws://cp-gateway:${CONTROL_PORT}" \
    -e "ENROLLMENT_TOKEN=$ENROLLMENT_TOKEN" \
    -e "NODE_NAME=${RID}-nested" \
    -e "NODE_SECRET_PATH=/var/lib/quasar-agent/node-secret" \
    -e "XDG_RUNTIME_DIR=/run/quasar-agent" \
    -e "QUASAR_HOME_ROOT=/var/lib/quasar/homes" \
    -e "QUASAR_TEMPLATE_ROOT=/var/lib/quasar/templates" \
    -e "QUASAR_ALLOW_PLAINTEXT_AGENT=1" \
    -e "QUASAR_HEALTH_ADDR=127.0.0.1:9191" \
    --add-host "cp-gateway:$gw" \
    "$AGENT_IMAGE" sh -c 'while :; do /usr/local/bin/quasar-node-agent; sleep 2; done' >/dev/null 2>&1; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: could not launch the nested agent"; done
    return
  fi
  local nested_host_id
  if ! nested_host_id=$(wait_host_registered "${RID}-nested" 90); then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: nested host never registered with the control plane"; done
    return
  fi

  # The agent shares the nested engine's network namespace (--network host
  # INSIDE dind), so its health endpoint is read from there: `docker exec`
  # into the agent cannot work while the nested dockerd is down.
  nested_health() { dind_exec sh -c "wget -q -S -O /dev/null http://127.0.0.1:9191/health 2>&1 | grep -o 'HTTP/[0-9.]* [0-9]*' | head -1" 2>/dev/null || true; }
  # Matched on the full command line, anchored at the end: the respawn loop's
  # own `sh -c` line contains the path too but does not END with it.
  nested_agent_pid() { dind_exec pgrep -f '/usr/local/bin/quasar-node-agent$' 2>/dev/null | head -1 || true; }

  local started_at1 restarts1
  started_at1=$(dind_docker inspect "${RID}-nested-agent" -f '{{.State.StartedAt}}' 2>/dev/null || echo "")
  restarts1=$(dind_docker inspect "${RID}-nested-agent" -f '{{.RestartCount}}' 2>/dev/null || echo "")

  # ── Before the fault: can this host serve a launch at all? ───────────────
  # host_not_ready means readiness is the SOLE reason, so 1c is only meaningful
  # on a nested host that was servable beforehand. Where the nested engine
  # cannot give the agent a working GPU (no vendor container runtime inside
  # it), the control plane rightly answers no_host_available and 1c is
  # unperformed — never passed, never failed on the host's limitation.
  local nested_servable=0 nested_unservable_why=""
  if only_run 1c; then
    compose_cmd stop quasar-node-agent >/dev/null 2>&1 || true
    if wait_blocking_empty "$nested_host_id" 180; then
      local res0 st0 sid0
      res0=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st0="${res0%%$'\t'*}"
      if [ "$st0" = "201" ]; then
        sid0=$(printf '%s' "${res0#*$'\t'}" | json_get session.id)
        CREATED_SESSION_IDS+=("$sid0")
        if wait_session_state "$sid0" running 120; then
          nested_servable=1
          pass "1c: before the fault, a launch on the nested host reaches running"
        else
          nested_unservable_why="a pre-fault launch did not reach running"
        fi
        stop_and_wait "$sid0"
      else
        nested_unservable_why="a pre-fault launch got HTTP $st0 code=$(printf '%s' "${res0#*$'\t'}" | json_get error.code)"
      fi
    else
      nested_unservable_why="its own probes keep it blocked: $(host_json "$nested_host_id" | jq -c '[.host.readiness_gate.blocking[]?.check_id]')"
    fi
  fi

  # ── Inject: stop the nested dockerd, then kill the agent PROCESS ─────────
  local fault_since fault_reported=0 pid_fault=""
  dind_exec pkill -TERM dockerd >/dev/null 2>&1 || true
  if ! poll_until 30 2 bash -c "! docker exec '${RID}-dind' docker info >/dev/null 2>&1"; then
    for id in 1a 1b 1c 1d 1e; do unperformed "$id: could not stop dockerd inside the nested engine"; done
  else
    fault_since=$(now_epoch)
    dind_exec pkill -KILL -f '/usr/local/bin/quasar-node-agent$' >/dev/null 2>&1 || true
    if wait_check_status "$nested_host_id" runtime_endpoint fail 120 "$fault_since" && wait_gate_state "$nested_host_id" active 30; then
      fault_reported=1
      pid_fault=$(nested_agent_pid)
      collect_readiness_checks "$nested_host_id"
    fi
  fi

  if [ "$fault_reported" != "1" ]; then
    for id in 1a 1b 1c 1d 1e; do
      if only_run "$id"; then fail "$id: runtime_endpoint never reported fail within 120s of the engine stopping (status='$(check_field "$nested_host_id" runtime_endpoint status)')"; fi
    done
  else
    if only_run 1a; then
      if [ "$(host_status "$nested_host_id")" = "online" ]; then pass "1a: host is online while its runtime is down"; else fail "1a: host status='$(host_status "$nested_host_id")' (want online)"; fi
      local id hb
      hb=$(host_json "$nested_host_id")
      for id in runtime_endpoint startup_cleanup; do
        local status remediation scope enforced gate_enforced
        status=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .status // ""')
        remediation=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .remediation // ""')
        scope=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .blocks.scope // ""')
        enforced=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness[]? | select(.id==$id) | .blocks.enforced_by // ""')
        gate_enforced=$(printf '%s' "$hb" | jq -r --arg id "$id" '.host.readiness_gate.blocking[]? | select(.check_id==$id) | .enforced_by // ""')
        if [ "$status" = "fail" ] && [ -n "$remediation" ]; then pass "1a: $id fails with its fix text"; else fail "1a: $id status='$status' remediation_len=${#remediation} (want fail with fix text)"; fi
        if [ "$scope" = "host" ] && [ "$enforced" = "agent" ]; then pass "1a: $id carries blocks {scope: host, enforced_by: agent}"; else fail "1a: $id blocks scope='$scope' enforced_by='$enforced' (want host/agent)"; fi
        if [ "$gate_enforced" = "agent" ]; then pass "1a: $id listed in readiness_gate.blocking, enforced_by agent"; else fail "1a: $id not in blocking as agent-enforced (enforced_by='$gate_enforced')"; fi
      done
    fi

    if only_run 1b; then
      local h1; h1=$(nested_health)
      case "$h1" in
        *" 503") pass "1b: agent health answers 503 while the runtime is down" ;;
        "") unperformed "1b: the nested agent's health endpoint could not be read" ;;
        *) fail "1b: agent health answered '$h1' while the runtime is down (want 503)" ;;
      esac
    fi

    # 1d runs UNDER THE FAULT: a 409 after recovery would prove nothing.
    if only_run 1d; then
      local id put
      for id in runtime_endpoint startup_cleanup; do
        put=$(http_raw PUT "admin/hosts/$nested_host_id/readiness-overrides/$id" "$ADMIN_TOK")
        if [ "$(http_status "$put")" = "409" ]; then pass "1d: override PUT on $id refused 409"; else fail "1d: override PUT on $id got $(http_status "$put") (want 409)"; fi
      done
    fi

    # 1c: the real agent is stopped so the nested host is the only candidate,
    # and stays stopped until the recovery launch has been asserted.
    if only_run 1c && [ "$nested_servable" != "1" ]; then
      unperformed "1c: the nested host could not serve a launch before the fault on this host ($nested_unservable_why), so a refusal could not have readiness as its sole reason"
    elif only_run 1c; then
      local raw st code
      raw=$(launch_app_raw "$APP_NOHOME_ID" "$ADMIN_TOK")
      st=$(http_status "$raw"); code=$(http_body "$raw" | json_get error.code)
      if [ "$st" = "503" ] && [ "$code" = "host_not_ready" ]; then
        pass "1c: launch refused by the control plane, 503 host_not_ready"
      else
        fail "1c: expected 503 host_not_ready, got $st code=$code; nested host: $(host_json "$nested_host_id" | jq -c '.host | {status, gate: .readiness_gate, capacity, capacity_detection}') gpus: $(http_body "$(http_raw GET "hosts/$nested_host_id/gpus" "$ADMIN_TOK")" | jq -c '[.items[]? | {gpu_index, slots_total, vram_mb_total, render_node, device_path}]') settings: $(host_json "$nested_host_id" | jq -c '.host.effective_settings // null') hosts_online: $(hosts_list_json | jq -c '[.items[] | select(.status=="online") | .node_name]')"
      fi
      if http_has_header "$raw" "Retry-After"; then fail "1c: host_not_ready carried Retry-After"; else pass "1c: no Retry-After on host_not_ready"; fi
    fi
  fi

  # ── Clear: start the nested dockerd again ────────────────────────────────
  local recover_since; recover_since=$(now_epoch)
  dind_exec sh -c 'setsid nohup dockerd --live-restore --host=unix:///var/run/docker.sock >/var/log/dockerd.log 2>&1 </dev/null &' >/dev/null 2>&1 || true
  poll_until 60 3 dind_docker info >/dev/null 2>&1 || true

  if [ "$fault_reported" = "1" ]; then
    if only_run 1a; then
      if wait_check_status "$nested_host_id" runtime_endpoint pass 120 "$recover_since"; then
        pass "1a: runtime_endpoint recovers to pass"
        local still
        still=$(host_json "$nested_host_id" | jq -r '[.host.readiness_gate.blocking[]? | select(.check_id=="runtime_endpoint" or .check_id=="startup_cleanup")] | length')
        if [ "$still" = "0" ]; then pass "1a: both checks left readiness_gate.blocking"; else fail "1a: $still runtime check(s) still in blocking after recovery"; fi
      else
        fail "1a: runtime_endpoint did not recover to pass within 120s"
      fi
    fi
    if only_run 1b; then
      local h2=""
      poll_until 60 3 bash -c "docker exec '${RID}-dind' sh -c 'wget -q -S -O /dev/null http://127.0.0.1:9191/health 2>&1' | grep -q ' 200'" && h2=200
      if [ "$h2" = "200" ]; then pass "1b: agent health returns to 200"; else fail "1b: agent health did not return to 200 (last '$(nested_health)')"; fi
    fi
    if only_run 1e; then
      local started_at2 restarts2 pid_after
      started_at2=$(dind_docker inspect "${RID}-nested-agent" -f '{{.State.StartedAt}}' 2>/dev/null || echo "")
      restarts2=$(dind_docker inspect "${RID}-nested-agent" -f '{{.RestartCount}}' 2>/dev/null || echo "")
      pid_after=$(nested_agent_pid)
      if [ -n "$started_at1" ] && [ "$started_at1" = "$started_at2" ] && [ "$restarts1" = "$restarts2" ]; then pass "1e: agent container never restarted (StartedAt and RestartCount unchanged)"; else fail "1e: agent container restarted (StartedAt '$started_at1' -> '$started_at2', RestartCount '$restarts1' -> '$restarts2')"; fi
      if [ -n "$pid_fault" ] && [ "$pid_fault" = "$pid_after" ]; then pass "1e: the agent process that reported the fault is the one that resumed (pid unchanged)"; else fail "1e: agent pid changed across the resume ('$pid_fault' -> '$pid_after')"; fi
    fi
    if only_run 1c && [ "$nested_servable" != "1" ]; then
      unperformed "1c: recovery launch — the nested host could not serve a launch before the fault either ($nested_unservable_why)"
    elif only_run 1c; then
      if wait_blocking_empty "$nested_host_id" 180; then
        local res2 st2 sid2 placed_host
        res2=$(launch_app "$APP_NOHOME_ID" "$ADMIN_TOK"); st2="${res2%%$'\t'*}"
        if [ "$st2" = "201" ]; then
          sid2=$(printf '%s' "${res2#*$'\t'}" | json_get session.id)
          CREATED_SESSION_IDS+=("$sid2")
          placed_host=$(printf '%s' "${res2#*$'\t'}" | json_get session.host_id)
          if [ "$placed_host" = "$nested_host_id" ]; then pass "1c: recovery launch placed on the recovered host"; else fail "1c: recovery launch placed on '$placed_host' (want the nested host)"; fi
          if wait_session_state "$sid2" running 120; then pass "1c: recovery launch reaches running"; else fail "1c: recovery launch did not reach running (state='$(http_body "$(http_raw GET "sessions/$sid2" "$ADMIN_TOK")" | json_get session.state)')"; fi
          stop_and_wait "$sid2"
        else
          fail "1c: recovery launch got HTTP $st2 (want 201) code=$(printf '%s' "${res2#*$'\t'}" | json_get error.code) nested_blocking=$(host_json "$nested_host_id" | jq -c '[.host.readiness_gate.blocking[]?.check_id]')"
        fi
      else
        fail "1c: nested host still blocked 180s after recovery: $(host_json "$nested_host_id" | jq -c '[.host.readiness[]? | select(.blocks != null and .status == "fail") | {id, summary}]')"
      fi
    fi
  fi

  collect_readiness_checks "$nested_host_id"

  # Tear the nested fixture down at the END of this scenario, not only in
  # the run-wide cleanup trap: stop the agent -> DELETE its host row,
  # asserting 204 -> rm dind + network.
  dind_docker stop "${RID}-nested-agent" >/dev/null 2>&1 || true
  local del; del=$(http_raw DELETE "hosts/$nested_host_id" "$ADMIN_TOK")
  if [ "$(http_status "$del")" = "204" ]; then pass "1: nested host row deleted 204"; else fail "1: nested host row delete got $(http_status "$del") (want 204)"; fi
  docker rm -f -v "${RID}-dind" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  NESTED_ENGINE_STARTED=0
  start_real_agent
}

# ── Run scenarios ────────────────────────────────────────────────────────────
scenario_1
scenario_2a
scenario_2b
scenario_3
scenario_4a
scenario_4a_prime
scenario_4b
scenario_5a
scenario_5b
scenario_6
scenario_7
scenario_8

# Falls through to the EXIT trap (cleanup), which tears everything down,
# verifies ownership cleanup (scenario 9), computes the verdict note, and
# calls finish_run.
