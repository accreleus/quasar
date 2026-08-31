#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/qa.sh — `make qa`: validate a CANDIDATE CONTAINER IMAGE against a
# live stack and emit one self-contained HTML report.
# Spec: docs/design/plans/2026-08-13-image-qa-harness-spec.md
#
#   make qa IMAGE=<tag> PROFILE=<name> HOST=<role|host> [APP='Steam'] [RUNS=3]
#           [SKIP_INPUT=keyboard,mouse] [PEER=<role|host>] [KEEP=1]
#           [ARGS='--no-repoint']
#
# What it does, in order (each step is a gate row in the report):
#   0 preflight   stack healthy, candidate image present on the host, dev-agent
#                 auth reachable
#   1 repoint     rewrites the app's runtime_spec.image to the candidate tag,
#                 remembering the previous value. The restore is trapped BEFORE
#                 the mutation: a killed run never leaves the stack on a dev tag.
#   2 launch      RUNS sessions: state=running, fps, steady-state luma, time to
#                 first content
#   3 oracle      the first-screen evidence frame (EVIDENCE, not an assertion —
#                 see the spec; a green report never means someone looked)
#   4 input       per-device stimuli from the profile, measured only after the
#                 content-settle gate
#   5 shutdown    timed `docker stop` of the session container: seconds, exit
#                 code, required/forbidden launcher log lines
#   6 teardown    sessions deleted THROUGH THE API (a bare docker stop leaves
#                 them `failed` in the DB), pin restored, no stray containers
#
# Report: deploy/results/qa-<UTC ts>/{report.html,report.json,shots/,logs/}.
# report.html embeds every screenshot as base64 — it is one uploadable file.
# Exit non-zero if any gate FAILs. EVIDENCE and SKIPPED never pass and never fail.
#
# Requirements on the target stack: QUASAR_DEV_AGENT_AUTH=1 (the per-run
# throwaway identity is minted through it). The peer host needs Chrome-for-Testing
# + playwright — provision with: .claude/skills/quasar-session/scripts/qses provision
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET_NAME=qa

usage() { sed -n '4,30p' "$0" | sed 's/^# \{0,1\}//'; }

