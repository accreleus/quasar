#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/bench_run.sh — ONE live benchmark iteration, submitted to quasar-bench.
#
#   HOST=devbox scripts/dx/bench_run.sh --app 'KDE Desktop' --profile 1080p60 --secs 240
#   make bench-run HOST=devbox ARGS="--app 'KDE Desktop' --profile 1080p120 --netem moderate"
#
# One iteration is: self-launch a session at a PINNED stream profile with a real
# headless WebRTC peer attached, observe it for --secs (optionally under netem),
# then submit the whole telemetry directory to quasar-bench as one run.
#
# Options
#   --app NAME           app to launch (default: the host's bench_app)
#   --profile ID         LAUNCH profile id — PINNED, e.g. 1080p120 (GET /v1/me/profiles
#                        or /v1/admin/launch-profiles). NOT a stream-profile RUNG id
#                        like `1080p120-h264`: POST /v1/sessions rejects a rung id
#                        with `unknown profile_id`. Required — an unpinned launch is
#                        not a benchmark cell, it is whatever the eligibility
#                        resolver happened to pick. The rung the session actually
#                        lands on is recorded in the run's own tags (codec + launch
#                        WxH from session.json), which is how a 1440p/4K launch
#                        resolving DOWN to the h264 floor stays visible.
#   --secs N             observation seconds (default 240). With --netem this is the
#                        per-level dwell instead; the total run is longer (see below).
#   --netem LEVEL        clean|mild|moderate|severe. Delegates the whole
#                        shaping + observe sequence to scripts/dx/abr_ladder_netem.sh
#                        (baseline hold + level + recovery hold) rather than
#                        re-implementing qnetem sequencing here. Incompatible
#                        with --peer local (see --peer).
#   --peer local|aux     where the headless WebRTC peer runs. `local` (DEFAULT)
#                        puts it on the STACK host itself: QSES_PEER_ROLE is set
#                        to the same role/host this run resolved for HOST, so
#                        qses's own "peer==stack -> use the local API URL" path
#                        (.claude/skills/quasar-session/scripts/qses) takes over.
#                        `aux` restores the old default, QSES_PEER_ROLE=aux-infra
#                        (today's hermes-over-WiFi peer). Changed 2026-08-19 on
#                        the peer-path finding (docs/reports/2026-08-19-peer-path/
#                        REPORT.md): a local peer measured 0.000% drops vs 2.7%
#                        through hermes, on an otherwise identical cell — every
#                        pre-2026-08-19 browser-side drop number was measuring the
#                        peer's network/CPU as much as Quasar's own pipeline.
#                        `--peer aux` is required with --netem: netem shapes the
#                        SENDER's egress toward the aux-infra NIC (qnetem sender,
#                        see abr_ladder_netem.sh), which media to a local peer
#                        never crosses — a local-peer netem cell would silently
#                        submit unshaped data under an `impaired` label, so this
#                        combination is refused instead.
#   --suite S            bench suite (default: `baseline`, or `abr-ladder` with --netem)
#   --scenario S         bench scenario (default: derived, e.g. 1080p120-h264-clean)
#   --tag K=V            repeatable; passed through to bench_submit.py (wins over
#                        every auto-derived tag)
#   --playout MS         pin the receiver playout target (?playout=MS on the peer's
#                        session URL), which ALSO switches off the AS-05 adaptive
#                        controller. Recorded as the tag `playout=MS`; when omitted
#                        the tag is `playout=auto` (the controller ran).
#   --peer-unlock-fps    force ON the CFT Chrome `--disable-frame-rate-limit
#                        --disable-gpu-vsync` launch flags (also: env
#                        QSES_PEER_UNLOCK_FPS=1, honoured by qses directly).
#                        Headless Chrome's RVFC present loop is capped at 60fps
#                        regardless of decoded content rate — a clean 1080p120
#                        h264 run measured present_fps pinned at 60.00 with ZERO
#                        frames_dropped/packets_lost/freeze_count (overnight-2 §C,
#                        docs/reports/2026-08-19-overnight-2/README.md); the
#                        marker instrument's ~50% "missing index" reading on that
#                        run is the ceiling, not real loss. AUTO-ON when --profile's
#                        trailing `pNNN` exceeds 60 (e.g. 1080p120), auto-OFF
#                        otherwise — a 60fps baseline's Chrome launch is byte-
#                        identical to every prior report unless this flag is
#                        given explicitly. Always tagged `peer_unlock_fps=0|1`.
#   --app-log-glob GLOB  pull matching files out of the session's managed home on the
#                        host (<home_root>/<...>) into <out>/app/ and upload them as
#                        artifacts — e.g. an in-app benchmark CSV.
#   --no-probe           with --bench-mode, do NOT arm the host-side
#                        `latency_probe` hostcfg override (no effect without
#                        --bench-mode — the probe is never armed without it).
#                        DEFAULT with --bench-mode is to arm it: this script
#                        PATCHes `overrides.latency_probe=true` on the session's
#                        host, verifies the read-back, tags the run
#                        `latency_probe=on`, and restores the host's prior value
#                        on every exit path (including Ctrl-C) — same
#                        snapshot/PATCH/verify/restore-in-trap pattern as
#                        bench_suite.sh's ABR settings. The probe's own
#                        perturbation was measured and found nil:
#                        docs/reports/2026-08-19-fps120-probe/REPORT.md part 2
#                        (drops Δ +0.143pp, g2g Δ +2.10ms, both inside the
#                        randomised-rerun PASS thresholds) — so this is now a
#                        standing part of every bench-mode run rather than an
#                        opt-in.
#   --out DIR            output directory (default .diagnostics/bench/<stamp>)
#   --settle N           seconds to wait for the peer to attach before observing (60)
#   --keep               leave the session running afterwards (default: stop it)
#   --no-submit          run and write the directory, but do not POST anything
#   --dry-run            print the plan and stop — nothing launched, shaped or posted
#
# Auto-derived tags: the profile id, the app, the netem level, and the HOST's live
# ABR configuration read back from GET /v1/admin/hosts/{id}/settings (abr_mode,
# abr_enabled, abr_ladder_resolution, abr_ladder_fps, abr_ladder_order, encoder,
# floor_follows_rung) — so a run always records the knobs it actually ran under
# rather than the knobs the operator believed were set.
#
# Conditions: the SAME settings read is written verbatim to <out>/conditions.json as
# `effective` (raw keys plus the tag-space aliases), together with the netem level
# and the number of OTHER non-terminal sessions on the host at launch. bench_submit.py
# posts it as the run's `conditions`, and quasar-bench compares every effective key
# against the same-named TAG — so a cell whose `--tag abr_mode=off` (the intent) does
# not match what the host reported (the reality) comes back with `mismatches`, this
# script FAILS, and the matrix stops instead of producing eleven more mislabelled
# cells. That is the automated form of the defect that invalidated the v1 baselines.
#
# Host scope: this LAUNCHES a session on a remote host (and with --netem shapes its
# egress), so it is a mutating remote verb — HOST=<host> must be typed explicitly.
#
# Cleanup: the session this script launched is stopped on EVERY exit path including
# Ctrl-C (unless --keep); --netem delegates to abr_ladder_netem.sh, whose own trap
# always clears the shaping.
#
# Needs BENCH_URL + BENCH_KEY (unless --no-submit), and QSES_ADMIN_TOKEN /
# QSES_DEV_KEY exactly as the quasar-session skill documents.
#
# Exit: 0 ok, 1 failed, 2 usage.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=bench-run

# `make bench-run ARGS="--profile 1080p60-h264 --secs 240"` delivers ARGS by
# ENVIRONMENT, not interpolated into the recipe line (#550). Several values
# parsed below reach a remote shell, so every token is shape-checked here too.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

dx_require_host_scope "$TARGET"

usage() { sed -n '3,91p' "$0" | sed 's/^# \{0,1\}//'; }

QSES="$DX_ROOT/.claude/skills/quasar-session/scripts/qses"

APP=""
PROFILE=""
SECS=240
NETEM=""
SUITE=""
SCENARIO=""
OUT=""
SETTLE=60
KEEP=0
SUBMIT=1
DRY=0
TAGS=()
APP_LOG_GLOB=""
CODEC=""
BENCH_MODE=0
PULSE_EVERY=""
PLAYOUT=""
PEER=local
PEER_UNLOCK_FPS=""
PROBE=1

