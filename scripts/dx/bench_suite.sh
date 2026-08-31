#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/bench_suite.sh — the benchmark MATRIX runner: profiles x abr_mode x
# ladder x netem, one quasar-bench run per cell, resumable.
#
#   HOST=devbox scripts/dx/bench_suite.sh --profiles 720p60-h264,1080p60-h264 \
#        --abr-modes off,smooth --ladder off --netem none --secs 240
#   HOST=devbox scripts/dx/bench_suite.sh --matrix docs/.../baseline.json
#   make bench-suite HOST=devbox ARGS='--profiles 1080p60-h264 --dry-run'
#
# Options
#   --matrix FILE        JSON matrix: {"suite","app","secs","iterations",
#                        "profiles":[],"abr_modes":[],"ladder":[],"netem":[]}.
#                        Any flag below overrides the matching file key.
#   --profiles A,B       stream profile ids (required unless --matrix supplies them)
#   --abr-modes A,B      off|protective|smooth        (default: off,smooth)
#   --ladder A,B         off|on                       (default: off)
#   --netem A,B          none|clean|mild|moderate|severe (default: none)
#   --codec C            pin EVERY cell's codec (forwarded to bench_run.sh --codec;
#                        needed because a headless qses launch carries no client
#                        decode probe, so the resolver takes the h264 floor and an
#                        av1-first chain still yields h264 cells without this)
#   --peer local|aux      forwarded to bench_run.sh for every cell (default: local —
#                        see bench_run.sh --help). Refused up front if it is `local`
#                        and any --netem token is not `none`: a local-peer cell
#                        cannot be shaped (the peer never crosses the aux-infra NIC
#                        netem shapes) — use --peer aux for a netem matrix.
#   --app NAME           app to launch in every cell
#   --app-log-glob GLOB  forwarded to bench_run.sh: pull matching app-side files out
#                        of the managed home and attach them to every cell's run
#   --secs N             observation seconds per cell (default 240)
#   --iterations N       repeats per cell (default 1)
#   --suite S            bench suite name (default: baseline)
#   --only SUBSTR        run only cells whose id contains SUBSTR
#   --state FILE         resume file (default <out>/state.tsv). Cells already
#                        recorded `ok` there are SKIPPED, so a killed matrix is
#                        resumed by re-running the identical command line.
#   --out DIR            run root (default .diagnostics/bench-suite/<stamp>)
#   --tag K=V            repeatable; added to every cell
#   --baseline           after the matrix, PUT /v1/baselines for every cell that ran
#                        ok, pinning that cell's run as the baseline for its
#                        suite+scenario (idempotent; re-running re-pins). Default OFF
#                        — pinning is a deliberate act, not a side effect of running
#                        a grid. `GET /v1/regressions?suite=&scenario=&metric=` then
#                        judges every later run against these.
#   --dry-run            print the cell plan and stop
#
# Conditions and mismatches
#   Each cell tags its INTENT (abr_mode, abr_enabled, ladder_resolution, ladder_fps
#   in the same value space the host reports) and bench_run.sh records what the host
#   actually did as the run's `conditions`. quasar-bench compares the two; a cell
#   whose label disagrees with reality comes back with `mismatches`, bench_run.sh
#   exits non-zero, and the cell is recorded `failed mismatch`. A matrix therefore
#   cannot silently produce a grid of mislabelled cells the way the v1 baselines did.
#
# Host settings
#   Each cell PATCHes GET/PATCH /v1/admin/hosts/{id}/settings to `abr_mode` and the
#   two ladder knobs it needs. The host's ORIGINAL overrides are snapshotted before
#   the first cell and restored on EVERY exit path (including Ctrl-C and a failed
#   cell) — the snapshot is also written to <out>/host-settings-before.json so a
#   hard kill can still be undone by hand.
#
# Host scope: mutating remote verb — HOST=<host> must be typed explicitly.
#
# Exit: 0 every cell ok, 1 any cell failed, 2 usage.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=bench-suite

# `make bench-suite ARGS='--profiles ...'` delivers ARGS by ENVIRONMENT, not
# interpolated into the recipe line (#550). Several values parsed below reach a
# remote shell, so every token is shape-checked here too.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

dx_require_host_scope "$TARGET"

usage() { sed -n '3,62p' "$0" | sed 's/^# \{0,1\}//'; }