# `make qa ARGS='--no-repoint'` delivers ARGS by ENVIRONMENT, not interpolated
# into the recipe line (#550). Every other qa knob (IMAGE, PROFILE, RUNS, …)
# already travelled that way.
[ $# -gt 0 ] || { dx_env_argv "$TARGET_NAME" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

REPOINT=1
while [ $# -gt 0 ]; do
  case "$1" in
    --no-repoint) REPOINT=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET_NAME" "unknown arg '$1' — see: scripts/dx/qa.sh --help" ;;
  esac
done

IMAGE="${IMAGE:-}"
PROFILE_NAME="${PROFILE:-}"
RUNS_OVERRIDE="${RUNS:-}"
APP_OVERRIDE="${APP:-}"
SKIP_INPUT="${SKIP_INPUT:-}"
KEEP="${KEEP:-0}"

[ -n "$IMAGE" ] || dx_guard "$TARGET_NAME" "IMAGE=<tag> is required (the candidate image AS TAGGED ON THE STACK HOST)"
# Spliced into a remote `docker image inspect '$IMAGE'`.
dx_require_safe "$TARGET_NAME" "IMAGE" "$IMAGE" "$DX_RE_IMAGE" "It is a docker image reference."
[ -n "$PROFILE_NAME" ] || dx_guard "$TARGET_NAME" \
  "PROFILE=<name> is required — one of: $(cd "$DX_ROOT/scripts/qa/profiles" && ls ./*.json | sed 's|\./||;s|\.json||' | tr '\n' ' ')"

PROFILE_FILE="$DX_ROOT/scripts/qa/profiles/${PROFILE_NAME}.json"
[ -f "$PROFILE_FILE" ] || dx_guard "$TARGET_NAME" \
  "no profile '$PROFILE_NAME' — have: $(cd "$DX_ROOT/scripts/qa/profiles" && ls ./*.json | sed 's|\./||;s|\.json||' | tr '\n' ' ')"

# qa mutates the target (it repoints an app at a dev image), so it obeys the
# same rule as up/down/restart: HOST must be TYPED, never inherited from
# QUASAR_DEFAULT_HOST.
dx_require_host_scope "$TARGET_NAME"
[ "$DX_HOST" != "local" ] || dx_guard "$TARGET_NAME" \
  "qa validates an image on a real GPU stack; HOST=local has no node agent. Pass HOST=<role|host>."

dx_have python3 || dx_guard "$TARGET_NAME" "python3 is required on this workstation"
dx_have node || dx_guard "$TARGET_NAME" "node is required on this workstation (report assembly)"

# ── host resolution ──────────────────────────────────────────────────────────
# dx_resolve_remote writes globals; snapshot them per role so the stack and the
# browser peer can be different boxes.
resolve_into() { # resolve_into <PREFIX> <role-or-host>
  local prefix="$1" key="$2"
  dx_resolve_remote "$key" || dx_guard "$TARGET_NAME" "'$key' is not a known role or host (see $DX_HOSTS_JSON)"
  eval "${prefix}_NAME=\$DX_REMOTE_NAME
${prefix}_ALIAS=\$DX_REMOTE_SSH_ALIAS
${prefix}_HOST=\$DX_REMOTE_HOST
${prefix}_USER=\$DX_REMOTE_USER
${prefix}_KEY=\$DX_REMOTE_KEY
${prefix}_DIR=\$DX_REMOTE_DIR
${prefix}_API=\$DX_REMOTE_API"
}

ssh_as() { # ssh_as <PREFIX> <cmd...>
  local prefix="$1"; shift
  eval "DX_REMOTE_SSH_ALIAS=\$${prefix}_ALIAS
DX_REMOTE_HOST=\$${prefix}_HOST
DX_REMOTE_USER=\$${prefix}_USER
DX_REMOTE_KEY=\$${prefix}_KEY"
  dx_ssh_remote "$@"
}

# ssh_script <PREFIX> — run the script arriving on THIS function's stdin on the
# remote host. Anything secret (session bearer, signaling token) goes in the
# script BODY, never in argv: an ssh command line is visible in `ps` on both
# ends, a piped script body is not.
ssh_script() {
  local prefix="$1"
  ssh_as "$prefix" "bash -s"
}

resolve_into STACK "$DX_HOST"
PEER_ROLE="${PEER:-$DX_HOST}"
resolve_into PEER "$PEER_ROLE"

[ -n "$STACK_API" ] || dx_guard "$TARGET_NAME" "host '$STACK_NAME' has no api URL in $DX_HOSTS_JSON"

RUN_TS="$(dx_timestamp)"
RESULTS_DIR="$DX_ROOT/deploy/results/qa-${RUN_TS}"
mkdir -p "$RESULTS_DIR/logs" "$RESULTS_DIR/runs"
COMMIT="$(git -C "$DX_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
STARTED_MS="$(python3 -c 'import time;print(int(time.time()*1000))')"

APP_NAME="$APP_OVERRIDE"
[ -n "$APP_NAME" ] || APP_NAME="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("app",""))' "$PROFILE_FILE")"
[ -n "$APP_NAME" ] || dx_guard "$TARGET_NAME" "profile '$PROFILE_NAME' declares no app and APP= was not passed"
SESSION_PROFILE_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("session_profile_id","") or "")' "$PROFILE_FILE")"
N_RUNS="$RUNS_OVERRIDE"
[ -n "$N_RUNS" ] || N_RUNS="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("runs",2))' "$PROFILE_FILE")"
case "$N_RUNS" in ''|*[!0-9]*) dx_guard "$TARGET_NAME" "RUNS must be a positive integer (got '$N_RUNS')" ;; esac
[ "$N_RUNS" -ge 1 ] || dx_guard "$TARGET_NAME" "RUNS must be >= 1"