while [ $# -gt 0 ]; do
  case "$1" in
    --app)      [ $# -ge 2 ] || dx_guard "$TARGET" "--app requires a name"; APP="$2"; shift 2 ;;
    --profile)  [ $# -ge 2 ] || dx_guard "$TARGET" "--profile requires an id"; PROFILE="$2"; shift 2 ;;
    --secs)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--secs requires an integer"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--secs must be an integer (got '$2')" ;; esac
      SECS="$2"; shift 2 ;;
    --netem)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--netem requires clean|mild|moderate|severe"
      case "$2" in clean|mild|moderate|severe) ;; *) dx_guard "$TARGET" "--netem must be clean|mild|moderate|severe (got '$2')" ;; esac
      NETEM="$2"; shift 2 ;;
    --suite)    [ $# -ge 2 ] || dx_guard "$TARGET" "--suite requires a name"; SUITE="$2"; shift 2 ;;
    --scenario) [ $# -ge 2 ] || dx_guard "$TARGET" "--scenario requires a name"; SCENARIO="$2"; shift 2 ;;
    --tag)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--tag requires K=V"
      case "$2" in *=*) ;; *) dx_guard "$TARGET" "--tag must be K=V (got '$2')" ;; esac
      TAGS+=("$2"); shift 2 ;;
    --app-log-glob)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--app-log-glob requires a glob"
      # Spliced into a remote `find . -path './$APP_LOG_GLOB'`. `*` and `?` are
      # the point of a glob, so they stay; quotes and `$` do not.
      dx_require_safe "$TARGET" "--app-log-glob" "$2" '^[A-Za-z0-9._/*?[-]+$' \
        "It is a path glob, not a shell expression."
      APP_LOG_GLOB="$2"; shift 2 ;;
    --codec)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--codec requires h264|h265|av1|auto"
      case "$2" in h264|h265|av1|auto) CODEC="$2" ;;
        *) dx_guard "$TARGET" "--codec must be one of h264 h265 av1 auto" ;; esac
      shift 2 ;;
    --bench-mode) BENCH_MODE=1; shift ;;
    --no-probe) PROBE=0; shift ;;
    --playout)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--playout requires milliseconds"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--playout must be a non-negative integer (ms)" ;; esac
      PLAYOUT="$2"; shift 2 ;;
    --input-pulse-every)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--input-pulse-every requires an integer (seconds)"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--input-pulse-every must be an integer (seconds)" ;; esac
      [ "$2" -ge 1 ] || dx_guard "$TARGET" "--input-pulse-every must be at least 1 second"
      PULSE_EVERY="$2"; shift 2 ;;
    --peer)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--peer requires local|aux"
      case "$2" in local|aux) ;; *) dx_guard "$TARGET" "--peer must be local|aux (got '$2')" ;; esac
      PEER="$2"; shift 2 ;;
    --peer-unlock-fps) PEER_UNLOCK_FPS=1; shift ;;
    --out)      [ $# -ge 2 ] || dx_guard "$TARGET" "--out requires a directory"; OUT="$2"; shift 2 ;;
    --settle)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--settle requires seconds"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--settle must be an integer" ;; esac
      SETTLE="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    --no-submit) SUBMIT=0; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/bench_run.sh --help" ;;
  esac
done

[ -n "$PROFILE" ] || dx_guard "$TARGET" \
  "--profile is required: an unpinned launch is not a benchmark cell (list LAUNCH profile ids with GET /v1/admin/launch-profiles — a stream-profile rung id like 1080p120-h264 is rejected; use --profile forced for an app whose row sets profile_policy=force, which rejects profile_id outright)"
[ -f "$QSES" ] || { dx_fail qses "$QSES missing"; dx_result "$TARGET"; }
# A pulse with no in-page instrument to answer it produces nothing; fail loudly
# rather than submit a cell whose i2p column is silently empty.
if [ -n "$PULSE_EVERY" ] && [ "$BENCH_MODE" != 1 ]; then
  dx_guard "$TARGET" "--input-pulse-every requires --bench-mode"
fi
dx_have python3 || { dx_fail python3 "not on PATH"; dx_result "$TARGET"; }

# BENCH_KEY name:secret tolerance (2026-08-19 overnight-2 harness note #2):
# deploy/.env's BENCH_API_KEYS stores `name:secret` (e.g. `harness:abc123...`);
# the server wants the bare secret only. Normalize here too — a run's whole
# session is expensive, and this script does not call bench_submit.py until
# the very end, so failing fast on a pasted-straight-from-.env key beats
# discovering the 401 only after the observation window already ran. (Backed
# up by the same tolerance inside bench_submit.py, for direct invocations.)
if [ "$SUBMIT" = 1 ] && [ -n "${BENCH_KEY:-}" ] && [ "${BENCH_KEY#*:}" != "$BENCH_KEY" ]; then
  dx_warn bench-key "BENCH_KEY looks like 'name:secret' (name=${BENCH_KEY%%:*}) — using only the part after the colon"
  BENCH_KEY="${BENCH_KEY#*:}"
  export BENCH_KEY
fi

# --netem shapes the SENDER's egress toward the aux-infra role's NIC (qnetem
# sender — abr_ladder_netem.sh / .claude/skills/quasar-netem/scripts/qnetem
# sender_nic(), which routes via `q_host_cfg "$IMPAIR_ROLE" ip`). A --peer local
# run never sends media across that NIC at all — refuse rather than silently
# submit an unshaped run mislabelled netem=$NETEM.
if [ -n "$NETEM" ] && [ "$PEER" = local ]; then
  dx_guard "$TARGET" \
    "--netem $NETEM shapes the sender's egress toward the aux-infra NIC, which a --peer local run never crosses (the peer is on the stack host itself) — the cell would be unshaped data mislabelled netem=$NETEM. Use --peer aux for a netem cell."
fi

# The peer role qses resolves. `local` reuses whatever HOST resolved to for
# this run (so qses's own peer==stack -> local-API path takes over, see
# .claude/skills/quasar-session/scripts/qses); `aux` is the pre-2026-08-19
# default. --peer always wins over any QSES_PEER_ROLE already in the
# environment — that is the whole point of the flag.
case "$PEER" in
  local) QSES_PEER_ROLE="$DX_HOST" ;;
  aux)   QSES_PEER_ROLE="aux-infra" ;;
esac
export QSES_PEER_ROLE

# Resolve the peer ROLE to a canonical host NAME for tagging, via the same
# DX_HOSTS_JSON qses itself reads (roles{} then a literal host name) — so old,
# untagged runs (implicitly hermes) and new ones stay distinguishable by the
# `peer=<host>` tag rather than by role, which can rename hosts underneath it.
# `local` (the DX_HOST sentinel, not a hosts.json entry) resolves to itself.
dx_peer_host_name() { # dx_peer_host_name <role-or-host>
  local key="$1"
  if [ "$key" = local ]; then printf 'local\n'; return 0; fi
  if [ -f "$DX_HOSTS_JSON" ] && dx_have python3; then
    python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(sys.argv[2]); sys.exit(0)
key = sys.argv[2]
print(d.get("roles", {}).get(key, key))
' "$DX_HOSTS_JSON" "$key"
  else
    printf '%s\n' "$key"
  fi
}
PEER_HOST="$(dx_peer_host_name "$QSES_PEER_ROLE")"

# peer-unlock-fps resolution order: explicit --peer-unlock-fps flag, then
# QSES_PEER_UNLOCK_FPS=1 in the environment (qses honours this directly too, so
# a bare `qses run` outside this script picks it up the same way), then AUTO
# from the profile id's trailing `pNNN` fps — ON only when that fps is > 60.
# `--profile forced` (no fps in the id at all) and any profile whose fps cannot
# be parsed resolve to OFF, matching a normal 60fps (or fps-less) baseline.
if [ -z "$PEER_UNLOCK_FPS" ] && [ "${QSES_PEER_UNLOCK_FPS:-0}" = 1 ]; then
  PEER_UNLOCK_FPS=1
fi
if [ -z "$PEER_UNLOCK_FPS" ]; then
  PROFILE_FPS=""
  if [[ "$PROFILE" =~ p([0-9]+)($|-) ]]; then
    PROFILE_FPS="${BASH_REMATCH[1]}"
  fi
  if [ -n "$PROFILE_FPS" ] && [ "$PROFILE_FPS" -gt 60 ] 2>/dev/null; then
    PEER_UNLOCK_FPS=1
  else
    PEER_UNLOCK_FPS=0
  fi
fi

