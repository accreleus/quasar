#!/usr/bin/env bash
# run-admission.sh — #383 end-to-end admission harness (encode slots + live
# free-VRAM veto). See docs/design/plans/2026-07-26-383-vram-admission-telemetry-spec.md §7.2.
#
# There is NO end-to-end admission coverage today: run-p3-multihost.sh
# deliberately RAISES the user quota to 10 to avoid rejection, and the only
# 503 assertion anywhere is one no-host conformance step (scripts/harness/apitest/main.go:146).
# This closes that gap.
#
# Runs ON THE HOST (not via `scripts/dev/dev.sh run` — that mounts only the repo
# with no docker socket, and this harness needs `docker exec` against the
# postgres container for the veto-fires / fail-open steps' direct DB mutation,
# per the spec's review finding #18).
#
# Usage:
#   scripts/harness/run-admission.sh [--stack=hermes|tower]
#
# Env overrides:
#   API=...                    override the derived control-plane base URL
#   QUASAR_VRAM_MIN_FREE_MB     default 1024 (#383 spec §4.3 knob default)
#   QUASAR_VRAM_STALENESS_SECS  default 20   (#383 spec §4.3 knob default)
#   ADMISSION_ALLOW_SHARED=1    proceed even if foreign active sessions are
#                               found on the stack (loud warning; see step 0)
#
# Asserts (spec §7.2, numbered to match):
#   1. telemetry precondition        — delegates to scripts/harness/checks/vram-telemetry.sh
#   2. slot exhaustion                — 503 capacity_exhausted + no session row persisted
#   3. release frees capacity         — stop one session, next launch is admitted
#   4. veto fires                     — DB-mutate gpus.vram_mb_free low (slots free), next launch rejected
#   5. fail-open                      — age vram_sampled_at past staleness, launch still admitted
#   6. veto is diagnosable            — step 4's rejection left evidence (control-plane log)
#   7. no declared-VRAM dependence    — an app declaring an absurd VRAM value launches anyway
#
# Steps 4/5 drive the veto by DIRECT DB MUTATION of gpus.vram_mb_free /
# vram_sampled_at (never a control-plane restart — that drops every agent
# websocket mid-run and makes the harness non-idempotent, spec §7.2 step 4/5
# note). The original values are snapshotted before the first mutation and
# restored by the cleanup trap, so an aborted run never wedges the stack.
#
# If the server side of #383 has not merged onto this stack (no vram_mb_free
# column, or the API doesn't yet report the vram_* fields), steps 4-6 SKIP
# with a clear message rather than fail — this harness is expected to be
# runnable (and mostly-skip) against a pre-#383 stack, and to light up fully
# once the schema + scheduler land. See CLAUDE.md's build/test note: this
# script cannot be validated end-to-end until the control-plane side merges.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/harness/lib/harness.sh
source "$ROOT/scripts/harness/lib/harness.sh"
# shellcheck source=scripts/harness/checks/vram-telemetry.sh
source "$ROOT/scripts/harness/checks/vram-telemetry.sh"

# ── Args ─────────────────────────────────────────────────────────────────────
STACK="hermes"
for a in "$@"; do
  case "$a" in
    --stack=*) STACK="${a#*=}" ;;
    -h | --help)
      sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown arg: $a" >&2
      exit 2
      ;;
  esac
done
case "$STACK" in
  hermes) DEFAULT_TLS_PORT=8443 ;;
  tower) DEFAULT_TLS_PORT=18443 ;;
  *)
    echo "unknown --stack=$STACK (hermes|tower)" >&2
    exit 2
    ;;
esac

# ── Config ───────────────────────────────────────────────────────────────────
if [ -f "$ROOT/deploy/.env" ]; then
  set -a
  # shellcheck source=/dev/null
  . "$ROOT/deploy/.env"
  set +a
fi
# Browser-facing routes are HTTPS-only since the HTTP->HTTPS redirect shipped
# (develop 20b1d33): plain HTTP answers /v1/* with a 308 and an empty body, so a
# harness defaulting to http:// sees "login returned no token" and skips against
# a perfectly healthy stack. Default to the TLS listener; curl uses -k because
# the default cert is the self-signed one generated at first boot. Only /health
# and the agent surface stay on plain HTTP (#376).
API="${API:-https://localhost:${QUASAR_TLS_PORT:-$DEFAULT_TLS_PORT}}"
ADMIN_EMAIL="${BOOTSTRAP_ADMIN_EMAIL:?need BOOTSTRAP_ADMIN_EMAIL in deploy/.env}"
ADMIN_PASS="${BOOTSTRAP_ADMIN_PASSWORD:?need BOOTSTRAP_ADMIN_PASSWORD in deploy/.env}"
TEST_USER_EMAIL="admission-harness@quasar.local"
TEST_USER_PASS="AdmissionHarness123!"
TEST_USER_NAME="admissionharness"

VRAM_MIN_FREE_MB="${QUASAR_VRAM_MIN_FREE_MB:-1024}"
VRAM_STALENESS_SECS="${QUASAR_VRAM_STALENESS_SECS:-20}"

harness_init "admission"
require curl python3 docker base64

harness_note "api" "$API"
harness_note "stack" "$STACK"
harness_note "vram_min_free_mb" "$VRAM_MIN_FREE_MB"
harness_note "vram_staleness_secs" "$VRAM_STALENESS_SECS"

# ── HTTP helpers (status+body split, like run-p5-home.sh's user_post_raw) ────
http_json() { # $1 METHOD  $2 v1-relative path  $3 bearer token  [$4 JSON body]
  local method="$1" path="$2" tok="$3" body="${4:-}"
  if [ -n "$body" ]; then
    curl -sk --connect-timeout 5 --max-time 20 -w '\n__STATUS__%{http_code}' \
      -X "$method" "$API/v1/$path" \
      -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sk --connect-timeout 5 --max-time 20 -w '\n__STATUS__%{http_code}' \
      -X "$method" "$API/v1/$path" -H "Authorization: Bearer $tok"
  fi
}
http_status() { echo "${1##*__STATUS__}"; }
http_body() { echo "${1%$'\n'__STATUS__*}"; }

json_get() { python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    for k in '$1'.split('.'):
        d = d[k] if not k.isdigit() else d[int(k)]
    print(d if d is not None else '')
except Exception:
    print('')
"; }

admin_session_ids() {
  http_body "$(http_json GET admin/sessions "$ADMIN_TOK")" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(','.join(sorted(s['id'] for s in d.get('items', []))))
except Exception:
    print('__error__')
"
}

# ── DB access (docker exec psql, like run-p1-10-demo.sh's psql_cp) ──────────
PG_CONTAINER=""
resolve_pg_container() {
  [ -n "$PG_CONTAINER" ] && return 0
  local candidate="deploy-quasar-postgres-1"
  if docker inspect "$candidate" >/dev/null 2>&1; then
    PG_CONTAINER="$candidate"
    return 0
  fi
  candidate="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 'quasar-postgres' || true)"
  [ -n "$candidate" ] || return 1
  PG_CONTAINER="$candidate"
}
psql_cp() {
  docker exec -e PGPASSWORD="${POSTGRES_PASSWORD:-quasar}" "$PG_CONTAINER" \
    psql -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-quasar}" -d quasar -tAc "$1"
}