MATRIX=""
PROFILES=""
ABR_MODES=""
LADDER=""
NETEM=""
CODEC=""
PEER=local
APP=""
APP_LOG_GLOB=""
SECS=""
ITERATIONS=""
SUITE=""
ONLY=""
STATE=""
OUT=""
DRY=0
BASELINE=0
TAGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --matrix)     [ $# -ge 2 ] || dx_guard "$TARGET" "--matrix requires a file"; MATRIX="$2"; shift 2 ;;
    --profiles)   [ $# -ge 2 ] || dx_guard "$TARGET" "--profiles requires a comma list"; PROFILES="$2"; shift 2 ;;
    --abr-modes)  [ $# -ge 2 ] || dx_guard "$TARGET" "--abr-modes requires a comma list"; ABR_MODES="$2"; shift 2 ;;
    --ladder)     [ $# -ge 2 ] || dx_guard "$TARGET" "--ladder requires a comma list"; LADDER="$2"; shift 2 ;;
    --netem)      [ $# -ge 2 ] || dx_guard "$TARGET" "--netem requires a comma list"; NETEM="$2"; shift 2 ;;
    --codec)      [ $# -ge 2 ] || dx_guard "$TARGET" "--codec requires a codec (h264|h265|av1)"; CODEC="$2"; shift 2 ;;
    --peer)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--peer requires local|aux"
      case "$2" in local|aux) ;; *) dx_guard "$TARGET" "--peer must be local|aux (got '$2')" ;; esac
      PEER="$2"; shift 2 ;;
    --app)        [ $# -ge 2 ] || dx_guard "$TARGET" "--app requires a name"; APP="$2"; shift 2 ;;
    --app-log-glob) [ $# -ge 2 ] || dx_guard "$TARGET" "--app-log-glob requires a glob"; APP_LOG_GLOB="$2"; shift 2 ;;
    --secs)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--secs requires an integer"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--secs must be an integer" ;; esac
      SECS="$2"; shift 2 ;;
    --iterations)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--iterations requires an integer"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--iterations must be an integer" ;; esac
      ITERATIONS="$2"; shift 2 ;;
    --suite)      [ $# -ge 2 ] || dx_guard "$TARGET" "--suite requires a name"; SUITE="$2"; shift 2 ;;
    --only)       [ $# -ge 2 ] || dx_guard "$TARGET" "--only requires a substring"; ONLY="$2"; shift 2 ;;
    --state)      [ $# -ge 2 ] || dx_guard "$TARGET" "--state requires a file"; STATE="$2"; shift 2 ;;
    --out)        [ $# -ge 2 ] || dx_guard "$TARGET" "--out requires a directory"; OUT="$2"; shift 2 ;;
    --tag)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--tag requires K=V"
      case "$2" in *=*) ;; *) dx_guard "$TARGET" "--tag must be K=V (got '$2')" ;; esac
      TAGS+=("$2"); shift 2 ;;
    --baseline) BASELINE=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/bench_suite.sh --help" ;;
  esac
done

dx_have python3 || { dx_fail python3 "not on PATH"; dx_result "$TARGET"; }

# ── matrix file (flags win) ──────────────────────────────────────────────────
if [ -n "$MATRIX" ]; then
  [ -f "$MATRIX" ] || dx_guard "$TARGET" "no such matrix file: $MATRIX"
  MVALS="$(python3 - "$MATRIX" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
def csv(key):
    v = m.get(key) or []
    return ",".join(str(x) for x in v)
print(csv("profiles")); print(csv("abr_modes")); print(csv("ladder")); print(csv("netem"))
print(m.get("app", "")); print(m.get("secs", "")); print(m.get("iterations", "")); print(m.get("suite", ""))
PY
)" || dx_guard "$TARGET" "could not parse $MATRIX"
  {
    read -r M_PROFILES; read -r M_ABR; read -r M_LADDER; read -r M_NETEM
    read -r M_APP; read -r M_SECS; read -r M_ITER; read -r M_SUITE
  } <<< "$MVALS"
  [ -n "$PROFILES" ]   || PROFILES="$M_PROFILES"
  [ -n "$ABR_MODES" ]  || ABR_MODES="$M_ABR"
  [ -n "$LADDER" ]     || LADDER="$M_LADDER"
  [ -n "$NETEM" ]      || NETEM="$M_NETEM"
  [ -n "$APP" ]        || APP="$M_APP"
  [ -n "$SECS" ]       || SECS="$M_SECS"
  [ -n "$ITERATIONS" ] || ITERATIONS="$M_ITER"
  [ -n "$SUITE" ]      || SUITE="$M_SUITE"