[ -n "$SUITE" ] || { if [ -n "$NETEM" ]; then SUITE=abr-ladder; else SUITE=baseline; fi; }
[ -n "$SCENARIO" ] || SCENARIO="$PROFILE-${NETEM:-clean}"

# --netem delegates to abr_ladder_netem.sh, which spends BASELINE(30) + dwell + RECOVERY(120)
# on top of the level itself. Keep the peer alive for all of it.
if [ -n "$NETEM" ]; then
  OBSERVE_S=$((30 + SECS + 120))
else
  OBSERVE_S="$SECS"
fi
PEER_S=$((SETTLE + OBSERVE_S + 60))

STAMP="$(dx_timestamp)"
[ -n "$OUT" ] || OUT="$DX_DIAG_DIR/bench/${SCENARIO}-${STAMP}"

dx_info "plan"
dx_info "  host      $DX_HOST"
dx_info "  app       ${APP:-<host bench_app>}"
dx_info "  profile   $PROFILE  (pinned)"
dx_info "  suite     $SUITE"
dx_info "  scenario  $SCENARIO"
dx_info "  netem     ${NETEM:-none}"
dx_info "  peer      $PEER (QSES_PEER_ROLE=$QSES_PEER_ROLE, peer_host=$PEER_HOST)"
dx_info "  peer_unlock_fps $PEER_UNLOCK_FPS"
dx_info "  codec     ${CODEC:-<profile order decides>}"
dx_info "  bench     $([ "$BENCH_MODE" = 1 ] && echo "on (?bench=1)${PULSE_EVERY:+, pulse every ${PULSE_EVERY}s}" || echo off)"
dx_info "  probe     $([ "$BENCH_MODE" = 1 ] && { [ "$PROBE" = 1 ] && echo "on (latency_probe armed)" || echo "off (--no-probe)"; } || echo "n/a (no --bench-mode)")"
dx_info "  observe   ${OBSERVE_S}s (peer held ${PEER_S}s, settle ${SETTLE}s)"
dx_info "  out       $OUT"
dx_info "  submit    $([ "$SUBMIT" = 1 ] && echo yes || echo no)"

if [ "$DRY" = 1 ]; then
  dx_info "  would run: $QSES run --stack $DX_HOST${APP:+ --app '$APP'}$([ "$PROFILE" = forced ] || echo " --profile $PROFILE") --secs $PEER_S --keep"
  if [ -n "$NETEM" ]; then
    dx_info "  would run: HOST=$DX_HOST $DX_DIR/abr_ladder_netem.sh <SID> --levels $NETEM --dwell $SECS --out $OUT"
  else
    dx_info "  would run: HOST=$DX_HOST $DX_DIR/session_soak.sh <SID> --profile observe --duration $OBSERVE_S --out $OUT"
  fi
  dx_info "  would run: bench_submit.py --dir $OUT --suite $SUITE --scenario $SCENARIO"
  dx_pass plan "printed above; nothing was launched, shaped or posted"
  dx_result "$TARGET" "suite=$SUITE" "scenario=$SCENARIO" "dry_run=1"
fi

mkdir -p "$OUT"

# ── host API helpers ─────────────────────────────────────────────────────────
# Everything admin-credentialed runs ON the stack host, against its host-local
# API URL — the same rule session_soak.sh and abr_ladder_netem.sh follow.
API_BASE="${DX_REMOTE_API:-http://127.0.0.1:$DX_CP_PORT}"
[ -n "${QSES_ADMIN_TOKEN:-}" ] || dx_guard "$TARGET" \
  "QSES_ADMIN_TOKEN is unset — mint one (see .claude/skills/quasar-session/SKILL.md) and export it"
dx_sanitize_admin_token "$TARGET"

host_curl() { # host_curl <path> [method]
  local method="${2:-GET}"
  if [ "$DX_HOST" = local ]; then
    curl -fsSk -m 20 -X "$method" "$API_BASE$1" -H "Authorization: Bearer $QSES_ADMIN_TOKEN"
  else
    dx_ssh_remote "curl -fsSk -m 20 -X $method '$API_BASE$1' -H 'Authorization: Bearer $QSES_ADMIN_TOKEN'"
  fi
}

# Same request, but yields ONLY the HTTP status — for calls where "did it work"
# is the answer we need and `-f`'s silent non-zero is not enough.
host_curl_code() { # host_curl_code <path> [method]
  local method="${2:-GET}"
  if [ "$DX_HOST" = local ]; then
    curl -sSk -m 20 -o /dev/null -w '%{http_code}' -X "$method" "$API_BASE$1" \
      -H "Authorization: Bearer $QSES_ADMIN_TOKEN"
  else
    dx_ssh_remote "curl -sSk -m 20 -o /dev/null -w '%{http_code}' -X $method '$API_BASE$1' -H 'Authorization: Bearer $QSES_ADMIN_TOKEN'"
  fi
}

# Same request, but takes a JSON body — for the latency_probe host-settings
# PATCH (arm/restore), a bodied request host_curl/host_curl_code cannot send.
host_curl_body() { # host_curl_body <path> <method> <json-body>
  if [ "$DX_HOST" = local ]; then
    curl -fsSk -m 20 -X "$2" "$API_BASE$1" \
      -H "Authorization: Bearer $QSES_ADMIN_TOKEN" -H 'Content-Type: application/json' -d "$3"
  else
    dx_ssh_remote "curl -fsSk -m 20 -X $2 '$API_BASE$1' -H 'Authorization: Bearer $QSES_ADMIN_TOKEN' -H 'Content-Type: application/json' -d '$3'"
  fi
}

# PATCH an app's profile_policy. Used only by the profile_policy=force preflight
# below (a JSON-body PATCH, unlike host_curl/host_curl_code which are body-less).
host_patch_profile_policy() { # host_patch_profile_policy <app_id> <policy>
  local body
  body="$(printf '{"profile_policy":"%s"}' "$2")"
  if [ "$DX_HOST" = local ]; then
    curl -fsSk -m 20 -X PATCH "$API_BASE/v1/apps/$1" \
      -H "Authorization: Bearer $QSES_ADMIN_TOKEN" -H 'Content-Type: application/json' -d "$body"
  else
    dx_ssh_remote "curl -fsSk -m 20 -X PATCH '$API_BASE/v1/apps/$1' -H 'Authorization: Bearer $QSES_ADMIN_TOKEN' -H 'Content-Type: application/json' -d '$body'"
  fi
}

# ── admin-token preflight (#508) ─────────────────────────────────────────────
# One authenticated call BEFORE anything else touches the host API — the
# latency-probe host resolution, the profile_policy=force preflight, and
# running_sids() below all swallow curl failures (running_sids in particular
# reads a failed call and an empty session list identically), so a bad
# QSES_ADMIN_TOKEN (expired, or a transport failure like the two-line
# cache-file trap dx_sanitize_admin_token above guards against) previously
# surfaced tens of minutes later as "could not resolve exactly one host" then
# "qses run exited before a session reached running" — while the session had
# actually launched fine. Fail hard here instead: anything other than HTTP
# 200 (000, a curl transport failure, included) is fatal, before any session
# is launched.
PREFLIGHT_CODE="$(host_curl_code /v1/admin/sessions 2>/dev/null || echo 000)"
if [ "$PREFLIGHT_CODE" != 200 ]; then
  dx_fail admin-token "preflight GET /v1/admin/sessions -> HTTP $PREFLIGHT_CODE — QSES_ADMIN_TOKEN is likely expired or invalid (a curl transport failure, e.g. the two-line ~/.cache/quasar/<host>.token cache-file trap, also reads as HTTP 000 here). Mint a fresh token: make admin-token HOST=$DX_HOST ARGS='--fresh' (or scripts/dx/admin_token.sh --host $DX_HOST --fresh), then re-export QSES_ADMIN_TOKEN and retry. Nothing was launched."
  dx_result "$TARGET" "out=$OUT"
fi
dx_pass admin-token "preflight GET /v1/admin/sessions -> HTTP 200"

running_sids() {
  host_curl "/v1/admin/sessions" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for s in (d.get("items") or d.get("sessions") or []):
    if s.get("state") == "running":
        print(s["id"])
' || true
}

# The state of ONE session, or "" when the question could not be asked. The
# distinction matters: `running_sids` returns nothing both when the session is
# gone AND when the API call failed outright, so polling a LIST for absence
# reports a 401 as a successful stop.
session_state() { # session_state <sid>
  host_curl "/v1/sessions/$1" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
s = d.get("session") if isinstance(d.get("session"), dict) else d
print((s or {}).get("state") or "")
' || true
}