# read_vram_state <gpu_id> <staleness_secs> — echoes "<vram_mb_free>\t<is_stale t|f>"
# for ONE gpu row. Used by the assertion-4/5 race guard: the agent heartbeat
# (<=5s) can land between a DB mutation and the launch attempt it's meant to
# drive, silently overwriting vram_mb_free AND vram_sampled_at together (both
# columns move on one heartbeat write) and invalidating the assertion. Reading
# both columns back before AND after the launch lets the caller detect that
# and retry instead of recording a spurious result.
# Returns "<free>\t<is_stale>" for one GPU. NB `boolean::text` renders
# 'true'/'false', NOT psql's bare-column 't'/'f' — comparing against "f" here
# silently never matched, so assertion 4 declared "the heartbeat won the race"
# on all five attempts while the mutation had in fact held perfectly.
read_vram_state() {
  psql_cp "SELECT COALESCE(vram_mb_free::text,'__NULL__') || E'\t' || ((now() - vram_sampled_at) > make_interval(secs => $2))::text FROM gpus WHERE id='$1';"
}

CP_CONTAINER=""
resolve_cp_container() {
  [ -n "$CP_CONTAINER" ] && return 0
  local candidate="deploy-quasar-control-plane-1"
  if docker inspect "$candidate" >/dev/null 2>&1; then
    CP_CONTAINER="$candidate"
    return 0
  fi
  candidate="$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 'quasar-control-plane' || true)"
  [ -n "$candidate" ] || return 1
  CP_CONTAINER="$candidate"
}

# ── Cleanup: EVERY launched session torn down, even on failure or Ctrl-C;
#    any DB mutation restored from its snapshot. Calls harness_report last
#    (see scripts/harness/lib/harness.sh's "own cleanup trap" convention). ───────────
CREATED_SESSION_IDS=()
NEW_APP_IDS=()
VRAM_SNAPSHOT_FILE=""
VRAM_MUTATED=0
ADMIN_TOK=""
USER_TOK=""
# Set only when the harness REUSED an existing app (repeat run) rather than
# creating a fresh one — snapshot of its enabled/default_vram_mb so cleanup
# can restore both, instead of leaving a repeat run's app permanently enabled
# with default_vram_mb patched to the assertion-7 absurd value (finding #4).
APP_ID=""
APP_ORIGINAL_ENABLED=""
APP_ORIGINAL_VRAM=""

restore_vram_snapshot() {
  [ -f "$VRAM_SNAPSHOT_FILE" ] || return 0
  [ -n "$PG_CONTAINER" ] || return 0
  local gid free ts agent_ms free_sql ts_sql agent_sql
  while IFS=$'\t' read -r gid free ts agent_ms; do
    [ -n "$gid" ] || continue
    [ "$free" = "__NULL__" ] && free_sql="NULL" || free_sql="$free"
    [ "$ts" = "__NULL__" ] && ts_sql="NULL" || ts_sql="'$ts'"
    [ "$agent_ms" = "__NULL__" ] && agent_sql="NULL" || agent_sql="$agent_ms"
    psql_cp "UPDATE gpus SET vram_mb_free=$free_sql, vram_sampled_at=$ts_sql, vram_sample_agent_ms=$agent_sql WHERE id='$gid';" >/dev/null 2>&1 \
      || echo "  WARNING: failed to restore vram columns for gpu $gid" >&2
  done <"$VRAM_SNAPSHOT_FILE"
}

cleanup() {
  # Capture on the FIRST line, before any other command in this function can
  # overwrite $? — this is what was in flight when the EXIT trap fired (e.g.
  # a `set -e` trip mid-script). Everything below is teardown busywork that
  # would otherwise clobber it; harness_report at the end gets it explicitly
  # so a crash is never silently reported as a clean PASS (scripts/harness/lib/harness.sh
  # "Cleanup trap" doc).
  local rc=$?
  echo ""
  echo "== cleanup =="
  local sid
  for sid in "${CREATED_SESSION_IDS[@]:-}"; do
    [ -n "$sid" ] || continue
    curl -sk --connect-timeout 5 --max-time 15 -X DELETE "$API/v1/sessions/$sid" \
      -H "Authorization: Bearer ${ADMIN_TOK:-}" >/dev/null 2>&1 || true
  done
  [ -n "${CREATED_SESSION_IDS[*]:-}" ] && echo "  ${#CREATED_SESSION_IDS[@]} harness session(s) stopped"

  if [ "$VRAM_MUTATED" = "1" ]; then
    restore_vram_snapshot
    echo "  gpus.vram_* columns restored from snapshot"
  fi

  # Belt-and-braces: provisioning restores this inline, but an abort between the
  # flip and that restore must never leave registration more open than we found it.
  if [ -n "${ORIGINAL_REG_MODE:-}" ]; then
    restore_registration_mode
    echo "  registration_mode restored"
  fi
  [ -n "$VRAM_SNAPSHOT_FILE" ] && rm -f "$VRAM_SNAPSHOT_FILE"

  local aid
  for aid in "${NEW_APP_IDS[@]:-}"; do
    [ -n "$aid" ] || continue
    curl -sk --connect-timeout 5 --max-time 15 -X PATCH "$API/v1/apps/$aid" \
      -H "Authorization: Bearer ${ADMIN_TOK:-}" -H 'Content-Type: application/json' \
      -d '{"enabled":false}' >/dev/null 2>&1 || true
  done

  # A REUSED app (repeat run — not in NEW_APP_IDS, so the loop above never
  # touches it) was snapshotted before assertion 7 patched default_vram_mb to
  # an absurd value and before this run re-enabled it. Restore both, or a
  # repeat run leaves "Admission Harness: No-VRAM App" permanently enabled
  # with default_vram_mb=999999 in the user's library.
  if [ -n "${APP_ID:-}" ] && [ -n "${APP_ORIGINAL_ENABLED:-}" ]; then
    curl -sk --connect-timeout 5 --max-time 15 -X PATCH "$API/v1/apps/$APP_ID" \
      -H "Authorization: Bearer ${ADMIN_TOK:-}" -H 'Content-Type: application/json' \
      -d "{\"enabled\":$APP_ORIGINAL_ENABLED,\"default_vram_mb\":$APP_ORIGINAL_VRAM}" >/dev/null 2>&1 \
      || echo "  WARNING: failed to restore reused app $APP_ID's original enabled=$APP_ORIGINAL_ENABLED / default_vram_mb=$APP_ORIGINAL_VRAM" >&2
  fi

  harness_report "$rc"
}
trap cleanup EXIT