fi

[ -n "$ABR_MODES" ]  || ABR_MODES="off,smooth"
[ -n "$LADDER" ]     || LADDER="off"
[ -n "$NETEM" ]      || NETEM="none"
[ -n "$SECS" ]       || SECS=240
[ -n "$ITERATIONS" ] || ITERATIONS=1
[ -n "$SUITE" ]      || SUITE=baseline
[ -n "$PROFILES" ]   || dx_guard "$TARGET" "no profiles — pass --profiles or a --matrix that lists them"

split() { local IFS=','; # shellcheck disable=SC2206
  SPLIT=($1); }

split "$PROFILES";  PROFILE_LIST=("${SPLIT[@]}")
split "$ABR_MODES"; ABR_LIST=("${SPLIT[@]}")
split "$LADDER";    LADDER_LIST=("${SPLIT[@]}")
split "$NETEM";     NETEM_LIST=("${SPLIT[@]}")

for m in "${ABR_LIST[@]}"; do
  case "$m" in off|protective|smooth) ;; *) dx_guard "$TARGET" "--abr-modes token '$m' must be off|protective|smooth" ;; esac
done
for l in "${LADDER_LIST[@]}"; do
  case "$l" in off|on) ;; *) dx_guard "$TARGET" "--ladder token '$l' must be off|on" ;; esac
done
for n in "${NETEM_LIST[@]}"; do
  case "$n" in none|clean|mild|moderate|severe) ;; *) dx_guard "$TARGET" "--netem token '$n' must be none|clean|mild|moderate|severe" ;; esac
  # A --peer local cell never crosses the aux-infra NIC netem shapes (see
  # bench_run.sh --help / qnetem sender) — refuse the whole matrix up front
  # rather than let each shaped cell fail one at a time.
  if [ "$PEER" = local ] && [ "$n" != none ]; then
    dx_guard "$TARGET" \
      "--netem $n cell(s) requested with --peer local — a local peer never crosses the aux-infra NIC netem shapes, so the cell would be unshaped data mislabelled netem=$n. Use --peer aux for a netem matrix."
  fi
done

STAMP="$(dx_timestamp)"
[ -n "$OUT" ] || OUT="$DX_DIAG_DIR/bench-suite/$STAMP"
[ -n "$STATE" ] || STATE="$OUT/state.tsv"

# ── the cell plan ────────────────────────────────────────────────────────────
CELLS=()
for p in "${PROFILE_LIST[@]}"; do
  for a in "${ABR_LIST[@]}"; do
    for l in "${LADDER_LIST[@]}"; do
      for n in "${NETEM_LIST[@]}"; do
        for i in $(seq 1 "$ITERATIONS"); do
          id="$p.abr-$a.ladder-$l.netem-$n.i$i"
          case "$id" in *"$ONLY"*) CELLS+=("$id|$p|$a|$l|$n|$i") ;; esac
        done
      done
    done
  done
done

dx_info "matrix: ${#PROFILE_LIST[@]} profile(s) x ${#ABR_LIST[@]} abr_mode(s) x ${#LADDER_LIST[@]} ladder x ${#NETEM_LIST[@]} netem x ${ITERATIONS} iteration(s) = ${#CELLS[@]} cell(s)"
dx_info "suite=$SUITE app=${APP:-<host bench_app>} secs=$SECS host=$DX_HOST peer=$PEER out=$OUT"
# The guard comes BEFORE the listing loop: bash 3.2 treats "${CELLS[@]}" on an
# EMPTY array as an unbound variable under `set -u`, so `--only <no match>` used
# to die with "CELLS[@]: unbound variable" and no RESULT line at all instead of
# the intended refusal.
[ "${#CELLS[@]}" -gt 0 ] || dx_guard "$TARGET" "--only '$ONLY' matched no cell"
for c in "${CELLS[@]}"; do dx_info "  cell ${c%%|*}"; done