dx_info "image=$IMAGE profile=$PROFILE_NAME app='$APP_NAME' runs=$N_RUNS stack=$STACK_NAME peer=$PEER_NAME commit=$COMMIT"
dx_info "results: $RESULTS_DIR"

# ── credentials (never echoed) ───────────────────────────────────────────────
# The admin bearer is minted ON the stack host from ITS deploy/.env — those
# credentials never leave that box, and never appear in this script's argv.
admin_exec() { # admin_exec <bash-snippet>   ($API and $ADMIN are in scope there)
  ssh_as STACK "cd '$STACK_DIR' && set -a; . deploy/.env; set +a
API=$STACK_API
ADMIN=\$(curl -k -fs -X POST \$API/v1/auth/login -H 'Content-Type: application/json' \
  -d \"{\\\"email\\\":\\\"\$BOOTSTRAP_ADMIN_EMAIL\\\",\\\"password\\\":\\\"\$BOOTSTRAP_ADMIN_PASSWORD\\\"}\" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"access_token\"])')
[ -n \"\$ADMIN\" ] || { echo 'admin login failed' >&2; exit 1; }
$1"
}

# ── gate 0: preflight ────────────────────────────────────────────────────────
dx_info "=== gate 0: preflight ==="
HEALTH="$(ssh_as STACK "curl -k -fsS -o /dev/null -w '%{http_code}' $STACK_API/health" 2>/dev/null || echo "unreachable")"
[ "$HEALTH" = 200 ] && HEALTH_STATE=ok || HEALTH_STATE="health returned $HEALTH"

IMAGE_DIGEST="$(ssh_as STACK "docker image inspect --format '{{.Id}}' '$IMAGE' 2>/dev/null" || true)"
if [ -n "$IMAGE_DIGEST" ]; then IMAGE_PRESENT=true; else IMAGE_PRESENT=false; fi

CP_CONTAINER="$(ssh_as STACK "docker ps --format '{{.Names}}' | grep -m1 control-plane" 2>/dev/null || true)"
DEV_KEY=""
if [ -n "$CP_CONTAINER" ]; then
  DEV_KEY="$(ssh_as STACK "docker exec $CP_CONTAINER cat /run/quasar/dev-agent-key 2>/dev/null" || true)"
fi
if [ -n "$DEV_KEY" ]; then DEV_AUTH=true; else DEV_AUTH=false; fi

python3 - "$RESULTS_DIR/logs/preflight.json" "$HEALTH_STATE" "$IMAGE_PRESENT" "$DEV_AUTH" <<'PY'
import json,sys
out,health,img,dev = sys.argv[1],sys.argv[2],sys.argv[3]=="true",sys.argv[4]=="true"
json.dump({"health":health,"image_present":img,"dev_agent_auth":dev},open(out,"w"),indent=2)
PY

[ "$HEALTH_STATE" = ok ] || dx_fail preflight "control-plane on $STACK_NAME is not healthy ($HEALTH_STATE)"
[ "$IMAGE_PRESENT" = true ] || dx_fail preflight "image '$IMAGE' is not present on $STACK_NAME (build/copy it there first)"
[ "$DEV_AUTH" = true ] || dx_fail preflight "no dev-agent key on $STACK_NAME — set QUASAR_DEV_AGENT_AUTH=1 there and redeploy"
if [ "$HEALTH_STATE" != ok ] || [ "$IMAGE_PRESENT" != true ] || [ "$DEV_AUTH" != true ]; then
  dx_guard "$TARGET_NAME" "preflight failed — nothing was changed on $STACK_NAME"
fi
dx_pass preflight "healthy · image present ($(printf '%.19s' "$IMAGE_DIGEST")) · dev-agent auth on"

# peer toolchain (Chrome-for-Testing + playwright) — provisioned by qses, not here
if ! ssh_as PEER "test -x /tmp/cft/chrome-linux64/chrome && test -d /tmp/t8-driver/node_modules/playwright-core" 2>/dev/null; then
  dx_guard "$TARGET_NAME" "peer '$PEER_NAME' has no Chrome-for-Testing/playwright — run: .claude/skills/quasar-session/scripts/qses provision"
fi

# ── app lookup ───────────────────────────────────────────────────────────────
admin_exec "curl -k -fs \$API/v1/admin/apps -H \"Authorization: Bearer \$ADMIN\"" > "$RESULTS_DIR/logs/apps.json"

# One pass: resolve the app by name, write its CURRENT runtime_spec verbatim
# (that file is what the restore replays), and print "id<TAB>image".
APP_LINE="$(QA_APP="$APP_NAME" python3 - "$RESULTS_DIR/logs/apps.json" "$RESULTS_DIR/logs/runtime_spec.original.json" <<'PY'
import os, sys, json
doc = json.load(open(sys.argv[1]))
items = doc.get("items", doc if isinstance(doc, list) else [])
want = os.environ["QA_APP"].lower()
for a in items:
    if want in (a.get("name") or "").lower():
        spec = a.get("runtime_spec") or {}
        if isinstance(spec, str):
            spec = json.loads(spec or "{}")
        json.dump(spec, open(sys.argv[2], "w"), indent=2)
        print("%s\t%s" % (a["id"], spec.get("image") or "-"))
        break
PY
)"
[ -n "$APP_LINE" ] || dx_guard "$TARGET_NAME" "no app matching '$APP_NAME' in the catalog on $STACK_NAME"
APP_ID="${APP_LINE%%	*}"
ORIG_IMAGE="${APP_LINE##*	}"
dx_info "app id=$APP_ID current image=${ORIG_IMAGE}"

# ── restore trap — registered BEFORE the first mutation ──────────────────────
PIN_RESTORED=0
SESSIONS_DELETED=0
declare -a LIVE_SIDS=()

patch_runtime_spec() { # patch_runtime_spec <spec-json-file>
  local spec b64
  spec="$(cat "$1")"
  b64="$(printf '%s' "$spec" | python3 -c 'import sys,base64;print(base64.b64encode(sys.stdin.buffer.read()).decode())')"
  admin_exec "SPEC=\$(printf '%s' '$b64' | base64 -d)
BODY=\$(SPEC=\"\$SPEC\" python3 -c 'import os,json;print(json.dumps({\"runtime_spec\":json.loads(os.environ[\"SPEC\"])}))')
curl -k -fs -o /dev/null -w '%{http_code}' -X PATCH \$API/v1/apps/$APP_ID \
  -H \"Authorization: Bearer \$ADMIN\" -H 'Content-Type: application/json' -d \"\$BODY\""
}

delete_session() { # delete_session <sid>  — through the API, never docker stop
  local sid="$1" code
  code="$(admin_exec "curl -k -sS -o /dev/null -w '%{http_code}' -X DELETE \$API/v1/sessions/$sid -H \"Authorization: Bearer \$ADMIN\"")" || return 1
  case "$code" in 200|202|404) SESSIONS_DELETED=$((SESSIONS_DELETED + 1)); return 0 ;; *) return 1 ;; esac
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  if [ "$KEEP" != 1 ]; then
    for sid in ${LIVE_SIDS[@]+"${LIVE_SIDS[@]}"}; do
      [ -n "$sid" ] || continue
      delete_session "$sid" >/dev/null 2>&1 && dx_info "session $sid deleted"
    done
  fi
  if [ "$REPOINT" = 1 ] && [ "$PIN_RESTORED" = 0 ]; then
    dx_info "restoring the app's original runtime_spec"
    if patch_runtime_spec "$RESULTS_DIR/logs/runtime_spec.original.json" >/dev/null 2>&1; then
      PIN_RESTORED=1
      dx_info "runtime_spec restored to image=${ORIG_IMAGE}"
    else
      printf 'FAIL qa — COULD NOT RESTORE the app runtime_spec on %s. It is still pointing at %s.\n' "$STACK_NAME" "$IMAGE" >&2
      printf '     restore by hand from: %s\n' "$RESULTS_DIR/logs/runtime_spec.original.json" >&2
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