# ── launch the peer-attached session ─────────────────────────────────────────
BEFORE="$(running_sids | sort | tr '\n' ' ')"
dx_info "pre-existing running sessions: ${BEFORE:-none}"

# NOTE: qses takes `--stack=NAME` (equals form only) — a space-separated
# `--stack NAME` is rejected with "unknown arg --stack".
# `--profile forced` = the APP ROW pins the profile server-side (profile_policy=force).
# Such an app rejects a launch that carries profile_id at all (409 "profile
# overrides are disabled for this launch"), so the cell is pinned MORE strongly
# than by --profile, not less — which is why the "an unpinned launch is not a
# benchmark cell" guard is satisfied without sending one.
QSES_ARGS=(run "--stack=$DX_HOST" --secs "$PEER_S" --keep)
[ "$PROFILE" = forced ] || QSES_ARGS+=(--profile "$PROFILE")
[ -n "$CODEC" ] && QSES_ARGS+=(--codec "$CODEC")
[ "$BENCH_MODE" = 1 ] && QSES_ARGS+=(--bench-mode)
[ "$PEER_UNLOCK_FPS" = 1 ] && QSES_ARGS+=(--peer-unlock-fps)
[ -n "$PULSE_EVERY" ] && QSES_ARGS+=(--input-pulse-every "$PULSE_EVERY")
# A pinned playout is a CELL DIMENSION, so it is tagged as well as applied: an
# arm measured at a fixed receiver buffer must never be confused with one that
# let the AS-05 controller move it. `playout=auto` records "controller ran".
[ -n "$PLAYOUT" ] && QSES_ARGS+=(--playout "$PLAYOUT")
case " ${TAGS[*]-} " in *" playout="*) ;; *) TAGS+=("playout=${PLAYOUT:-auto}") ;; esac
[ -n "$APP" ] && QSES_ARGS+=(--app "$APP")

SID=""
PEER_PID=""
STOPPED=0

# profile_policy=force preflight (2026-08-19 overnight-2 harness note #3): set
# by the block below, right after the EXIT/INT/TERM trap is armed, never
# before — so the trap can restore the app's policy on every exit path
# including a failure between the PATCH and the launch.
FORCE_OVERRIDE_APP_ID=""
FORCE_OVERRIDE_ORIG_POLICY=""
FORCE_OVERRIDE_RESTORED=0

restore_profile_policy() {
  [ -n "$FORCE_OVERRIDE_APP_ID" ] || return 0
  [ "$FORCE_OVERRIDE_RESTORED" = 1 ] && return 0
  FORCE_OVERRIDE_RESTORED=1
  if host_patch_profile_policy "$FORCE_OVERRIDE_APP_ID" "$FORCE_OVERRIDE_ORIG_POLICY" >/dev/null 2>&1; then
    dx_info "restored profile_policy=$FORCE_OVERRIDE_ORIG_POLICY on app $FORCE_OVERRIDE_APP_ID"
  else
    dx_warn profile-policy \
      "failed to restore profile_policy=$FORCE_OVERRIDE_ORIG_POLICY on app $FORCE_OVERRIDE_APP_ID — restore it by hand: curl -X PATCH \$API_BASE/v1/apps/$FORCE_OVERRIDE_APP_ID -H 'Authorization: Bearer <admin>' -d '{\"profile_policy\":\"$FORCE_OVERRIDE_ORIG_POLICY\"}'"
  fi
}

stop_session() {
  [ -n "$SID" ] || return 0
  [ "$KEEP" = 1 ] && { dx_info "--keep: leaving session $SID running"; return 0; }
  [ "$STOPPED" = 1 ] && return 0
  STOPPED=1
  # DELETE directly on the admin token this script already holds, rather than
  # shelling out to `qses stop`: that verb resolves its own credential over ssh on
  # the stack host, and this script has a working one in hand. (`qses stop` itself
  # was fixed alongside this — it now honours QSES_ADMIN_TOKEN, falls back to the
  # per-boot dev key, and verifies the session actually reached a terminal state.
  # Either route is correct now; this one is simply one fewer moving part.)
  #
  # What is NOT optional is the verification. A leaked session holds an encode
  # slot, and across a 12-cell matrix that ends with admission refusing launches.
  local code
  code="$(host_curl_code "/v1/sessions/$SID" DELETE 2>/dev/null || echo 000)"
  case "$code" in
    2*|404) : ;;  # accepted, or already gone
    *) dx_warn stop "DELETE /v1/sessions/$SID returned HTTP $code — checking the session state anyway" ;;
  esac
  # Verify against the SESSION, never against absence from a list: a failed list
  # call also returns nothing, which would read as a successful stop.
  local state=""
  for _ in $(seq 1 20); do
    sleep 3
    state="$(session_state "$SID")"
    case "$state" in stopped|failed|"") break ;; esac
  done
  case "$state" in
    stopped|failed)
      dx_info "stopped session $SID (state=$state)" ;;
    "")
      dx_fail stop "could not read the state of session $SID back (DELETE returned HTTP $code) — the session may STILL be running and holding an encode slot; check it by hand before the next run" ;;
    *)
      dx_fail stop "session $SID is still '$state' after DELETE (HTTP $code) — it holds an encode slot; stop it before the next run" ;;
  esac
}

# ── latency_probe host override (bench mode only, --no-probe opts out) ───────
# Same snapshot/PATCH/verify/restore-in-trap shape as bench_suite.sh's ABR
# settings: the host is PATCHed BEFORE launch (so the probe is active for the
# whole session), the read-back is verified rather than trusted, and the prior
# value is restored on every exit path including a failed launch. Resolved via
# GET /v1/hosts the same way bench_suite.sh does — this script cannot wait for
# the session to tell it host_id, because the probe must be armed before launch.
PROBE_HOST_ID=""
PROBE_ARMED=0
PROBE_RESTORED=0
PROBE_BEFORE_VALUE="__unset__"

# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
restore_probe_override() {
  [ "$PROBE_ARMED" = 1 ] || return 0
  [ "$PROBE_RESTORED" = 1 ] && return 0
  PROBE_RESTORED=1
  local body
  if [ "$PROBE_BEFORE_VALUE" = "__unset__" ]; then
    body='{"overrides":{"latency_probe":null}}'
  else
    body="{\"overrides\":{\"latency_probe\":$PROBE_BEFORE_VALUE}}"
  fi
  if host_curl_body "/v1/admin/hosts/$PROBE_HOST_ID/settings" PATCH "$body" >/dev/null 2>&1; then
    dx_info "latency_probe override restored on host $PROBE_HOST_ID (-> $PROBE_BEFORE_VALUE)"
  else
    dx_warn probe "failed to restore latency_probe on host $PROBE_HOST_ID — re-apply by hand: curl -X PATCH \$API_BASE/v1/admin/hosts/$PROBE_HOST_ID/settings -H 'Authorization: Bearer <admin>' -d '$body'"
  fi
}

# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ -n "$PEER_PID" ] && kill -0 "$PEER_PID" 2>/dev/null; then
    kill -TERM "$PEER_PID" 2>/dev/null || true
    wait "$PEER_PID" 2>/dev/null || true
  fi
  stop_session
  restore_profile_policy
  restore_probe_override
  exit "$rc"
}
trap cleanup EXIT INT TERM

# Arm the probe now that the trap can restore it on any exit path from here on
# (never before the trap — a failure between PATCH and trap-arm would leave the
# override stuck, exactly the mistake the profile_policy preflight below avoids
# by the same rule).
if [ "$BENCH_MODE" = 1 ] && [ "$PROBE" = 1 ]; then
  PROBE_HOST_ID="$(host_curl /v1/hosts 2>/dev/null | python3 -c '
import sys, json
try:
    items = (json.load(sys.stdin) or {}).get("items") or []
except Exception:
    items = []
print(items[0]["id"] if len(items) == 1 else "")
' 2>/dev/null || true)"
  if [ -z "$PROBE_HOST_ID" ]; then
    dx_warn probe "could not resolve exactly one host from GET /v1/hosts — latency_probe was NOT armed for this run"
  else
    PROBE_BEFORE_VALUE="$(host_curl "/v1/admin/hosts/$PROBE_HOST_ID/settings" 2>/dev/null | python3 -c '