if [ "$DRY" = 1 ]; then
  dx_pass plan "printed above; no session launched, no host setting changed, nothing posted"
  dx_result "$TARGET" "cells=${#CELLS[@]}" "suite=$SUITE" "dry_run=1"
fi

[ -n "${QSES_ADMIN_TOKEN:-}" ] || dx_guard "$TARGET" \
  "QSES_ADMIN_TOKEN is unset — mint one (see .claude/skills/quasar-session/SKILL.md) and export it"

mkdir -p "$OUT"
touch "$STATE"

API_BASE="${DX_REMOTE_API:-http://127.0.0.1:$DX_CP_PORT}"

# host_api below splices the token into a REMOTE `curl ... -H 'Authorization:
# Bearer $QSES_ADMIN_TOKEN'`, so a quote in it would execute on the fleet host.
# bench_run.sh has always sanitized here; this path did not.
dx_sanitize_admin_token "$TARGET"

host_api() { # host_api <METHOD> <path> [json-body]
  local method="$1" path="$2" body="${3:-}"
  local args="-fsSk -m 25 -X $method '$API_BASE$path' -H 'Authorization: Bearer $QSES_ADMIN_TOKEN'"
  [ -n "$body" ] && args="$args -H 'Content-Type: application/json' -d '$body'"
  if [ "$DX_HOST" = local ]; then
    eval "curl $args"
  else
    dx_ssh_remote "curl $args"
  fi
}

# One host, or say which. Taking items[0] from a multi-host control plane would
# PATCH (and later "restore") the ABR settings of a host that has nothing to do
# with the run, silently.
HOST_ID="$(host_api GET /v1/hosts | python3 -c '
import sys, json
items = json.load(sys.stdin).get("items") or []
if len(items) != 1:
    sys.stderr.write("GET /v1/hosts returned %d hosts (%s) — this matrix PATCHes host settings and refuses to guess which one\n"
                     % (len(items), ", ".join(h.get("node_name", h["id"]) for h in items)))
    sys.exit(1)
print(items[0]["id"])')" \
  || { dx_fail host "could not resolve exactly one host from GET /v1/hosts"; dx_result "$TARGET"; }
dx_info "host id $HOST_ID"

# ── host-settings API helpers ────────────────────────────────────────────────
# PATCH /v1/admin/hosts/{id}/settings takes {"overrides": {...}} and MERGES it; a
# null value CLEARS a key (control-plane/internal/hostcfg/handler.go `patchReq`).
# A BARE map is accepted with 200 OK and changes NOTHING — which is exactly how
# the first baseline matrix ran every single cell against the host's unchanged
# pre-existing ABR configuration while labelling half of them `abr=off`.
# Verified live on devbox 2026-08-17: bare `{"abr_enabled":false}` -> 200, the
# overrides object unchanged; wrapped -> 200 and the value actually moves.
patch_overrides() { # patch_overrides <json object of overrides>
  local body
  body="$(python3 -c 'import json,sys;print(json.dumps({"overrides": json.loads(sys.argv[1])}))' "$1")" \
    || return 1
  host_api PATCH "/v1/admin/hosts/$HOST_ID/settings" "$body"
}

read_overrides() {
  host_api GET "/v1/admin/hosts/$HOST_ID/settings" \
    | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin).get("overrides") or {}))'
}

# Never trust a 200: read the overrides back and prove every requested key holds
# the requested value, or the cell is mislabelled and must not be benchmarked.
verify_overrides() { # verify_overrides <json object of wanted overrides>
  local now
  now="$(read_overrides 2>/dev/null)" || now='{}'
  python3 -c '
import json, sys
want = json.loads(sys.argv[1])
now = json.loads(sys.argv[2] or "{}")
bad = ["%s is %r, wanted %r" % (k, now.get(k), v) for k, v in want.items() if now.get(k) != v]
if bad:
    sys.stderr.write("; ".join(bad) + "\n")
    sys.exit(1)
' "$1" "$now"
}