# ── gate 1: repoint ──────────────────────────────────────────────────────────
if [ "$REPOINT" = 1 ]; then
  dx_info "=== gate 1: repoint app -> $IMAGE ==="
  python3 - "$RESULTS_DIR/logs/runtime_spec.original.json" "$RESULTS_DIR/logs/runtime_spec.candidate.json" "$IMAGE" <<'PY'
import json,sys
src,dst,image = sys.argv[1],sys.argv[2],sys.argv[3]
spec=json.load(open(src)); spec["image"]=image
json.dump(spec,open(dst,"w"),indent=2)
PY
  CODE="$(patch_runtime_spec "$RESULTS_DIR/logs/runtime_spec.candidate.json")"
  case "$CODE" in 200|204) dx_pass repoint "app runtime_spec.image = $IMAGE (was ${ORIG_IMAGE})" ;;
    *) dx_guard "$TARGET_NAME" "repoint failed: PATCH /v1/apps/$APP_ID returned $CODE" ;;
  esac
else
  dx_info "--no-repoint: the stack is assumed to already serve $IMAGE"
fi

# ── peer-side probe staging ──────────────────────────────────────────────────
# The probe is COPIED to the peer every run and never read from the peer's own
# checkout. A peer running a stale driver is exactly how a healthy stack was
# mis-diagnosed as a black stream for two sessions (2026-08-11).
PROBE_SHA="$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest()[:12])' "$DX_ROOT/scripts/qa/probe.mjs")"
ssh_as PEER "cat > /tmp/qa-probe.mjs" < "$DX_ROOT/scripts/qa/probe.mjs"
dx_info "PROBE sha256=$PROBE_SHA peer=$PEER_NAME (copied, not checked out)"