import sys, json
try:
    ov = (json.load(sys.stdin) or {}).get("overrides") or {}
except Exception:
    ov = {}
print(json.dumps(ov["latency_probe"]).lower() if "latency_probe" in ov else "__unset__")
' 2>/dev/null || echo "__unset__")"
    if host_curl_body "/v1/admin/hosts/$PROBE_HOST_ID/settings" PATCH '{"overrides":{"latency_probe":true}}' >/dev/null 2>&1; then
      PROBE_ARMED=1
      PROBE_NOW="$(host_curl "/v1/admin/hosts/$PROBE_HOST_ID/settings" 2>/dev/null | python3 -c '
import sys, json
try:
    ov = (json.load(sys.stdin) or {}).get("overrides") or {}
except Exception:
    ov = {}
print(str(ov.get("latency_probe")).lower())
' 2>/dev/null || echo "")"
      if [ "$PROBE_NOW" = true ]; then
        dx_pass probe "latency_probe armed on host $PROBE_HOST_ID (verified overrides.latency_probe=true, was $PROBE_BEFORE_VALUE)"
      else
        dx_warn probe "PATCH returned OK but overrides.latency_probe read back '$PROBE_NOW' (wanted true) on host $PROBE_HOST_ID — tagging the run anyway, but treat host-stage probe stages as suspect"
      fi
      # "on", not "1": the effective-settings flag() below stringifies bool
      # hostcfg keys as on/off (matching abr_enabled etc), and quasar-bench's
      # tag-vs-effective mismatch check compares the tag verbatim against
      # `conditions.effective.latency_probe` — "1" vs "on" is a same-meaning,
      # different-spelling false positive that FAILS the run as mislabelled
      # (caught live on devbox 2026-08-19 verifying this very feature).
      TAGS+=("latency_probe=on")
    else
      dx_warn probe "PATCH to arm latency_probe on host $PROBE_HOST_ID failed — running without it"
      PROBE_HOST_ID=""
    fi
  fi
fi

# ── profile_policy=force preflight ───────────────────────────────────────────
# 2026-08-19 overnight-2 harness note #3: `POST /v1/sessions {profile_id: ...}`
# hard-409s (ErrProfileOverrideDisabled, "profile overrides are disabled for
# this launch") for ANY non-admin profile_id against an app whose
# profile_policy is `force` — including the app's own default, on a
# control-plane build that predates the launcher.go carve-out for that exact
# case (fixed alongside this, control-plane/internal/session/launcher.go, but
# this harness must not assume every target host is running that fix yet).
# `--profile forced` already sidesteps this by sending no profile_id at all;
# this block covers the remaining case, a PINNED --profile against a `force`
# app, by relaxing the app to `prefer` for the duration of the run using the
# admin token this script already holds, and restoring it via the trap above
# on every exit path (idempotent — restore_profile_policy runs once).
if [ "$PROFILE" != forced ]; then
  BENCH_APP_NAME="$APP"
  if [ -z "$BENCH_APP_NAME" ] && [ -f "$DX_HOSTS_JSON" ] && dx_have python3; then
    BENCH_APP_NAME="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print(""); sys.exit(0)
key = sys.argv[2]
name = d.get("roles", {}).get(key, key)
print(d.get("hosts", {}).get(name, {}).get("bench_app", ""))
' "$DX_HOSTS_JSON" "$DX_HOST")"
  fi
  if [ -n "$BENCH_APP_NAME" ]; then
    FA_INFO="$(host_curl "/v1/apps" 2>/dev/null | BENCH_APP_NAME="$BENCH_APP_NAME" python3 -c '
import json, os, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
name = os.environ.get("BENCH_APP_NAME", "")
for a in (d.get("items") or d.get("apps") or []):
    if a.get("name") == name:
        print(a.get("id") or "")
        print(a.get("profile_policy") or "")
        print(a.get("default_profile_id") or "")
        break
' 2>/dev/null || true)"
    FA_ID="$(printf '%s\n' "$FA_INFO" | sed -n '1p')"
    FA_POLICY="$(printf '%s\n' "$FA_INFO" | sed -n '2p')"
    FA_DEFAULT="$(printf '%s\n' "$FA_INFO" | sed -n '3p')"
    if [ -n "$FA_ID" ] && [ "$FA_POLICY" = force ] && [ "$PROFILE" != "$FA_DEFAULT" ]; then
      dx_info "preflight: app '$BENCH_APP_NAME' is profile_policy=force (default=${FA_DEFAULT:-<none>}) and this run pins --profile $PROFILE, which would 409 — PATCHing profile_policy to 'prefer' for the run's duration, restoring to 'force' on exit"
      if host_patch_profile_policy "$FA_ID" prefer >/dev/null 2>&1; then
        FORCE_OVERRIDE_APP_ID="$FA_ID"
        FORCE_OVERRIDE_ORIG_POLICY="$FA_POLICY"
        TAGS+=("profile_policy_overridden=1")
      else
        dx_guard "$TARGET" "preflight PATCH to relax profile_policy on app '$BENCH_APP_NAME' ($FA_ID) failed — refusing to launch a --profile that would 409 anyway"
      fi
    fi
  fi
fi

# Stamped BEFORE the launch: --app-log-glob uses it to keep the app-side pull to
# the files THIS session produced (see the pull block below). A few seconds of
# slack is harmless — a previous session's files are minutes or days older.
LAUNCH_EPOCH="$(date -u +%s)"

dx_info "launching the peer-attached session (qses run --keep, backgrounded)"
bash "$QSES" "${QSES_ARGS[@]}" > "$OUT/qses-run.log" 2>&1 &
PEER_PID=$!

# Poll for a running session that was NOT there before — never adopt somebody
# else's session, and never guess from a fixed sleep.
for _ in $(seq 1 60); do
  sleep 5
  for s in $(running_sids); do
    case " $BEFORE " in *" $s "*) continue ;; esac
    SID="$s"; break
  done
  [ -n "$SID" ] && break
  if ! kill -0 "$PEER_PID" 2>/dev/null; then
    dx_fail launch "qses run exited before a session reached running — see $OUT/qses-run.log"
    dx_result "$TARGET" "out=$OUT"
  fi
done
[ -n "$SID" ] || { dx_fail launch "no new running session appeared within 300s — see $OUT/qses-run.log"; dx_result "$TARGET" "out=$OUT"; }
dx_pass launch "session $SID running at profile $PROFILE"

# The host to read settings from is the one THIS SESSION landed on — ask the
# session, do not take items[0] of GET /v1/hosts. On a multi-host control plane
# items[0] is a coin flip, and it would tag the run with another host's encoder
# and ABR configuration without a word.
HOST_ID="$(host_curl "/v1/sessions/$SID" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
s = d.get("session") if isinstance(d.get("session"), dict) else d
print((s or {}).get("host_id") or "")
' 2>/dev/null || true)"
[ -n "$HOST_ID" ] || dx_warn host "could not read host_id from session $SID — the run will carry no host-configuration tags"

# ── conditions: what the host was ACTUALLY doing, captured AT LAUNCH ──────────
# At launch, not at submission: by the time the run is submitted the suite has
# already PATCHed the host for the NEXT cell, so a settings read at the end would
# describe a configuration this stream never ran under.
SETTINGS_JSON="$OUT/host-settings.json"
if [ -n "$HOST_ID" ]; then
  host_curl "/v1/admin/hosts/$HOST_ID/settings" > "$SETTINGS_JSON" 2>/dev/null \
    || dx_warn settings "could not read GET /v1/admin/hosts/$HOST_ID/settings"

  # #528: the PATCH that armed latency_probe (above) only verified
  # `overrides.latency_probe` — the SAME read that quasar-bench later compares
  # against the `latency_probe=on` tag is `effective.latency_probe`, which the
  # node agent only refreshes once it echoes back a fresh config_update. On a
  # host whose override was not already warm, the read above can win that race
  # and land here with `effective.latency_probe=false`, so the run gets tagged
  # `on` but posts as mismatched and quasar-bench fails the whole submission.
  # Bounded poll (10 x 1s, matching the verify-read style used to arm the
  # override itself) rather than a fixed sleep or an unbounded loop — if the
  # agent still hasn't echoed after 10s the run proceeds with whatever was last
  # read (same as before this fix), just no longer racing the common case.
  if [ "$PROBE_ARMED" = 1 ] && [ -n "$HOST_ID" ]; then
    EFF_PROBE_NOW="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    eff = d.get("effective") or {}