# ── 0. Login ─────────────────────────────────────────────────────────────────
echo "== 0. login =="
if ! ADMIN_TOK=$(curl -sk --connect-timeout 5 --max-time 15 -f -X POST "$API/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" 2>/dev/null | json_get access_token); then
  fail "admin login failed against $API — is the stack up?"
  exit 1
fi
[ -n "$ADMIN_TOK" ] || { fail "admin login returned no access_token"; exit 1; }
pass "admin token obtained"

login_test_user() {
  curl -sk --connect-timeout 5 --max-time 15 -f -X POST "$API/v1/auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"email\":\"$TEST_USER_EMAIL\",\"password\":\"$TEST_USER_PASS\"}" 2>/dev/null | json_get access_token
}

# Provisioning the harness user is fiddlier than it looks, and getting it wrong
# is how this harness failed its first two hermes runs:
#   * /v1/auth/register is INVITE-GATED (W1 LP-SEC-01), so a bare register POST
#     cannot work. qses gets away with one only because its harness user was
#     registered by hand before invite-gating landed.
#   * With registration_mode = `closed` — the default, and what hermes runs —
#     the invite system is off ENTIRELY: minting succeeds (201) but redeeming
#     the code still returns 403 registration_closed.
# So: log in if the user already exists (the repeat-run path); otherwise flip
# registration_mode to invite_only just long enough to redeem a single-use
# invite, and restore the original mode in the cleanup trap. The flip is
# snapshotted BEFORE it happens, so an abort mid-provision cannot leave a
# stack more open than it was found.
ORIGINAL_REG_MODE=""
restore_registration_mode() {
  [ -n "$ORIGINAL_REG_MODE" ] || return 0
  local resp status
  resp=$(curl -sk --connect-timeout 5 --max-time 15 -w '\n__STATUS__%{http_code}' \
    -X PATCH "$API/v1/admin/settings" \
    -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
    -d "{\"registration_mode\":\"$ORIGINAL_REG_MODE\"}" 2>/dev/null)
  status="$(http_status "$resp")"
  if [ "$status" = "200" ]; then
    # Only clear the guard on a CONFIRMED restore. Clearing it unconditionally
    # (the old behavior) made a failed PATCH invisible: the cleanup-trap
    # backstop checks `[ -n "$ORIGINAL_REG_MODE" ]` and would see it empty,
    # so a stack could be left in invite_only with no warning at all.
    ORIGINAL_REG_MODE=""
  else
    echo "  WARNING: failed to restore registration_mode to '$ORIGINAL_REG_MODE' (PATCH /v1/admin/settings -> HTTP $status) — leaving the guard set so cleanup retries; stack may still be in a more-open registration_mode than it started in" >&2
  fi
}

USER_TOK="$(login_test_user || true)"
if [ -z "$USER_TOK" ]; then
  REG_MODE=$(http_body "$(http_json GET admin/settings "$ADMIN_TOK")" | json_get settings.registration_mode)
  if [ "$REG_MODE" = "closed" ]; then
    ORIGINAL_REG_MODE="$REG_MODE"
    http_json PATCH admin/settings "$ADMIN_TOK" '{"registration_mode":"invite_only"}' >/dev/null 2>&1 || true
  fi
  # NB: the mint response is wrapped — {"invite":{"code":...}}, not a bare code.
  INVITE_CODE=$(http_body "$(http_json POST admin/invites "$ADMIN_TOK" '{"role":"user","max_uses":1}')" | json_get invite.code)
  if [ -z "$INVITE_CODE" ]; then
    restore_registration_mode
    fail "could not mint an invite for the harness test user (POST /v1/admin/invites returned no code)"
    exit 1
  fi
  REG_RESP=$(http_json POST auth/register "" "{\"email\":\"$TEST_USER_EMAIL\",\"username\":\"$TEST_USER_NAME\",\"password\":\"$TEST_USER_PASS\",\"invite_code\":\"$INVITE_CODE\"}")
  REG_STATUS=$(http_status "$REG_RESP")
  restore_registration_mode
  USER_TOK="$(login_test_user || true)"
  [ -n "$USER_TOK" ] \
    || { fail "harness test-user provisioning failed: register -> HTTP $REG_STATUS $(http_body "$REG_RESP")"; exit 1; }
fi
[ -n "$USER_TOK" ] \
  || { fail "harness test-user login failed (invite-gated registration also did not yield a usable account)"; exit 1; }
[ -n "$USER_TOK" ] || { fail "harness test-user login returned no access_token"; exit 1; }
TEST_USER_ID=$(http_body "$(http_json GET me "$USER_TOK")" | json_get user.id)
[ -n "$TEST_USER_ID" ] || { fail "could not resolve harness test-user id via GET /v1/me"; exit 1; }
pass "harness test user ready ($TEST_USER_ID)"