QA_CONFIG="$(python3 -c 'import json,sys;print(json.dumps(json.load(open(sys.argv[1])).get("gates",{})))' "$PROFILE_FILE")"

# ── gates 2-4: the runs ──────────────────────────────────────────────────────
LAST_SID=""
for i in $(seq 1 "$N_RUNS"); do
  dx_info "=== run $i/$N_RUNS ==="

  # Throwaway per-run identity (#399) — minted on the stack host via the DX verb,
  # so the dev key stays on that box and out of this script's argv.
  CREDS="$(ssh_as STACK "cd '$STACK_DIR' && QUASAR_DEV_AGENT_KEY=\$(docker exec $CP_CONTAINER cat /run/quasar/dev-agent-key) \
    bash scripts/dx/agentcreds.sh --role user --ttl 30m --url $STACK_API")" || \
    dx_guard "$TARGET_NAME" "agent-creds mint failed on $STACK_NAME (needs QUASAR_DEV_AGENT_AUTH=1)"
  TOK="$(printf '%s' "$CREDS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["quasar.auth.token"])')"

  LAUNCH_BODY="$(QA_APP_ID="$APP_ID" QA_PROFILE_ID="$SESSION_PROFILE_ID" python3 -c '
import os,json
d={"app_id":os.environ["QA_APP_ID"]}
if os.environ.get("QA_PROFILE_ID"): d["profile_id"]=os.environ["QA_PROFILE_ID"]
print(json.dumps(d))')"

  LAUNCH="$(printf 'curl -k -s -w "\\n%%{http_code}" -X POST %s/v1/sessions \
  -H "Authorization: Bearer %s" -H "Content-Type: application/json" -d %s\n' \
    "$STACK_API" "$TOK" "'$LAUNCH_BODY'" | ssh_script STACK)"
  CODE="$(printf '%s' "$LAUNCH" | tail -1)"
  BODY="$(printf '%s' "$LAUNCH" | sed '$d')"
  if [ "$CODE" != 201 ]; then
    dx_fail launch "run $i: POST /v1/sessions returned $CODE — $BODY"
    printf '%s' "$BODY" > "$RESULTS_DIR/runs/run-$i.json"
    python3 - "$RESULTS_DIR/runs/run-$i.json" "$i" "$CODE" <<'PY'