except Exception:
    eff = {}
print(str(eff.get("latency_probe")).lower())
' "$SETTINGS_JSON" 2>/dev/null || echo "")"
    ATTEMPT=0
    while [ "$EFF_PROBE_NOW" != true ] && [ "$ATTEMPT" -lt 10 ]; do
      ATTEMPT=$((ATTEMPT + 1))
      sleep 1
      host_curl "/v1/admin/hosts/$HOST_ID/settings" > "$SETTINGS_JSON" 2>/dev/null \
        || { dx_warn settings "could not re-read GET /v1/admin/hosts/$HOST_ID/settings while waiting for latency_probe to echo"; break; }
      EFF_PROBE_NOW="$(python3 -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    eff = d.get("effective") or {}
except Exception:
    eff = {}
print(str(eff.get("latency_probe")).lower())
' "$SETTINGS_JSON" 2>/dev/null || echo "")"
    done
    if [ "$EFF_PROBE_NOW" = true ]; then
      dx_pass settings "effective.latency_probe=true confirmed on host $HOST_ID (after ${ATTEMPT}s wait) before recording conditions.json"
    else
      dx_warn settings "effective.latency_probe never echoed true on host $HOST_ID after ${ATTEMPT}s — conditions.json will still show '$EFF_PROBE_NOW' against tag latency_probe=on; quasar-bench may flag this as mismatched"
    fi
  fi
fi

# Other non-terminal sessions sharing the host. A benchmark cell run beside two
# other live encodes is not the same cell, and this is the only record of it.
CONCURRENT="$(host_curl "/v1/admin/sessions" 2>/dev/null | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(0); sys.exit(0)
me, host = sys.argv[1], sys.argv[2]
n = 0
for s in (d.get("items") or d.get("sessions") or []):
    if s.get("id") == me:
        continue
    if s.get("state") in ("stopped", "failed", "terminated", "error"):
        continue
    if host and s.get("host_id") and s.get("host_id") != host:
        continue
    n += 1
print(n)
' "$SID" "$HOST_ID" 2>/dev/null || echo 0)"
dx_info "concurrent non-terminal sessions on the host: ${CONCURRENT:-0}"

python3 - "$SETTINGS_JSON" "$OUT/conditions.json" "${NETEM:-none}" "${CONCURRENT:-0}" "$PEER_HOST" "$PEER_UNLOCK_FPS" <<'PY' || dx_warn conditions "could not write conditions.json"
import json, os, sys
settings_path, out_path, netem, concurrent, peer_host, peer_unlock_fps = sys.argv[1:7]
eff = {}
try:
    with open(settings_path) as fh:
        d = json.load(fh)
    eff = dict(d.get("effective") or {})
except Exception:
    eff = {}

# Tag-space aliases, byte-for-byte the same projection the tag derivation below
# uses. quasar-bench compares `conditions.effective` keys against the SAME-NAMED
# tag, so an alias is what makes `ladder_resolution` comparable at all; the raw
# keys stay too, as evidence.
def flag(v):
    s = str(v).lower()
    return "on" if s == "true" else ("off" if s == "false" else str(v))

alias = {"abr_ladder_resolution": "ladder_resolution",
         "abr_ladder_fps": "ladder_fps",
         "abr_ladder_floor_follows_rung": "floor_follows_rung"}
projected = {}
for k, v in eff.items():
    projected[k] = flag(v) if isinstance(v, bool) or str(v).lower() in ("true", "false") else str(v)
    if k in alias:
        projected[alias[k]] = flag(v)

cond = {"effective": projected,
        "netem": {"level": netem},
        "concurrent_sessions": int(concurrent or 0),
        "peer_host": peer_host,
        "peer_unlock_fps": int(peer_unlock_fps or 0)}
with open(out_path, "w") as fh:
    json.dump(cond, fh, indent=2, sort_keys=True)
PY

dx_info "settling ${SETTLE}s for the headless peer to attach and warm up"
sleep "$SETTLE"

# ── observe ──────────────────────────────────────────────────────────────────
SOAK_RC=0
if [ -n "$NETEM" ]; then
  HOST="$DX_HOST" bash "$DX_DIR/abr_ladder_netem.sh" "$SID" \
    --levels "$NETEM" --dwell "$SECS" --out "$OUT" || SOAK_RC=$?
else
  HOST="$DX_HOST" bash "$DX_DIR/session_soak.sh" "$SID" \
    --profile observe --duration "$OBSERVE_S" --out "$OUT" || SOAK_RC=$?
fi
[ "$SOAK_RC" = 0 ] && dx_pass observe "telemetry captured into $OUT" \
  || dx_warn observe "the observe stage exited $SOAK_RC — submitting whatever it captured"

# Let the held peer FINISH before any teardown, for every run — not only bench
# mode. PEER_S = settle + observe + 60, so the peer outlives the observe stage
# by design; its end-of-run readout (DECODE fps / LUMA steady state, written to
# qses-run.log) is only meaningful if the session is still alive when it reads.
# Stopping the session first turns that readout into a measurement of a dead
# stream — fps=0, rtt=None, LUMA≈0.7 (black) — which is exactly how the
# 2026-08-22 av1 bench suite was misread as "every cell black". On a failed
# observe stage the peer may have several minutes left; skip the wait then and
# say the readout is not evidence, rather than burn the matrix's wall clock.
if [ "$SOAK_RC" = 0 ] && [ -n "$PEER_PID" ] && kill -0 "$PEER_PID" 2>/dev/null; then
  dx_info "waiting up to 240s for the headless peer to finish its held window"
  for _ in $(seq 1 240); do
    kill -0 "$PEER_PID" 2>/dev/null || break
    sleep 1
  done
  kill -0 "$PEER_PID" 2>/dev/null \
    && dx_warn peer "peer still running after 240s — proceeding to teardown (its readout may describe a stopped stream)"
elif [ "$SOAK_RC" != 0 ] && [ -n "$PEER_PID" ] && kill -0 "$PEER_PID" 2>/dev/null; then
  dx_warn peer "observe failed with the peer still holding — the DECODE/LUMA lines in qses-run.log will describe a stream torn down mid-hold; do not read them as stream health"
fi

# ── optional app-side files out of the managed home ──────────────────────────
APP_ARTIFACTS=()
if [ -n "$APP_LOG_GLOB" ]; then
  HOME_ROOT=/var/lib/quasar/homes
  if [ -s "$SETTINGS_JSON" ]; then
    HOME_ROOT="$(python3 -c '
import sys, json
d = json.load(open(sys.argv[1]))
eff = d.get("effective") or {}
res = d.get("resolved") or {}
print(eff.get("home_root") or res.get("home_root") or "/var/lib/quasar/homes")
' "$SETTINGS_JSON" 2>/dev/null || echo /var/lib/quasar/homes)"
  fi
  mkdir -p "$OUT/app"
  dx_info "pulling app files matching '$APP_LOG_GLOB' from $HOME_ROOT on $DX_HOST"
  if [ "$DX_HOST" = local ]; then
    # shellcheck disable=SC2086  # the glob must be expanded, that is the point
    cp $HOME_ROOT/$APP_LOG_GLOB "$OUT/app/" 2>/dev/null || true
  else
    # Two things this line has to get right, both learned the hard way on devbox
    # 2026-08-17:
    #
    # 1. `cd` FIRST, tar with no -C. tar's -C changes TAR's directory, but the glob
    #    is expanded by the remote SHELL before tar ever runs — against the ssh
    #    session's own cwd (the login home), where `agent-*/...` matches nothing.
    #    The pattern reached tar as a literal and every pull came back empty with
    #    only a "nothing matched" warning to show for it.
    #
    # 2. Scope to THIS session by mtime. `$HOME_ROOT` holds every managed home the
    #    box has ever made, so an `agent-*/…` glob matches every past run of the
    #    app as well as this one. Since the artifacts are uploaded by BASENAME, ~20
    #    files all called `summary.jsonl` collapse to one and an arbitrary winner
    #    is attached — a completion receipt for somebody else's session, which is
    #    worse than no receipt. `-newermt` against the launch time cuts it to the
    #    files this run actually produced.
    # The -path pattern is SINGLE-QUOTED for the remote shell (it sits inside the
    # local double quotes, so the quotes survive verbatim): find must receive the
    # pattern literally. Left bare it is glob-expanded by the remote shell in
    # $HOME_ROOT into dozens of paths, find sees several -path arguments, and the
    # whole pipeline silently yields an empty archive.
    dx_ssh_remote "cd '$HOME_ROOT' && { sudo find . -path './$APP_LOG_GLOB' -newermt '@$LAUNCH_EPOCH' -print0 2>/dev/null || find . -path './$APP_LOG_GLOB' -newermt '@$LAUNCH_EPOCH' -print0 2>/dev/null; } | { sudo tar cf - --null -T - 2>/dev/null || tar cf - --null -T - 2>/dev/null; }" \
      | tar xf - -C "$OUT/app" 2>/dev/null || true
  fi
  while IFS= read -r f; do
    [ -n "$f" ] && APP_ARTIFACTS+=(--artifact "$f")
  done < <(find "$OUT/app" -type f 2>/dev/null)
  if [ "${#APP_ARTIFACTS[@]}" -gt 0 ]; then
    dx_pass app-logs "$(( ${#APP_ARTIFACTS[@]} / 2 )) file(s) pulled"
  else
    dx_warn app-logs "nothing matched '$APP_LOG_GLOB' under $HOME_ROOT"
  fi