# …and the stored override is STILL not proof the stream ran that way. `overrides`
# is what the control plane has recorded; `effective` is what the AGENT reports it
# actually resolved (hostcfg/handler.go: "the agent's last-reported effective
# settings, not this PATCH"). The two can disagree, and did:
#
#   the `off` arm used to PATCH `{"abr_enabled":false}` alone. PATCH MERGES, so on
#   a host carrying a stored `abr_mode: "smooth"` the agent received BOTH keys, and
#   `session/settings.rs::apply_json` applies `abr_enabled` FIRST (-> Off) and the
#   authoritative 3-way `abr_mode` SECOND (-> Smooth). Net effect: overrides read
#   back `abr_enabled=false` exactly as asked, effective read back
#   `abr_mode=smooth, abr_enabled=true`, and the cell labelled `abr=off` ran ABR
#   in smooth mode. Observed live on devbox 2026-08-17 during baseline-v2 cell 1.
#   The arm therefore names `abr_mode` explicitly (see the cell loop) AND is proven
#   against `effective` here.
#
# effective is agent-reported and therefore ASYNCHRONOUS — the config_update has to
# reach the agent and come back — so this polls rather than reading once. Values in
# that map are stringified by the agent (`effective_map`), hence the lowercase
# string compare.
verify_effective() { # verify_effective <json object of wanted overrides>
  local want_eff i now
  want_eff="$(python3 -c '
import json, sys
# the wanted OVERRIDE map, projected into the agent effective map value space
print(json.dumps({k: str(v).lower() for k, v in json.loads(sys.argv[1]).items()}))' "$1")" || return 1
  for i in 1 2 3 4 5 6 7 8 9 10; do
    now="$(host_api GET "/v1/admin/hosts/$HOST_ID/settings" 2>/dev/null \
      | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin).get("effective") or {}))' 2>/dev/null)" || now='{}'
    if python3 -c '
import json, sys
want = json.loads(sys.argv[1]); now = json.loads(sys.argv[2] or "{}")
bad = ["effective.%s is %r, wanted %r" % (k, now.get(k), v) for k, v in want.items() if str(now.get(k)).lower() != v]
if bad:
    sys.stderr.write("; ".join(bad) + "\n")
    sys.exit(1)
' "$want_eff" "$now" 2>/dev/null; then
      dx_info "   effective <- $(python3 -c '
import json,sys
d = json.loads(sys.argv[1]); w = json.loads(sys.argv[2])
print(json.dumps({k: d.get(k) for k in sorted(w)}))' "$now" "$want_eff")"
      return 0
    fi
    sleep 3
  done
  python3 -c '
import json, sys
want = json.loads(sys.argv[1]); now = json.loads(sys.argv[2] or "{}")
sys.stderr.write("; ".join("effective.%s is %r, wanted %r" % (k, now.get(k), v)
                           for k, v in want.items() if str(now.get(k)).lower() != v) + "\n")' \
    "$want_eff" "$now" >&2
  return 1
}

# ── snapshot + restore the host's ABR overrides ──────────────────────────────
# The snapshot is the `overrides` object verbatim, so restoring is a single PATCH
# of exactly what was there — including keys this script never touches.
BEFORE_JSON="$OUT/host-settings-before.json"
host_api GET "/v1/admin/hosts/$HOST_ID/settings" > "$OUT/host-settings-full.json" \
  || { dx_fail settings "could not read the host settings"; dx_result "$TARGET"; }
python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
json.dump(d.get("overrides") or {}, open(sys.argv[2], "w"), indent=2, sort_keys=True)
' "$OUT/host-settings-full.json" "$BEFORE_JSON"
dx_info "host overrides snapshotted to $BEFORE_JSON:"
dx_info "  $(tr -d '\n ' < "$BEFORE_JSON")"

RESTORED=0
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
restore_settings() {
  [ "$RESTORED" = 1 ] && return 0
  RESTORED=1
  local now body
  now="$(read_overrides 2>/dev/null)" || now='{}'
  # A PATCH MERGES, so re-sending the snapshot alone would leave behind every key
  # a CELL added that the snapshot never had (the common case: a host with no ABR
  # overrides at all would keep the last cell's arm forever, "restored"). Send the
  # snapshot PLUS an explicit null for each key present now but not before — that
  # is a true "set the overrides back to exactly what they were".
  body="$(python3 -c '
import json, sys
before = json.load(open(sys.argv[1]))
now = json.loads(sys.argv[2] or "{}")
ov = dict(before)
for k in now:
    if k not in before:
        ov[k] = None
print(json.dumps({"overrides": ov}))
' "$BEFORE_JSON" "$now")"
  if host_api PATCH "/v1/admin/hosts/$HOST_ID/settings" "$body" >/dev/null 2>&1 \
     && verify_overrides "$(cat "$BEFORE_JSON")" 2>/dev/null; then
    dx_info "host overrides RESTORED from $BEFORE_JSON (verified by read-back)"
  else
    dx_fail restore "could NOT restore the host overrides — re-apply $BEFORE_JSON by hand:"
    dx_info "  curl -X PATCH $API_BASE/v1/admin/hosts/$HOST_ID/settings -d @$BEFORE_JSON"
  fi
}
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
cleanup() { local rc=$?; trap - EXIT INT TERM; restore_settings; exit "$rc"; }
trap cleanup EXIT INT TERM