# ── 0b. Refuse to share a stack with someone else's live session ────────────
# "must refuse to run against a stack that already has foreign active
# sessions (or clearly report that it is sharing the box)" — spec §7.2.
echo "== 0b. foreign active-session guard =="
ACTIVE_JSON=$(http_body "$(http_json GET admin/sessions "$ADMIN_TOK")")
FOREIGN_COUNT=$(printf '%s' "$ACTIVE_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
active = {'pending', 'assigned', 'starting', 'running', 'stopping'}
foreign = [s for s in d.get('items', [])
           if s.get('state') in active and s.get('user_id') != '$TEST_USER_ID']
print(len(foreign))
" 2>/dev/null || echo "")
if [ -z "$FOREIGN_COUNT" ]; then
  fail "could not evaluate the foreign-active-session guard (GET /v1/admin/sessions unparsable)"
  exit 1
fi
if [ "$FOREIGN_COUNT" -gt 0 ]; then
  if [ "${ADMISSION_ALLOW_SHARED:-0}" = "1" ]; then
    echo "  WARNING: $FOREIGN_COUNT foreign active session(s) on this stack — proceeding anyway (ADMISSION_ALLOW_SHARED=1). Slot-exhaustion steps may starve them." >&2
    skip "stack is SHARED: $FOREIGN_COUNT foreign active session(s) present (proceeding under ADMISSION_ALLOW_SHARED=1)"
  else
    fail "refusing to run: $FOREIGN_COUNT foreign active session(s) already on this stack (rerun with ADMISSION_ALLOW_SHARED=1 to override — slot exhaustion WILL affect them)"
    exit 1
  fi
else
  pass "no foreign active sessions — stack is exclusively ours for this run"
fi

# ── App: created WITHOUT a VRAM field (also serves assertion 7) ─────────────
echo "== seed: no-VRAM test app =="
APP_NAME="Admission Harness: No-VRAM App"
APP_LIST=$(http_body "$(http_json GET admin/apps "$ADMIN_TOK")")
# Tab-separated id/enabled/default_vram_mb in one pass, using json.dumps (not
# Python's print(bool)) for the latter two so they're valid JSON literals when
# spliced straight into cleanup's restore PATCH body later.
EXISTING_APP_INFO=$(printf '%s' "$APP_LIST" | python3 -c "
import sys, json
d = json.load(sys.stdin)
m = next((a for a in d.get('items', []) if a.get('name') == '''$APP_NAME'''), None)
if m:
    print('%s\t%s\t%s' % (m.get('id', ''), json.dumps(m.get('enabled', True)), json.dumps(m.get('default_vram_mb', 1024))))
" 2>/dev/null || echo "")
if [ -n "$EXISTING_APP_INFO" ]; then
  IFS=$'\t' read -r APP_ID APP_ORIGINAL_ENABLED APP_ORIGINAL_VRAM <<<"$EXISTING_APP_INFO"
fi
if [ -z "$APP_ID" ]; then
  APP_BODY='{"name":"'"$APP_NAME"'","runtime_spec":{"image":"quasar-agent-dev:latest","args":["sleep","inf"],"env":{},"mounts":[],"gpu":false},"default_encode_slots":1,"default_width":640,"default_height":480,"default_fps":30,"default_bitrate_kbps":2000}'
  APP_RAW=$(http_json POST apps "$ADMIN_TOK" "$APP_BODY")
  [ "$(http_status "$APP_RAW")" = "201" ] || { fail "failed to create the no-VRAM test app: $(http_body "$APP_RAW")"; exit 1; }
  APP_ID=$(http_body "$APP_RAW" | json_get app.id)
  NEW_APP_IDS+=("$APP_ID")
fi
curl -sk --connect-timeout 5 --max-time 15 -X PATCH "$API/v1/apps/$APP_ID" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"enabled":true}' >/dev/null
pass "no-VRAM test app ready: $APP_ID"

# ── Survey capacity: total available encode slots + max GPU vram_mb_total
#    across every ONLINE host, and whether the vram_* fields are present. ────
echo "== survey: capacity + vram telemetry field presence =="
HOSTS_JSON=$(http_body "$(http_json GET hosts "$ADMIN_TOK")")
ONLINE_HOST_IDS=$(printf '%s' "$HOSTS_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for h in d.get('items', []):
    if h.get('status') == 'online':
        print(h['id'])
" 2>/dev/null || true)
[ -n "$ONLINE_HOST_IDS" ] || { fail "no online hosts — bring the stack up first"; exit 1; }

GPU_SURVEY_COMBINED="$(mktemp "${TMPDIR:-/tmp}/admission-gpu-survey.XXXXXX")"
while IFS= read -r hid; do
  [ -n "$hid" ] || continue
  GJSON=$(http_body "$(http_json GET "hosts/$hid/gpus" "$ADMIN_TOK")")
  printf 'HOST\t%s\t%s\n' "$hid" "$(printf '%s' "$GJSON" | base64 | tr -d '\n')" >>"$GPU_SURVEY_COMBINED"
done <<EOF_HOSTS
$ONLINE_HOST_IDS
EOF_HOSTS

GPU_SURVEY=$(python3 - "$GPU_SURVEY_COMBINED" <<'PYEOF'
import sys, json, base64

with open(sys.argv[1], encoding="utf-8") as fh:
    lines = [l.rstrip("\n") for l in fh if l.strip()]
for line in lines:
    parts = line.split("\t")
    if parts[0] != "HOST":
        continue
    hid = parts[1]
    b64 = parts[2] if len(parts) > 2 else ""
    try:
        d = json.loads(base64.b64decode(b64).decode("utf-8", "replace"))
    except Exception:
        continue
    for g in d.get("items", []):
        gid = g.get("gpu_id", "")
        total = g.get("vram_mb_total") or 0
        slots_total = g.get("slots_total") or 0
        slots_reserved = g.get("slots_reserved") or 0
        avail = max(0, slots_total - slots_reserved)
        has_vram_fields = "yes" if ("vram_mb_free" in g and "vram_sampled_at" in g) else "no"
        print(f"{gid}\t{hid}\t{avail}\t{total}\t{has_vram_fields}")
PYEOF
)
rm -f "$GPU_SURVEY_COMBINED"

TOTAL_AVAILABLE_SLOTS=0
MAX_VRAM_TOTAL=0
# The specific gpu_id that achieved MAX_VRAM_TOTAL — assertions 4/5 mutate and
# read back THIS one gpu row (not a blanket UPDATE across every gpu), so the
# race-detection re-reads below have a single unambiguous row to check.
VETO_TARGET_GPU_ID=""
VRAM_FIELDS_PRESENT="no"
while IFS=$'\t' read -r gid _hid avail total has_fields; do
  [ -n "$avail" ] || continue
  TOTAL_AVAILABLE_SLOTS=$((TOTAL_AVAILABLE_SLOTS + avail))
  if [ "${total:-0}" -gt "$MAX_VRAM_TOTAL" ]; then
    MAX_VRAM_TOTAL="$total"
    VETO_TARGET_GPU_ID="$gid"
  fi
  [ "$has_fields" = "yes" ] && VRAM_FIELDS_PRESENT="yes"
done <<<"$GPU_SURVEY"

harness_note "total_available_slots_upper_bound" "$TOTAL_AVAILABLE_SLOTS"
harness_note "max_vram_mb_total" "$MAX_VRAM_TOTAL"
harness_note "vram_fields_present_in_api" "$VRAM_FIELDS_PRESENT"

[ "$TOTAL_AVAILABLE_SLOTS" -gt 0 ] || { fail "no available encode slots across any online host — cannot run the harness"; exit 1; }
if [ "$TOTAL_AVAILABLE_SLOTS" -gt 50 ]; then
  echo "  capping the informational total at 50 (fleet reports $TOTAL_AVAILABLE_SLOTS available)"
  TOTAL_AVAILABLE_SLOTS=50
fi
# INFORMATIONAL ONLY, and an UPPER BOUND — this sums slots_available across
# EVERY online GPU, but schedulableBindingSQL (control-plane scheduler)
# restricts launch candidates to the GPU(s) matching the host's effective
# encoder (e.g. a Tower running encoder=vulkan is only schedulable on GPU
# index 0). So this number is routinely higher than the real number of
# launches the exhaustion loop below can make before a 503 — it must NEVER be
# used to predict how many launches "should" succeed (that miscount is what
# made a real, expected 503 look like a harness setup failure on the first
# live Tower run of this script).
echo "  total available encode slots across all online GPUs (upper bound, IGNORES per-host encoder->GPU binding): $TOTAL_AVAILABLE_SLOTS"
echo "  max vram_mb_total across online GPUs: $MAX_VRAM_TOTAL"
echo "  vram_mb_free/vram_sampled_at present in GET /v1/hosts/{id}/gpus: $VRAM_FIELDS_PRESENT"

# Encode-slot launches this harness may need across its whole run: the
# exhaustion loop below (bounded by ADMISSION_LAUNCH_CAP) plus one each for
# the release/veto/fail-open/no-declared-VRAM steps. Generous on purpose so
# the user quota is never the thing that trips a 503 instead of real capacity.
ADMISSION_LAUNCH_CAP=12
curl -sk --connect-timeout 5 --max-time 15 -X PATCH "$API/v1/users/$TEST_USER_ID" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d "{\"max_concurrent_sessions\":$((ADMISSION_LAUNCH_CAP + 10))}" >/dev/null 2>&1 || true

LAUNCHES_SUCCEEDED=0
launch_no_vram_app() { # echoes "<status>\t<body>"
  local raw
  raw=$(http_json POST sessions "$USER_TOK" "{\"app_id\":\"$APP_ID\"}")
  printf '%s\t%s\n' "$(http_status "$raw")" "$(http_body "$raw")"
}

# ── 1. Telemetry precondition (delegates to scripts/harness/checks/vram-telemetry.sh) ─
echo "== 1. telemetry precondition =="
vram_telemetry_check "$VRAM_STALENESS_SECS"

# ── 1b/2. Exhaust encode slots by launching until a 503 arrives, and assert
#    on the first one. The real number of launchable slots is not knowable
#    from the survey above (it ignores encoder->GPU scheduling binding), so
#    this does not predict a launch count — it launches in a loop, bounded by
#    ADMISSION_LAUNCH_CAP, until a launch is rejected. Hitting the cap without
#    ever seeing a rejection is itself a failure (either the cap is too low
#    for this fleet, or slot exhaustion never fires). ───────────────────────
echo "== 1b. exhaust encode slots (launch until 503, cap $ADMISSION_LAUNCH_CAP) =="
EXHAUSTION_STATUS=""
EXHAUSTION_BODY=""
BEFORE_REJECTED_IDS=""
i=0
while [ "$i" -lt "$ADMISSION_LAUNCH_CAP" ]; do
  BEFORE_REJECTED_IDS=$(admin_session_ids)
  RESULT=$(launch_no_vram_app)
  ST="${RESULT%%$'\t'*}"; BODY="${RESULT#*$'\t'}"
  if [ "$ST" = "201" ]; then
    SID=$(printf '%s' "$BODY" | json_get session.id)
    [ -n "$SID" ] || { fail "slot-exhaustion setup: launch $((i + 1))/$ADMISSION_LAUNCH_CAP got 201 with no session.id: $BODY"; break; }
    CREATED_SESSION_IDS+=("$SID")
    LAUNCHES_SUCCEEDED=$((LAUNCHES_SUCCEEDED + 1))
    i=$((i + 1))
    continue
  fi
  EXHAUSTION_STATUS="$ST"
  EXHAUSTION_BODY="$BODY"
  break
done
echo "  ${#CREATED_SESSION_IDS[@]} session(s) holding slots ($i successful launch(es) before rejection or cap)"

# ── 2. Slot exhaustion: 503 capacity_exhausted + no session row persisted ───
echo "== 2. assertion: slot exhaustion -> 503 + no persisted row =="
if [ -z "$EXHAUSTION_STATUS" ]; then
  fail "assertion 2: launched $ADMISSION_LAUNCH_CAP session(s) with no 503 — either ADMISSION_LAUNCH_CAP is too low for this fleet's real (encoder-bound) slot count, or slot exhaustion never fires"
else
  if [ "$EXHAUSTION_STATUS" = "503" ]; then
    CODE=$(printf '%s' "$EXHAUSTION_BODY" | json_get error.code)
    if [ "$CODE" = "capacity_exhausted" ]; then
      pass "assertion 2: slot exhaustion -> 503 capacity_exhausted"
    else
      fail "assertion 2: got 503 but error.code='$CODE' (want capacity_exhausted): $EXHAUSTION_BODY"
    fi
  else
    fail "assertion 2: expected 503 on slot exhaustion, got HTTP $EXHAUSTION_STATUS: $EXHAUSTION_BODY"
  fi
  AFTER_REJECTED_IDS=$(admin_session_ids)
  if [ "$BEFORE_REJECTED_IDS" = "$AFTER_REJECTED_IDS" ] && [ "$BEFORE_REJECTED_IDS" != "__error__" ]; then
    pass "assertion 2: no session row persisted for the rejected launch"
  else
    fail "assertion 2: admin/sessions changed after the rejected launch (before=[$BEFORE_REJECTED_IDS] after=[$AFTER_REJECTED_IDS]) — a row may have been persisted"
  fi
fi

# ── 3. Release frees capacity ────────────────────────────────────────────────
echo "== 3. assertion: release frees capacity =="
if [ "${#CREATED_SESSION_IDS[@]}" -gt 0 ]; then
  RELEASE_SID="${CREATED_SESSION_IDS[0]}"
  curl -sk --connect-timeout 5 --max-time 15 -X DELETE "$API/v1/sessions/$RELEASE_SID" \
    -H "Authorization: Bearer $USER_TOK" >/dev/null 2>&1 || true
  sleep 2
  RESULT=$(launch_no_vram_app)
  ST="${RESULT%%$'\t'*}"; BODY="${RESULT#*$'\t'}"
  if [ "$ST" = "201" ]; then
    pass "assertion 3: stopping a session frees capacity — next launch admitted"
    LAUNCHES_SUCCEEDED=$((LAUNCHES_SUCCEEDED + 1))
    NEW_SID=$(printf '%s' "$BODY" | json_get session.id)
    [ -n "$NEW_SID" ] && CREATED_SESSION_IDS+=("$NEW_SID")
  else
    fail "assertion 3: launch after release still rejected (HTTP $ST): $BODY"
  fi
else
  fail "assertion 3: no harness sessions were held — slot-exhaustion setup failed earlier"
fi

# ── Housekeeping: release ALL harness sessions before the veto tests, which
#    require slots to be free so only the VRAM veto can cause a rejection. ──
echo "== housekeeping: releasing all harness sessions before the veto tests =="
for sid in "${CREATED_SESSION_IDS[@]:-}"; do
  [ -n "$sid" ] || continue
  curl -sk --connect-timeout 5 --max-time 15 -X DELETE "$API/v1/sessions/$sid" \
    -H "Authorization: Bearer $USER_TOK" >/dev/null 2>&1 || true
done
CREATED_SESSION_IDS=()
sleep 3

# ── 4/5/6. VRAM veto — needs the DB columns AND at least one online GPU whose
#    vram_mb_total exceeds the floor (otherwise every GPU structurally
#    abstains per spec §4.1 and the test would prove nothing). Also honours
#    the documented QUASAR_VRAM_MIN_FREE_MB=0 kill switch. ───────────────────
VRAM_COLUMNS_PRESENT=0
if resolve_pg_container; then
  CNT=$(psql_cp "SELECT count(*) FROM information_schema.columns WHERE table_name='gpus' AND column_name='vram_mb_free';" 2>/dev/null || echo 0)
  [ "${CNT:-0}" = "1" ] && VRAM_COLUMNS_PRESENT=1
else
  echo "  WARNING: could not locate the postgres container (tried deploy-quasar-postgres-1, then a 'quasar-postgres' name match) — steps 4-6 will skip" >&2
fi

if [ "$VRAM_COLUMNS_PRESENT" != "1" ]; then
  skip "assertion 4 (veto fires): gpus.vram_mb_free column not found on this stack's DB — #383 migration 0033 not applied yet"
  skip "assertion 5 (fail-open): gpus.vram_mb_free column not found on this stack's DB — #383 migration 0033 not applied yet"
  skip "assertion 6 (veto diagnosable): gpus.vram_mb_free column not found on this stack's DB — #383 migration 0033 not applied yet"
elif [ "$VRAM_MIN_FREE_MB" -le 0 ]; then
  skip "assertion 4 (veto fires): QUASAR_VRAM_MIN_FREE_MB<=0 is the documented kill switch (spec §4.3) — veto disabled by design"
  skip "assertion 5 (fail-open): veto disabled (QUASAR_VRAM_MIN_FREE_MB<=0) — nothing to fail open from"
  skip "assertion 6 (veto diagnosable): veto disabled (QUASAR_VRAM_MIN_FREE_MB<=0) — no rejection to diagnose"
elif [ "$MAX_VRAM_TOTAL" -le "$VRAM_MIN_FREE_MB" ]; then
  skip "assertion 4 (veto fires): every online GPU's vram_mb_total ($MAX_VRAM_TOTAL) <= the floor ($VRAM_MIN_FREE_MB) — spec §4.1's structural APU-abstain path would fire instead of the intended low-free path"
  skip "assertion 5 (fail-open): skipped alongside assertion 4 (no valid veto target)"
  skip "assertion 6 (veto diagnosable): skipped alongside assertion 4 (no rejection was produced)"
elif [ -z "$VETO_TARGET_GPU_ID" ]; then
  # Defensive: MAX_VRAM_TOTAL > floor implies the survey loop set this, but
  # don't silently mutate every gpu row (the old blanket-UPDATE behavior) if
  # it somehow didn't.
  skip "assertion 4 (veto fires): no specific gpu_id resolved for the veto target despite MAX_VRAM_TOTAL > floor — survey/parse bug, refusing to blanket-mutate every gpu row"
  skip "assertion 5 (fail-open): skipped alongside assertion 4 (no valid veto target)"
  skip "assertion 6 (veto diagnosable): skipped alongside assertion 4 (no rejection was produced)"
else
  VRAM_SNAPSHOT_FILE="$(mktemp "${TMPDIR:-/tmp}/admission-vram-snapshot.XXXXXX")"
  psql_cp "SELECT id || E'\t' || COALESCE(vram_mb_free::text,'__NULL__') || E'\t' || COALESCE(vram_sampled_at::text,'__NULL__') || E'\t' || COALESCE(vram_sample_agent_ms::text,'__NULL__') FROM gpus;" \
    >"$VRAM_SNAPSHOT_FILE"
  VRAM_MUTATED=1

  LOW_FREE=$((VRAM_MIN_FREE_MB - 1))
  # Sentinel for vram_sample_agent_ms, applied alongside every assertion-4/5
  # mutation below. applyVramSamples' ingest write (vramSampleUpsertSQL,
  # control-plane/internal/agentws/store.go) is monotonic on this column —
  # `WHERE g.vram_sample_agent_ms IS NULL OR g.vram_sample_agent_ms < $5` — so
  # pinning it above any real agent timestamp makes every incoming heartbeat a
  # no-op for as long as the harness needs the mutation to hold. Without this,
  # the <=5s agent heartbeat can (and on the first live Tower run, did)
  # rewrite vram_mb_free/vram_sampled_at before or during the launch attempt
  # every single time — the window is not winnable by retrying, since one
  # `docker exec psql` round-trip alone can cost more than a second.
  #
  # This is safe: restore_vram_snapshot (above) already snapshots and restores
  # vram_sample_agent_ms alongside vram_mb_free/vram_sampled_at on every exit
  # path, including abort (the `cleanup` EXIT trap). And even a hard kill of
  # this script self-heals on the agent's next reconnect — verified by reading
  # control-plane/internal/agentws/store.go: markGPUsStaleAndClearVramSQL
  # unconditionally NULLs vram_mb_used/vram_mb_free/vram_sampled_at AND
  # vram_sample_agent_ms together on every enrollHost/reconnectHost, which
  # does not depend on this harness (or its snapshot) having run at all.
  VRAM_SAMPLE_AGENT_MS_SENTINEL=9223372036854770000
  VRAM_RACE_MAX_ATTEMPTS=5

  # ── 4. veto fires ────────────────────────────────────────────────────────
  # Races the agent heartbeat (<=5s): a heartbeat landing between the UPDATE
  # and the launch restores a healthy vram_mb_free/vram_sampled_at (both
  # columns move together on one heartbeat write), which would admit the
  # launch and record a spurious FAIL. Re-read the target gpu row both right
  # after the mutation AND right after the launch attempt; only trust the
  # launch's result when the mutation was provably still in effect at both
  # points. Bounded retry, then SKIP (not fail) if the race can't be won.
  echo "== 4. assertion: veto fires (slots free, vram_mb_free driven below the floor) =="
  ASSERTION4_HELD=0
  attempt=1
  while [ "$attempt" -le "$VRAM_RACE_MAX_ATTEMPTS" ]; do
    psql_cp "UPDATE gpus SET vram_mb_free = $LOW_FREE, vram_sampled_at = now(), vram_sample_agent_ms = $VRAM_SAMPLE_AGENT_MS_SENTINEL WHERE id='$VETO_TARGET_GPU_ID';" >/dev/null
    PRE_STATE=$(read_vram_state "$VETO_TARGET_GPU_ID" "$VRAM_STALENESS_SECS")
    PRE_FREE="${PRE_STATE%%$'\t'*}"; PRE_STALE="${PRE_STATE#*$'\t'}"
    if [ "$PRE_FREE" != "$LOW_FREE" ] || [ "$PRE_STALE" != "false" ]; then
      echo "  attempt $attempt/$VRAM_RACE_MAX_ATTEMPTS: heartbeat won the race before the launch even fired (free=$PRE_FREE stale=$PRE_STALE) — retrying"
      attempt=$((attempt + 1))
      continue
    fi
    MARK_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    RESULT=$(launch_no_vram_app)
    ST="${RESULT%%$'\t'*}"; BODY="${RESULT#*$'\t'}"
    POST_STATE=$(read_vram_state "$VETO_TARGET_GPU_ID" "$VRAM_STALENESS_SECS")
    POST_FREE="${POST_STATE%%$'\t'*}"; POST_STALE="${POST_STATE#*$'\t'}"
    if [ "$POST_FREE" != "$LOW_FREE" ] || [ "$POST_STALE" != "false" ]; then
      echo "  attempt $attempt/$VRAM_RACE_MAX_ATTEMPTS: heartbeat won the race during the launch (free=$POST_FREE stale=$POST_STALE) — result HTTP $ST is not trustworthy, retrying"
      attempt=$((attempt + 1))
      continue
    fi
    ASSERTION4_HELD=1
    break
  done

  if [ "$ASSERTION4_HELD" != "1" ]; then
    skip "assertion 4 (veto fires): could not hold vram_mb_free=$LOW_FREE on gpu $VETO_TARGET_GPU_ID across a launch attempt after $VRAM_RACE_MAX_ATTEMPTS attempts — the agent heartbeat (<=5s) kept winning the race and restoring a healthy reading before/during every attempt"
    skip "assertion 5 (fail-open): skipped alongside assertion 4 (no held mutation to test fail-open against)"
    skip "assertion 6 (veto diagnosable): skipped alongside assertion 4 (no rejection was produced)"
  else
    if [ "$ST" = "503" ]; then
      CODE=$(printf '%s' "$BODY" | json_get error.code)
      if [ "$CODE" = "capacity_exhausted" ]; then
        pass "assertion 4: veto fires — launch rejected with slots free (503 capacity_exhausted; vram_mb_free=$LOW_FREE < floor=$VRAM_MIN_FREE_MB, held across the attempt)"
      else
        fail "assertion 4: got 503 but error.code='$CODE' (want capacity_exhausted): $BODY"
      fi
    else
      fail "assertion 4: expected 503 with vram_mb_free below the floor (slots were free, mutation held across the attempt), got HTTP $ST: $BODY"
    fi

    # ── 5. fail-open ──────────────────────────────────────────────────────
    # Same race, opposite failure mode: if the heartbeat refreshes
    # vram_sampled_at within the staleness window BEFORE assertion 5's own
    # aging UPDATE is even read back, the launch is admitted because the
    # sample is genuinely fresh-and-healthy, NOT because staleness caused an
    # abstain — which would pass for the wrong reason and prove nothing.
    # vram_mb_free is intentionally left at LOW_FREE from assertion 4 (still
    # verified held above) so a fresh sample WOULD veto; only staleness is
    # under test here.
    echo "== 5. assertion: fail-open (stale sample must not veto) =="
    ASSERTION5_HELD=0
    attempt=1
    while [ "$attempt" -le "$VRAM_RACE_MAX_ATTEMPTS" ]; do
      psql_cp "UPDATE gpus SET vram_sampled_at = now() - make_interval(secs => $((VRAM_STALENESS_SECS + 10))), vram_sample_agent_ms = $VRAM_SAMPLE_AGENT_MS_SENTINEL WHERE id='$VETO_TARGET_GPU_ID';" >/dev/null
      PRE_STATE=$(read_vram_state "$VETO_TARGET_GPU_ID" "$VRAM_STALENESS_SECS")
      PRE_FREE="${PRE_STATE%%$'\t'*}"; PRE_STALE="${PRE_STATE#*$'\t'}"
      if [ "$PRE_FREE" != "$LOW_FREE" ] || [ "$PRE_STALE" != "true" ]; then
        echo "  attempt $attempt/$VRAM_RACE_MAX_ATTEMPTS: heartbeat won the race before the launch even fired (free=$PRE_FREE stale=$PRE_STALE) — retrying"
        attempt=$((attempt + 1))
        continue
      fi
      RESULT=$(launch_no_vram_app)
      ST="${RESULT%%$'\t'*}"; BODY="${RESULT#*$'\t'}"
      POST_STATE=$(read_vram_state "$VETO_TARGET_GPU_ID" "$VRAM_STALENESS_SECS")
      POST_FREE="${POST_STATE%%$'\t'*}"; POST_STALE="${POST_STATE#*$'\t'}"
      if [ "$POST_FREE" != "$LOW_FREE" ] || [ "$POST_STALE" != "true" ]; then
        echo "  attempt $attempt/$VRAM_RACE_MAX_ATTEMPTS: heartbeat won the race during the launch (free=$POST_FREE stale=$POST_STALE) — result HTTP $ST is not trustworthy, retrying"
        attempt=$((attempt + 1))
        continue
      fi
      ASSERTION5_HELD=1
      break
    done

    if [ "$ASSERTION5_HELD" != "1" ]; then
      skip "assertion 5 (fail-open): could not hold a stale vram_sampled_at (with vram_mb_free still below the floor) on gpu $VETO_TARGET_GPU_ID across a launch attempt after $VRAM_RACE_MAX_ATTEMPTS attempts — the agent heartbeat kept refreshing it, which would make a PASS here prove nothing (admitted because fresh-and-healthy, not because stale)"
    elif [ "$ST" = "201" ]; then
      pass "assertion 5: fail-open — a stale sample (age > ${VRAM_STALENESS_SECS}s staleness window, held across the attempt) does not veto; launch admitted"
      LAUNCHES_SUCCEEDED=$((LAUNCHES_SUCCEEDED + 1))
      SID=$(printf '%s' "$BODY" | json_get session.id)
      [ -n "$SID" ] && CREATED_SESSION_IDS+=("$SID")
    else
      fail "assertion 5: expected admission on a stale sample (fail-open per spec §2 property 2; mutation held across the attempt), got HTTP $ST: $BODY"
    fi
  fi

  # Only meaningful once assertion 4 actually produced a rejection to
  # diagnose (ASSERTION4_HELD=1, MARK_TS set right before that rejection's
  # launch attempt) — already SKIPped above alongside assertion 4 otherwise.
  if [ "$ASSERTION4_HELD" = "1" ]; then
    echo "== 6. assertion: veto rejection is diagnosable =="
    # The emitted line is Coordinator.logVramVetoRejection (session/launcher.go):
    #   "admission: live free-VRAM veto refused a GPU with free encode slots"
    # carrying gpu_id / vram_mb_free / debit_mb / min_free_mb / vram_sampled_at.
    # Spec §4.4 originally also asked for a session_trace_event; that is not
    # expressible — session_trace_events.session_id is NOT NULL REFERENCES
    # sessions(id) and a rejected launch deliberately persists no session row
    # (assertion 2 above). The structured log carries the whole payload instead.
    if resolve_cp_container; then
      EVIDENCE=$(docker logs "$CP_CONTAINER" --since "$MARK_TS" 2>&1 \
        | grep -F 'live free-VRAM veto refused a GPU' || true)
      if [ -z "$EVIDENCE" ]; then
        fail "assertion 6: no veto log line found in control-plane ($CP_CONTAINER) since $MARK_TS — expected 'live free-VRAM veto refused a GPU with free encode slots' (session/launcher.go logVramVetoRejection)"
      else
        MISSING=""
        for field in gpu_id vram_mb_free debit_mb min_free_mb; do
          printf '%s' "$EVIDENCE" | grep -q "$field" || MISSING="$MISSING $field"
        done
        if [ -z "$MISSING" ]; then
          pass "assertion 6: veto rejection is diagnosable — log line names the GPU and the numbers it judged on"
        else
          fail "assertion 6: veto log line found but missing field(s):$MISSING (spec §4.4 requires the numbers the veto judged on, or a misconfigured floor is an unexplainable 503)"
        fi
      fi
    else
      skip "assertion 6: could not locate the control-plane container (tried deploy-quasar-control-plane-1, then a 'quasar-control-plane' name match) — cannot verify diagnosability"
    fi
  fi
fi

# Cleanly release capacity again before the final no-declared-VRAM check, and
# WAIT for it to actually free rather than sleeping a fixed guess — on a
# small fleet (Tower has 2 usable slots once encoder binding is accounted
# for) a fixed sleep raced the DELETEs and assertion 7 ran against a still-
# full box, which then misreported "declared VRAM still influences placement"
# for what was really ordinary slot exhaustion. Poll admin/sessions until
# every session THIS harness released has reached a terminal state (not in
# admin/sessions' active set at all, or present with a non-active state).
# The veto mutation must also be undone here, not left to the cleanup trap: it
# parks vram_mb_free below the floor on the ONLY schedulable GPU, so assertion 7
# would launch into an actively-vetoed host and read the resulting
# capacity_exhausted as "declared VRAM still binds". (Observed on Tower: 
# assertion 7 skipped as inconclusive for exactly this reason.)
if [ "$VRAM_MUTATED" = "1" ]; then
  restore_vram_snapshot
  VRAM_MUTATED=0
  echo "  veto mutation restored before assertion 7 (the GPU must not still be vetoed)"
fi

echo "== housekeeping: releasing all harness sessions + waiting for capacity to free before assertion 7 =="
RELEASE_IDS=("${CREATED_SESSION_IDS[@]:-}")
for sid in "${RELEASE_IDS[@]:-}"; do
  [ -n "$sid" ] || continue
  curl -sk --connect-timeout 5 --max-time 15 -X DELETE "$API/v1/sessions/$sid" \
    -H "Authorization: Bearer $USER_TOK" >/dev/null 2>&1 || true
done
CREATED_SESSION_IDS=()

CAPACITY_FREED=0
RELEASE_IDS_CSV="$(IFS=,; echo "${RELEASE_IDS[*]:-}")"
if [ -z "${RELEASE_IDS_CSV//,/}" ]; then
  CAPACITY_FREED=1 # nothing was held
else
  WAIT_ATTEMPTS=0
  WAIT_MAX_ATTEMPTS=30
  while [ "$WAIT_ATTEMPTS" -lt "$WAIT_MAX_ATTEMPTS" ]; do
    ACTIVE_LEFT=$(http_body "$(http_json GET admin/sessions "$ADMIN_TOK")" | python3 -c "
import sys, json
d = json.load(sys.stdin)
active = {'pending', 'assigned', 'starting', 'running', 'stopping'}
mine = set('$RELEASE_IDS_CSV'.split(',')) - {''}
left = [s['id'] for s in d.get('items', []) if s.get('id') in mine and s.get('state') in active]
print(len(left))
" 2>/dev/null || echo "")
    if [ "$ACTIVE_LEFT" = "0" ]; then
      CAPACITY_FREED=1
      break
    fi
    WAIT_ATTEMPTS=$((WAIT_ATTEMPTS + 1))
    sleep 1
  done
fi
if [ "$CAPACITY_FREED" = "1" ]; then
  echo "  capacity confirmed free (all harness sessions terminal)"
else
  echo "  WARNING: harness sessions did not all reach a terminal state within ${WAIT_MAX_ATTEMPTS}s — assertion 7 may still see a full box" >&2
fi

# ── 7. No declared-VRAM dependence ───────────────────────────────────────────
# The original form of this assertion demanded default_vram_mb be unset/0, which
# is simply wrong: the column has a NOT NULL DEFAULT 1024 and omitting the field
# on create deliberately falls through to it (cb97bfb — an omitted field used to
# decode as 0 and bypass admission). "Absent" is not observable on read.
#
# The stronger and actually meaningful test: declare an ABSURD VRAM value, far
# above any GPU's total, and prove the app still launches. Under the old rule
# that was an instant no_host_available. If it launches now, placement provably
# does not consult the declared number.
echo "== 7. assertion: an absurd declared default_vram_mb does not block a launch =="
ABSURD_VRAM=999999
PATCH_RAW=$(http_json PATCH "apps/$APP_ID" "$ADMIN_TOK" "{\"default_vram_mb\":$ABSURD_VRAM}")
if [ "$(http_status "$PATCH_RAW")" != "200" ]; then
  fail "assertion 7: could not set default_vram_mb=$ABSURD_VRAM on the test app: $(http_body "$PATCH_RAW")"
else
  READBACK=$(http_body "$(http_json GET "apps/$APP_ID" "$ADMIN_TOK")" | json_get app.default_vram_mb)
  LAUNCH_RAW=$(http_json POST sessions "$USER_TOK" "{\"app_id\":\"$APP_ID\"}")
  LAUNCH_STATUS=$(http_status "$LAUNCH_RAW")
  LAUNCH_BODY=$(http_body "$LAUNCH_RAW")
  if [ "$LAUNCH_STATUS" = "201" ]; then
    SID=$(printf '%s' "$LAUNCH_BODY" | json_get session.id)
    [ -n "$SID" ] && CREATED_SESSION_IDS+=("$SID")
    pass "assertion 7: app declaring ${READBACK}MB VRAM (no GPU has that) launched anyway — declared VRAM no longer binds placement"
  elif [ "$LAUNCH_STATUS" = "503" ] && [ "$(printf '%s' "$LAUNCH_BODY" | json_get error.code)" = "capacity_exhausted" ]; then
    # Distinguish "the fleet's real (encoder-bound) slots were still full" —
    # inconclusive, this proves nothing about whether declared VRAM binds —
    # from any OTHER refusal, which would be the real failure this assertion
    # is checking for.
    skip "assertion 7: inconclusive — refused with 503 capacity_exhausted (slots were still full despite the housekeeping wait above); cannot tell whether the declared ${READBACK}MB VRAM influenced placement or the box was simply out of encode slots: $LAUNCH_BODY"
  else
    fail "assertion 7: app declaring ${READBACK}MB VRAM was refused with HTTP $LAUNCH_STATUS — declared VRAM still influences placement: $LAUNCH_BODY"
  fi
fi

harness_note "launches_succeeded" "$LAUNCHES_SUCCEEDED"
# Falling off the end triggers `cleanup` (EXIT trap), which tears down every
# session/DB mutation this run made and finishes with harness_report.