fi

# ── bench mode: the in-page marker-decode windows ────────────────────────────
# Two independent sources, in priority order:
#   1. the peer's own readout of `window.__qBench.windows()`, printed by qses as
#      a single BENCH_JSON line. This works against ANY control plane, including
#      one that predates the `bench.window` trace-event type.
#   2. GET /v1/admin/sessions/{id}/trace/events?types=bench.window — the durable
#      path, once a control plane carrying the allow-list entry is deployed.
# (1) is authoritative when present because it is the unfiltered series; (2) is
# the fallback and the cross-check.
if [ "$BENCH_MODE" = 1 ]; then
  # The peer prints its BENCH_JSON line only when the driver EXITS, and the peer
  # is deliberately held longer than the observe window (PEER_S = settle +
  # observe + 60) so the session never self-stops mid-measurement. So we have to
  # wait for it here — reading the log the moment observe ends finds a log that
  # stops at "HARNESS peer=..." and reports zero windows for a perfectly good run.
  if kill -0 "$PEER_PID" 2>/dev/null; then
    dx_info "waiting up to 180s for the peer to finish and print its bench readout"
    for _ in $(seq 1 180); do
      kill -0 "$PEER_PID" 2>/dev/null || break
      sleep 1
    done
    kill -0 "$PEER_PID" 2>/dev/null && dx_warn bench-peer "the peer is still running after 180s"
  fi
  BENCH_JSON="$OUT/bench-windows.json"
  if grep -q '^BENCH_JSON ' "$OUT/qses-run.log" 2>/dev/null; then
    grep '^BENCH_JSON ' "$OUT/qses-run.log" | tail -1 | cut -d' ' -f2- > "$BENCH_JSON"
  else
    dx_warn bench-peer "the peer printed no BENCH_JSON line — falling back to the trace-event path"
    if host_curl "/v1/admin/sessions/$SID/trace/events?types=bench.window" > "$OUT/bench-events.json" 2>/dev/null; then
      python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
items = d.get("events") or d.get("items") or []
json.dump({"windows": [e.get("payload") or {} for e in items]}, open(sys.argv[2], "w"))
' "$OUT/bench-events.json" "$BENCH_JSON" 2>/dev/null || rm -f "$BENCH_JSON"
    fi
  fi
  if [ -s "$BENCH_JSON" ]; then
    NWIN="$(python3 -c '
import json, sys
d = json.load(open(sys.argv[1]))
w = d.get("windows") if isinstance(d, dict) else d
print(len(w or []))
' "$BENCH_JSON" 2>/dev/null || echo 0)"
    if [ "${NWIN:-0}" -gt 0 ]; then
      dx_pass bench-windows "$NWIN one-second windows captured"
    else
      dx_warn bench-windows "the readout carried no windows — did the marker ever decode?"
    fi
  else
    dx_fail bench-windows "bench mode was on but no windows came back (peer readout and trace-event path both empty)"
  fi
fi

# ── session verdict + caps.negotiated (C11 phase 1) ──────────────────────────
# Best-effort GETs, right beside the trace-event fetch above. Neither may ever
# fail a run: an older control plane has no /verdict route and no
# caps.negotiated allow-list entry (404 either way), and a read racing a
# session that already finished is unremarkable. bench_submit.py folds both
# files, when present, into conditions/tags — see build_conditions() and
# read_caps_negotiated().
VERDICT_JSON="$OUT/verdict.json"
if host_curl "/v1/admin/sessions/$SID/verdict" > "$VERDICT_JSON" 2>/dev/null && [ -s "$VERDICT_JSON" ]; then
  VDICT="$(python3 -c '
import json, sys
try:
    print((json.load(open(sys.argv[1])) or {}).get("verdict") or "")
except Exception:
    print("")
' "$VERDICT_JSON" 2>/dev/null || echo "")"
  if [ -n "$VDICT" ]; then
    dx_pass verdict "session verdict: $VDICT"
  else
    rm -f "$VERDICT_JSON"
    dx_warn verdict "GET .../verdict returned no verdict field — discarding"
  fi
else
  rm -f "$VERDICT_JSON"
  dx_warn verdict "GET /v1/admin/sessions/$SID/verdict unavailable (older control plane, or no verdict yet) — submitting without a verdict condition"
fi

CAPS_JSON="$OUT/caps-negotiated.json"
if host_curl "/v1/admin/sessions/$SID/trace/events?types=caps.negotiated" > "$CAPS_JSON" 2>/dev/null && [ -s "$CAPS_JSON" ]; then
  dx_pass caps-negotiated "fetched"
else
  rm -f "$CAPS_JSON"
  dx_warn caps-negotiated "GET .../trace/events?types=caps.negotiated unavailable — codec/profile/encoder tags fall back to session.json"
fi

# ── fold app-side + bench-mode series into metrics.jsonl / trace.json ─────────
if [ "$BENCH_MODE" = 1 ] || [ -n "$APP_LOG_GLOB" ]; then
  FOLD_OUT="$(python3 "$DX_DIR/bench_app_samples.py" --dir "$OUT" --t0-ms "$(( LAUNCH_EPOCH * 1000 ))" 2>&1)" \
    && dx_pass app-samples "$FOLD_OUT" \
    || dx_warn app-samples "folding app/bench series failed: $FOLD_OUT"
fi