import json,sys
json.dump({"run":int(sys.argv[2]),"error":"launch_failed","message":"POST /v1/sessions returned %s"%sys.argv[3]},open(sys.argv[1],"w"))
PY
    continue
  fi

  eval "$(printf '%s' "$BODY" | python3 -c '
import sys,json,shlex
d=json.load(sys.stdin)
print("SID=%s"%shlex.quote(d["session"]["id"]))
print("SIG_URL=%s"%shlex.quote(d["signaling"]["url"]))
print("SIG_TOKEN=%s"%shlex.quote(d["signaling"]["token"]))')"
  LIVE_SIDS+=("$SID")
  LAST_SID="$SID"
  dx_info "run $i: SID=$SID"

  # wait for running
  STATE=""
  for _ in $(seq 1 30); do
    sleep 2
    STATE="$(printf 'curl -k -fs %s/v1/sessions/%s -H "Authorization: Bearer %s" \
  | python3 -c "import sys,json;print(json.load(sys.stdin)[\\"session\\"][\\"state\\"])"\n' \
      "$STACK_API" "$SID" "$TOK" | ssh_script STACK || echo unknown)"
    if [ "$STATE" = running ] || [ "$STATE" = failed ]; then
      break
    fi
  done
  dx_info "run $i: state=$STATE"

  if [ "$STATE" != running ]; then
    python3 - "$RESULTS_DIR/runs/run-$i.json" "$i" "$STATE" <<'PY'
import json,sys
json.dump({"run":int(sys.argv[2]),"error":"never_running","message":"session state=%s"%sys.argv[3]},open(sys.argv[1],"w"))
PY
    dx_fail launch "run $i: session never reached running (state=$STATE)"
    continue
  fi

  # peer-side probe. The env block travels in the piped script body, so the
  # session bearer and signaling token never appear in a remote command line.
  PROBE_SCRIPT="$(python3 - "$STACK_API" "$SID" "$SIG_URL" "$SIG_TOKEN" "$TOK" "$APP_NAME" \
    "$i" "$SKIP_INPUT" "$QA_CONFIG" <<'PY'
import shlex, sys
(api, sid, sig_url, sig_token, tok, app, run, skip, cfg) = sys.argv[1:10]
env = {
    "SPA_URL": api, "SID": sid, "SIG_URL": sig_url, "SIG_TOKEN": sig_token,
    "AUTH_TOKEN": tok, "APP_NAME": app, "QA_RUN_LABEL": run,
    "QA_SKIP_DEVICES": skip, "QA_CONFIG": cfg,
    "CHROME": "/tmp/cft/chrome-linux64/chrome", "PW_DIR": "/tmp/t8-driver",
}
print("\n".join("export %s=%s" % (k, shlex.quote(v)) for k, v in env.items()))
print("node /tmp/qa-probe.mjs")
PY
)"
  set +e
  printf '%s\n' "$PROBE_SCRIPT" | ssh_script PEER \
    > "$RESULTS_DIR/runs/run-$i.json" 2> "$RESULTS_DIR/logs/probe-$i.stderr"
  PROBE_RC=$?
  set -e
  if [ "$PROBE_RC" -ne 0 ]; then
    dx_warn probe "run $i: probe exited $PROBE_RC (see logs/probe-$i.stderr)"
    if [ ! -s "$RESULTS_DIR/runs/run-$i.json" ]; then
      python3 - "$RESULTS_DIR/runs/run-$i.json" "$i" "$PROBE_RC" <<'PY'
import json,sys
json.dump({"run":int(sys.argv[2]),"error":"probe_failed","message":"probe exited %s"%sys.argv[3]},open(sys.argv[1],"w"))
PY
    fi
  else
    dx_info "run $i: $(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