# ── run the cells ────────────────────────────────────────────────────────────
OK_N=0
SKIP_N=0
BAD_N=0

for c in "${CELLS[@]}"; do
  OLDIFS="$IFS"; IFS='|'; read -r id prof abr lad net iter <<< "$c"; IFS="$OLDIFS"

  if grep -qF "$(printf '%s\tok' "$id")" "$STATE" 2>/dev/null; then
    dx_info "cell $id — already ok in $STATE, skipping"
    SKIP_N=$((SKIP_N + 1))
    continue
  fi

  # BOTH keys, always. `abr_mode` is the authoritative 3-way enum the agent
  # applies LAST; naming only the deprecated `abr_enabled` bool lets a stored
  # `abr_mode` override (merged in by the PATCH) win and silently re-enable ABR in
  # a cell labelled `off` — see verify_effective above.
  case "$abr" in
    off) SET='{"abr_enabled":false,"abr_mode":"off"}' ;;
    *)   SET="{\"abr_enabled\":true,\"abr_mode\":\"$abr\"}" ;;
  esac
  case "$lad" in
    on)  SET="${SET%\}},\"abr_ladder\":true,\"abr_ladder_resolution\":true,\"abr_ladder_fps\":true}" ;;
    off) SET="${SET%\}},\"abr_ladder\":false,\"abr_ladder_resolution\":false,\"abr_ladder_fps\":false}" ;;
  esac

  printf '\n'
  dx_info "── cell $id"
  dx_info "   host settings <- $SET"
  if ! patch_overrides "$SET" >/dev/null; then
    dx_fail "$id" "could not PATCH the host settings"
    printf '%s\tfailed\tsettings\n' "$id" >> "$STATE"
    BAD_N=$((BAD_N + 1))
    continue
  fi
  if ! verify_overrides "$SET"; then
    dx_fail "$id" "the host settings did NOT take — the PATCH returned OK but the read-back disagrees; refusing to run a cell whose arm label would be a lie"
    printf '%s\tfailed\tsettings-readback\n' "$id" >> "$STATE"
    BAD_N=$((BAD_N + 1))
    continue
  fi
  if ! verify_effective "$SET"; then
    dx_fail "$id" "the AGENT never reported the requested settings as effective — the override is stored but the stream would not run under it; refusing to run a mislabelled cell"
    printf '%s\tfailed\tsettings-effective\n' "$id" >> "$STATE"
    BAD_N=$((BAD_N + 1))
    continue
  fi

  SCEN="$prof-abr-$abr-ladder-$lad-netem-$net"
  case "$abr" in off) ABR_ENABLED=off ;; *) ABR_ENABLED=on ;; esac
  RUN_ARGS=(--profile "$prof" --secs "$SECS" --suite "$SUITE"
            --scenario "$SCEN"
            --out "$OUT/$id"
            --tag "cell=$id" --tag "iteration=$iter" --tag "ladder=$lad"
            --tag "abr=$abr"
            # The cell's INTENT, in the value space the host reports its
            # effective settings in — this is the half of the tags-vs-conditions
            # comparison quasar-bench checks. Passing them explicitly also stops
            # bench_run.sh's auto-derivation (which reads the same host settings
            # the conditions come from) from making both sides agree by
            # construction and the check vacuous.
            --tag "abr_mode=$abr" --tag "abr_enabled=$ABR_ENABLED"
            --tag "ladder_resolution=$lad" --tag "ladder_fps=$lad")
  # `abr` is the CELL's arm and it is load-bearing: the `off` arm is
  # `abr_enabled=false`, which leaves the host's `abr_mode` still reading
  # `smooth`, so the auto-derived `abr_mode` tag cannot tell the two arms apart
  # on its own. (Both arms coming back tagged abr_mode=smooth was ALSO the first
  # visible symptom of the bare-map PATCH being a no-op — see patch_overrides
  # above. The tag is still correct and still needed; the no-op was the bug.)
  [ -n "$APP" ] && RUN_ARGS+=(--app "$APP")
  [ -n "$APP_LOG_GLOB" ] && RUN_ARGS+=(--app-log-glob "$APP_LOG_GLOB")
  [ "$net" != none ] && RUN_ARGS+=(--netem "$net")
  [ -n "$CODEC" ] && RUN_ARGS+=(--codec "$CODEC" --tag "codec=$CODEC")