# ── tags: the host's LIVE configuration, not the operator's belief ───────────
HOST_TAGS=()
if [ -s "$SETTINGS_JSON" ]; then
  while IFS= read -r kv; do
    [ -n "$kv" ] && HOST_TAGS+=(--tag "$kv")
  done < <(python3 -c '
import sys, json
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
# `effective` is the truth: env + host overrides MERGED, all values as strings.
# `resolved` is the control-plane-side view and does NOT see the environment of
# the agent container — it reports encoder=openh264 (the compiled-in default) on
# a host whose deploy/.env sets QUASAR_ENCODER=nvenc. Reading `resolved` would
# tag every run with the wrong encoder. (No apostrophes in this block: it lives
# inside a single-quoted python3 -c argument.)
eff = d.get("effective") or {}
res = d.get("resolved") or {}
r = {k: eff.get(k, res.get(k)) for k in set(eff) | set(res)}
def flag(v):
    s = str(v).lower()
    if v is True or s == "true":
        return "on"
    if v is False or s == "false":
        return "off"
    return str(v)
for src, dst in (("abr_mode", "abr_mode"), ("encoder", "encoder"),
                 ("abr_enabled", "abr_enabled"),
                 ("abr_ladder_resolution", "ladder_resolution"),
                 ("abr_ladder_fps", "ladder_fps"),
                 ("abr_ladder_order", "abr_ladder_order"),
                 ("abr_ladder_floor_follows_rung", "floor_follows_rung")):
    if src in r:
        print("%s=%s" % (dst, flag(r[src])))
' "$SETTINGS_JSON" || true)
fi

# ── ice: WHICH PATH THE MEDIA TOOK, as a sliceable axis (#509) ───────────────
# Same "the host's LIVE configuration, not the operator's belief" rule as the
# tags above, applied to the transport path. A TURN-configured deployment whose
# media still went host-to-host is NOT a TURN cell, and before this the two were
# indistinguishable in the store: the first #509 TURN run compared byte-identical
# to baseline and read as a clean pass while relaying nothing.
#
# LOW CARDINALITY ONLY. qses prints ICE_JSON as an already-reduced summary; the
# raw driver block carries candidate IP addresses and TURN server URLs and is
# deliberately not lifted here, because tags go to an external bench store.
ICE_TAGS=()
if grep -q '^ICE_JSON ' "$OUT/qses-run.log" 2>/dev/null; then
  ICE_SUMMARY_JSON="$OUT/ice-summary.json"
  grep '^ICE_JSON ' "$OUT/qses-run.log" | tail -1 | cut -d' ' -f2- > "$ICE_SUMMARY_JSON"
  while IFS= read -r kv; do
    [ -n "$kv" ] && ICE_TAGS+=(--tag "$kv")
  done < <(python3 -c '
import sys, json
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
def s(v):
    return "none" if v is None or v == "" else str(v)
print("ice_local=%s" % s(d.get("local")))
print("ice_remote=%s" % s(d.get("remote")))
print("ice_relay_protocol=%s" % s(d.get("relay_protocol")))
print("ice_servers=%s" % s(d.get("servers")))
# The one derived boolean worth having: did media actually traverse a relay?
print("ice_relayed=%d" % (1 if "relay" in (s(d.get("local")), s(d.get("remote"))) else 0))
' "$ICE_SUMMARY_JSON" || true)
  if [ "${#ICE_TAGS[@]}" -gt 0 ]; then
    ICE_TAG_SUMMARY=""
    for kv in "${ICE_TAGS[@]}"; do
      [ "$kv" = --tag ] || ICE_TAG_SUMMARY="$ICE_TAG_SUMMARY $kv"
    done
    dx_pass ice-path "${ICE_TAG_SUMMARY# }"
  fi
else
  dx_warn ice-path "the peer printed no ICE_JSON line — this run cannot say whether media went host-to-host or over a relay"
fi

# ── submit ───────────────────────────────────────────────────────────────────
# ── I8: prove the codec pin, do not assume it ────────────────────────────────
# `qses` verifies at launch against the launch response; this is the SECOND,
# independent check — against `session.json`, the exact record bench_submit.py
# derives the `codec` tag from. If those two ever disagree, the cell would be
# submitted with a codec tag that contradicts the pin, which is the mislabelled-
# cell failure class this harness already treats as exit 3.
if [ -n "$CODEC" ] && [ "$CODEC" != auto ] && [ -s "$OUT/session.json" ]; then
  RESOLVED_CODEC="$(python3 -c '
import json, sys
try: print((json.load(open(sys.argv[1])) or {}).get("codec") or "")
except Exception: print("")
' "$OUT/session.json" 2>/dev/null || echo "")"
  if [ -z "$RESOLVED_CODEC" ]; then
    dx_warn codec-pin "session.json carries no codec — the pin could not be verified here (qses verified it at launch)"
  elif [ "$RESOLVED_CODEC" != "$CODEC" ]; then
    dx_fail codec-pin "asked --codec $CODEC but the session resolved $RESOLVED_CODEC — refusing to submit a mislabelled cell"
    dx_result "$TARGET" "suite=$SUITE" "scenario=$SCENARIO" "codec_pin=$CODEC" "codec_resolved=$RESOLVED_CODEC"
  else
    dx_pass codec-pin "pinned and resolved agree: $RESOLVED_CODEC"
  fi
fi

if [ "$SUBMIT" != 1 ]; then
  dx_info "--no-submit: telemetry left at $OUT"
  stop_session
  dx_result "$TARGET" "out=$OUT" "sid=$SID" "submitted=0"
fi

# The peer is held longer than the measurement, so the first --settle seconds of
# every unimpaired cell are peer-attach + app-start transient. Telling the
# submitter where that boundary is turns the single whole-hold `run` phase into
# `warmup` + `observe`, so `/v1/stats?...&window=observe` answers with the steady
# state instead of a figure ~2-3x inflated by start-up (devbox 2026-08-18:
# 2.38% whole-hold vs 0.86% observe on the same cell).
SUB=(--warmup-secs "$SETTLE" --dir "$OUT" --suite "$SUITE" --scenario "$SCENARIO" --host "$DX_HOST"
     --tag "launch_profile=$PROFILE" --tag "netem_level=${NETEM:-none}"
     # peer=<resolved host>, not the role: old, untagged runs implicitly ran
     # against hermes (docs/reports/2026-08-19-peer-path/REPORT.md) — this makes
     # every run from here on distinguishable by which host actually held the
     # WebRTC peer, which drops belong to Quasar's own pipeline.
     --tag "peer=$PEER_HOST")
[ -s "$OUT/conditions.json" ] && SUB+=(--conditions "$OUT/conditions.json")
[ -n "$APP" ] && SUB+=(--tag "app=$APP")
# The codec was PINNED for this cell, not observed after the fact — record which
# way. (bench_submit.py also derives a `codec` tag from session.json; an explicit
# --tag wins, and the two agreeing is the check that the pin actually took.)
[ -n "$CODEC" ] && SUB+=(--tag "codec_pin=$CODEC")
[ "$BENCH_MODE" = 1 ] && SUB+=(--tag "bench_mode=1")
SUB+=(--tag "peer_unlock_fps=$PEER_UNLOCK_FPS")
[ -n "$PULSE_EVERY" ] && SUB+=(--tag "input_pulse_every_s=$PULSE_EVERY")
[ "${#HOST_TAGS[@]}" -gt 0 ] && SUB+=("${HOST_TAGS[@]}")
[ "${#ICE_TAGS[@]}" -gt 0 ] && SUB+=("${ICE_TAGS[@]}")
[ "${#APP_ARTIFACTS[@]}" -gt 0 ] && SUB+=("${APP_ARTIFACTS[@]}")
# The bench readout travels as ARTIFACTS, not samples: bench-frames.json is a
# per-frame timeline (up to 5000 records), which is what makes drop attribution
# reconstructable from a submitted run rather than only from a live page.
[ -s "$OUT/bench-windows.json" ] && SUB+=(--artifact "$OUT/bench-windows.json")
[ -s "$OUT/bench-frames.json" ] && SUB+=(--artifact "$OUT/bench-frames.json")
for t in ${TAGS[@]+"${TAGS[@]}"}; do SUB+=(--tag "$t"); done

SUB_RC=0
SUB_OUT="$(python3 "$DX_DIR/bench_submit.py" "${SUB[@]}" 2>&1)" || SUB_RC=$?
printf '%s\n' "$SUB_OUT"
case "$SUB_RC" in
  0) dx_pass submit "posted to quasar-bench" ;;
  # rc 3: the run WAS posted, but a tag disagrees with what the host reported.
  # The evidence is kept (and tagged mismatch=1) — the CELL is what failed, and
  # the failure must propagate so bench_suite.sh stops rather than grinding out
  # the rest of the matrix under a knob nobody asked for.
  3) dx_fail submit "the run is posted but MISLABELLED — quasar-bench reports tag/condition mismatches (see above); it is tagged mismatch=1 at $OUT" ;;
  *) dx_fail submit "bench_submit.py failed (rc $SUB_RC) — telemetry kept at $OUT" ;;
esac

# ── the standing g2g budget table ────────────────────────────────────────────
# Every bench-mode run prints its reconciled stage budget, the same table
# `make bench-budget` prints by hand — this is what makes the budget a
# STANDING instrument rather than a one-off report (docs/testing-bench-mode.md
# "The glass-to-glass budget"). Informational only: a regressed stage here does
# NOT change this script's own exit code (bench_suite.sh's matrix bookkeeping
# depends on bench_run.sh's rc meaning "did the cell submit", not "did every
# stage stay inside its threshold") — read the table, or gate on it separately
# with `make bench-budget RUN=<id>`.
if [ "$BENCH_MODE" = 1 ] && [ "$SUB_RC" != 2 ]; then
  SUB_RUN_ID="$(printf '%s\n' "$SUB_OUT" | sed -n 's/.*RESULT .*run_id=\([0-9A-Za-z-]*\).*/\1/p' | tail -1)"
  if [ -n "$SUB_RUN_ID" ]; then
    dx_info "budget: scripts/dx/bench_budget.py --run $SUB_RUN_ID"
    python3 "$DX_DIR/bench_budget.py" --run "$SUB_RUN_ID" --suite "$SUITE" || true
  else
    dx_warn budget "could not find a run id in bench_submit.py's output — skipping the budget table"
  fi
fi

stop_session
dx_result "$TARGET" "out=$OUT" "sid=$SID" "suite=$SUITE" "scenario=$SCENARIO"