l=d.get("luma") or {}; s=d.get("stats") or {}
print("fps=%s luma=%s first_content=%ss settle=%s"%(s.get("fps"),l.get("mean"),d.get("first_content_s"),d.get("settle")))' "$RESULTS_DIR/runs/run-$i.json")"
  fi

  # ── gate 5: shutdown — on the LAST run, before the session is deleted ──────
  if [ "$i" = "$N_RUNS" ]; then
    dx_info "=== gate 5: clean shutdown ==="
    CONTAINER="$(ssh_as STACK "docker ps --format '{{.Names}}' | grep -m1 -- '${SID:0:8}'" 2>/dev/null || true)"
    if [ -z "$CONTAINER" ]; then
      MATCH="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("session_container_match","") or "")' "$PROFILE_FILE")"
      [ -n "$MATCH" ] && CONTAINER="$(ssh_as STACK "docker ps --format '{{.Names}}' | grep -m1 -- '$MATCH'" 2>/dev/null || true)"
    fi
    if [ -z "$CONTAINER" ]; then
      dx_warn shutdown "could not identify the session container for $SID — gate 5 will be SKIPPED"
    else
      dx_info "stopping container $CONTAINER"
      SHUTDOWN_RAW="$(ssh_as STACK "
START=\$(date +%s.%N)
docker stop '$CONTAINER' >/dev/null 2>&1
END=\$(date +%s.%N)
EXIT=\$(docker inspect -f '{{.State.ExitCode}}' '$CONTAINER' 2>/dev/null || echo unknown)
echo \"stop_seconds=\$(awk -v a=\$START -v b=\$END 'BEGIN{printf \\\"%.2f\\\", b-a}')\"
echo \"exit_code=\$EXIT\"
echo '---LOG---'
docker logs --tail 200 '$CONTAINER' 2>&1 || true")"
      printf '%s\n' "$SHUTDOWN_RAW" > "$RESULTS_DIR/logs/shutdown-raw.txt"
      python3 - "$RESULTS_DIR/logs/shutdown-raw.txt" "$RESULTS_DIR/logs/shutdown.json" "$i" "$CONTAINER" <<'PY'
import json,sys,re
raw=open(sys.argv[1],encoding="utf-8",errors="replace").read()
head,_,log = raw.partition("---LOG---")
def field(name,default=None):
    m=re.search(rf"^{name}=(.*)$",head,re.M)
    return m.group(1).strip() if m else default
try: secs=float(field("stop_seconds","") or "nan")
except ValueError: secs=None
try: code=int(field("exit_code","") or "")
except ValueError: code=None
json.dump({"container":sys.argv[4],
           "attempts":[{"run":int(sys.argv[3]),"stop_seconds":secs,"exit_code":code,"log":log.strip()}]},
          open(sys.argv[2],"w"),indent=2)
PY
      dx_info "shutdown: $(python3 -c '
import json,sys
a=json.load(open(sys.argv[1]))["attempts"][0]
print("stop_seconds=%s exit_code=%s"%(a["stop_seconds"],a["exit_code"]))' "$RESULTS_DIR/logs/shutdown.json")"
    fi
  fi

  # teardown THIS run's session through the API (never a bare docker stop —
  # that is what leaves rows in `failed`)
  if [ "$KEEP" = 1 ] && [ "$i" = "$N_RUNS" ]; then
    dx_info "KEEP=1 — leaving session $SID up"
  else
    delete_session "$SID" && dx_info "run $i: session deleted via API" || dx_warn teardown "run $i: DELETE /v1/sessions/$SID failed"
    # Drop it from the live list so the exit trap does not try again.
    NEXT_SIDS=()
    for s in ${LIVE_SIDS[@]+"${LIVE_SIDS[@]}"}; do
      [ "$s" = "$SID" ] || NEXT_SIDS+=("$s")
    done
    LIVE_SIDS=(${NEXT_SIDS[@]+"${NEXT_SIDS[@]}"})
  fi