RUN_ARGS+=(--peer "$PEER")
  for t in ${TAGS[@]+"${TAGS[@]}"}; do RUN_ARGS+=(--tag "$t"); done

  # tee'd, not swallowed: the run id and any mismatch verdict are in bench_run.sh's
  # own output, and --baseline needs the id of the run each cell produced.
  CELL_LOG="$OUT/$id.log"
  RUN_RC=0
  set +e
  HOST="$DX_HOST" bash "$DX_DIR/bench_run.sh" "${RUN_ARGS[@]}" 2>&1 | tee "$CELL_LOG"
  RUN_RC=${PIPESTATUS[0]}
  set -e
  RUN_ID="$(sed -n 's/.*target=bench-submit .*run_id=\([0-9A-Za-z-]*\).*/\1/p' "$CELL_LOG" 2>/dev/null | tail -1)"

  if [ "$RUN_RC" = 0 ]; then
    dx_pass "$id" "submitted${RUN_ID:+ as $RUN_ID}"
    printf '%s\tok\t%s\t%s\n' "$id" "$SCEN" "${RUN_ID:-}" >> "$STATE"
    OK_N=$((OK_N + 1))
  elif grep -q 'FAIL mismatch' "$CELL_LOG" 2>/dev/null; then
    dx_fail "$id" "MISLABELLED — the run is posted (tagged mismatch=1) but a tag disagrees with what the host reported; the arm label would be a lie, see $CELL_LOG"
    printf '%s\tfailed\tmismatch\t%s\n' "$id" "${RUN_ID:-}" >> "$STATE"
    BAD_N=$((BAD_N + 1))
  else
    dx_fail "$id" "bench_run.sh failed — see $OUT/$id"
    printf '%s\tfailed\trun\n' "$id" >> "$STATE"
    BAD_N=$((BAD_N + 1))
  fi
done

restore_settings

# ── optional: pin each ok cell's run as its suite+scenario baseline ──────────
# Deliberately opt-in and deliberately LAST: a baseline is the thing every later
# run is judged against, so it is pinned only from a matrix that finished, and
# only from cells that came back ok (a mismatched or failed cell is exactly what
# must never become the reference). PUT is idempotent — re-running re-pins.
PIN_N=0
if [ "$BASELINE" = 1 ]; then
  printf '\n'
  dx_info "── pinning baselines (PUT /v1/baselines) for suite $SUITE"
  if [ -z "${BENCH_URL:-}" ] || [ -z "${BENCH_KEY:-}" ]; then
    dx_fail baseline "--baseline needs BENCH_URL and BENCH_KEY exported"
  else
    while IFS=$'\t' read -r b_id b_state b_scen b_run; do
      [ "$b_state" = ok ] || continue
      [ -n "$b_scen" ] && [ -n "$b_run" ] || {
        dx_warn baseline "cell $b_id has no recorded scenario/run id (an older state file) — skipped"
        continue
      }
      if python3 "$DX_DIR/vendor/bench.py" baseline "$b_run" \
           --suite "$SUITE" --scenario "$b_scen" >/dev/null; then
        dx_info "   $b_scen <- $b_run"
        PIN_N=$((PIN_N + 1))
      else
        dx_fail baseline "could not pin $b_scen -> $b_run"
      fi
    done < "$STATE"
    [ "$PIN_N" -gt 0 ] && dx_pass baseline "$PIN_N scenario baseline(s) pinned" || true
  fi
fi

dx_result "$TARGET" "out=$OUT" "cells=${#CELLS[@]}" "ok=$OK_N" "skipped=$SKIP_N" "failed=$BAD_N" "baselines=$PIN_N"