done

# ── gate 6: restore + stray check ────────────────────────────────────────────
dx_info "=== gate 6: teardown ==="
if [ "$REPOINT" = 1 ]; then
  if CODE="$(patch_runtime_spec "$RESULTS_DIR/logs/runtime_spec.original.json")" && { [ "$CODE" = 200 ] || [ "$CODE" = 204 ]; }; then
    PIN_RESTORED=1
    dx_pass restore "app runtime_spec restored to image=${ORIG_IMAGE}"
  else
    dx_fail restore "could not restore the original runtime_spec (HTTP $CODE) — see logs/runtime_spec.original.json"
  fi
else
  PIN_RESTORED=1
fi

STRAY=0
if [ -n "${LAST_SID:-}" ] && [ "$KEEP" != 1 ]; then
  STRAY="$(ssh_as STACK "docker ps --format '{{.Names}}' | grep -c -- '${LAST_SID:0:8}'" 2>/dev/null || echo 0)"
fi
[ "$STRAY" = 0 ] || dx_warn teardown "$STRAY stray session container(s) still running on $STACK_NAME"

# ── report ───────────────────────────────────────────────────────────────────
ENDED_MS="$(python3 -c 'import time;print(int(time.time()*1000))')"
RUN_FILES="$(ls "$RESULTS_DIR"/runs/run-*.json 2>/dev/null | tr '\n' ',' | sed 's/,$//')"
[ -n "$RUN_FILES" ] || dx_guard "$TARGET_NAME" "no run artifacts were produced — see $RESULTS_DIR/logs/"

python3 - "$RESULTS_DIR/meta.json" "$IMAGE" "$IMAGE_DIGEST" "$ORIG_IMAGE" "$STACK_NAME" "$STACK_API" \
  "$PROFILE_NAME" "$N_RUNS" "$COMMIT" "$((ENDED_MS - STARTED_MS))" "$SKIP_INPUT" "$REPOINT" \
  "$PIN_RESTORED" "$STRAY" "$SESSIONS_DELETED" <<'PY'
import json, sys, datetime
(out, image, digest, orig_image, host, api, profile, runs, commit,
 duration_ms, skip_input, repointed, pin_restored, stray, deleted) = sys.argv[1:16]
repointed = repointed == "1"
json.dump({
    "image": {"tag": image, "digest": digest, "base": None},
    "replaced_pin": ({"image_ref": orig_image, "restored": pin_restored == "1"}
                     if repointed else None),
    "host": host, "stack_api": api, "profile": profile,
    "runs": int(runs), "commit": commit,
    "generated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "duration_ms": int(duration_ms),
    "skip_input": [s.strip() for s in skip_input.split(",") if s.strip()],
    "repointed": repointed,
    "environment_restored": {
        "pin": pin_restored == "1",
        "stray_containers": int(stray or 0),
        "sessions_deleted": int(deleted),
    },
}, open(out, "w"), indent=2)
PY

SHUTDOWN_ARG=()
[ -f "$RESULTS_DIR/logs/shutdown.json" ] && SHUTDOWN_ARG=(--shutdown "$RESULTS_DIR/logs/shutdown.json")

set +e
node "$DX_ROOT/scripts/qa/assemble.mjs" \
  --out "$RESULTS_DIR" \
  --meta "$RESULTS_DIR/meta.json" \
  --profile "$PROFILE_FILE" \
  --runs "$RUN_FILES" \
  --preflight "$RESULTS_DIR/logs/preflight.json" \
  "${SHUTDOWN_ARG[@]}"
ASSEMBLE_RC=$?
set -e

dx_info "report: $RESULTS_DIR/report.html"
if [ "$ASSEMBLE_RC" -ne 0 ]; then
  dx_fail gates "one or more gates FAILED — see the report"
fi
dx_result "$TARGET_NAME" "image=$IMAGE" "profile=$PROFILE_NAME" "runs=$N_RUNS" "report=$RESULTS_DIR/report.html"
