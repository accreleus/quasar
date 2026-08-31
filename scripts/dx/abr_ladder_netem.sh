#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/abr_ladder_netem.sh — the D8 acceptance sequence for the ABR resolution
# ladder: run ONE continuous `session_soak.sh --profile observe` soak spanning a
# baseline hold + every netem level + a recovery hold, while THIS script shapes the
# sender's egress at each level boundary and records the wall-clock instant of every
# apply/clear to marks.jsonl.
#
# The soak is never stopped and re-launched per level — an earlier per-level design
# did that, and a ladder step landing near a level boundary would fall outside
# whichever short run's own trace window, silently reading back as unknown. One soak
# spanning the whole sequence, correlated after the fact via marks.jsonl, fixes that.
#
#   scripts/dx/abr_ladder_netem.sh <SID>|--latest [--levels mild,moderate,severe]
#                                  [--dwell 60] [--fps] [--out DIR] [--dry-run]
#   make abr-ladder SID=latest HOST=devbox
#   make abr-ladder SID=<sid> HOST=devbox ARGS='--fps'
#
# `--fps` is the D7 phase-2 (fps rung) scenario. It changes no shaping — the fps rung
# rides the SAME setpoint signal and the same netem levels as the resolution rung —
# but it defaults the sequence to `--levels moderate,severe --dwell 90` (the ladder
# must get deep enough to reach the hybrid pivot AND hold there long enough for the
# fps step's dwell) and it asserts the extra preconditions up front, because every one
# of them fails SILENTLY as "no fps steps in the report":
#
#   1. the host has `abr_ladder_fps=true` AND `abr_ladder_resolution=true` (the
#      planner reaches the fps rung through the resolution rung under `hybrid`),
#   2. `abr_ladder_order` is `hybrid` (default), `fps_first`, or `res_first`,
#   3. the SESSION launched at >= 120 fps — a 60 fps session has no fps rung at all
#      and the run will report resolution steps only, which is not a failure,
#   4. the agent image contains `videorate` (gst-plugins-base:videorate=enabled). An
#      image without it reports `fps_lever = false` and the ladder disables the rung
#      on its first attempt, logging "lever refused ... fps".
#
# Read the result in LADDER.md's steps table: `rung` is `fps` for a rate step, and its
# `from`/`to` are frame RATES (120 -> 60), not indices. Under `hybrid` the expected
# engage order is resolution rungs down to 1080p, THEN fps 120 -> 60, THEN deeper
# resolution; recovery must be the exact reverse.
#
# Timeline (all inside one `session_soak.sh --profile observe` run):
#   0                     baseline, no shaping (BASELINE_S — the pre-impairment norm)
#   + BASELINE_S          qnetem sender <level 1>            -> marks.jsonl {impair,level}
#   + dwell               qnetem sender <level 2> (or sender-clear for "clean") -> mark
#   ...
#   + dwell * N           qnetem sender-clear (always, final) -> {clear}
#   + RECOVERY_S           soak ends
#
# scripts/dx/session_soak_report.py reads marks.jsonl when present and anchors
# time-to-first-step on the FIRST `impair` mark and time-to-recover on the LAST
# `clear` mark — the marks are ground truth for when the network actually changed;
# the soak driver has no way to know that on its own (it never touches qnetem).
#
# Preconditions: the session is running and being played by a human (a static scene
# gives the encoder nothing to fail at); the host has `abr_ladder_resolution=true`
# (flip it in Host Settings — that flip is acceptance item 6); QUASAR_ENCODER is nvenc
# or va (Vulkan has no lever and the soak exits 3).
#
# Levels: clean|mild|moderate|severe. `clean` is issued as `qnetem sender-clear`
# (qnetem has no "clean" level of its own) and marked "clear"; anything else is
# marked "impair" with its level name.
#
# Host scope: this shapes a REMOTE host's live network egress, so it is a mutating
# remote verb exactly like up/down/restart/rebuild — HOST=<host> must be typed
# explicitly (see scripts/dx/common.sh DX_REMOTE_MUTATING_VERBS).
#
# Exit: 0 the run completed and no level reported pumping; 1 FAILED/DEGRADED; 2 usage.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
TARGET=abr-ladder
dx_require_host_scope "$TARGET"

usage() { sed -n '3,67p' "$0" | sed 's/^# \{0,1\}//'; }

QNETEM="$DX_ROOT/.claude/skills/quasar-netem/scripts/qnetem"
BASELINE_S=30
RECOVERY_S=120

# qnetem's sender role defaults to `gpu-test`, which is a DIFFERENT host from the
# one this run is soaking whenever HOST= names anything else. Shaping one host's
# egress while measuring another's session is silently wrong (and mutates an
# unrelated production-ish box), so pin the sender to the host under test.
# An explicit QNETEM_SENDER_ROLE in the environment still wins.
if [ "$DX_HOST" != local ]; then
  QNETEM_SENDER_ROLE="${QNETEM_SENDER_ROLE:-$DX_HOST}"
  export QNETEM_SENDER_ROLE
fi

# The host-local control-plane URL for best-effort trace annotations — same rule
# session_soak.sh's local/remote branches use.
if [ "$DX_HOST" = local ]; then
  API_BASE="http://127.0.0.1:$DX_CP_PORT"
else
  API_BASE="${DX_REMOTE_API:-}"
fi

# `make abr-ladder SID=<id>|latest ARGS='--dwell 240'` delivers both knobs by
# ENVIRONMENT, not interpolated into the recipe line (#550). This has to run
# BEFORE the defaults below: `SID=""` would shadow the environment value.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" SID ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

SID=""
LEVELS="mild,moderate,severe"
DWELL=60
OUT=""
DRY=0
FPS_SCENARIO=0
LEVELS_SET=0
DWELL_SET=0

if [ $# -eq 0 ]; then
  usage >&2
  exit 2
fi
case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  # `SID=latest` arrives as the bare word; it means the same as `--latest`.
  latest) SID="--latest"; shift ;;
  --*) ;;
  *) SID="$1"; shift ;;
esac

while [ $# -gt 0 ]; do
  case "$1" in
    --latest) SID="--latest"; shift ;;
    --levels)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--levels requires a comma list of clean|mild|moderate|severe"
      LEVELS="$2"; LEVELS_SET=1; shift 2 ;;
    --dwell)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--dwell requires seconds"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--dwell must be an integer (got '$2')" ;; esac
      DWELL="$2"; DWELL_SET=1; shift 2 ;;
    --out)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--out requires a directory"
      OUT="$2"; shift 2 ;;
    --fps) FPS_SCENARIO=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/abr_ladder_netem.sh --help" ;;
  esac
done

[ -n "$SID" ] || dx_guard "$TARGET" "no session — pass a <SID>, or --latest"
SID_ARG="$SID"

# ── --fps: the D7 phase-2 scenario ────────────────────────────────────────────
# Same shaping, a deeper/longer default sequence, and the preconditions spelled out
# — each of them fails silently as "no fps steps" otherwise (see the header).
if [ "$FPS_SCENARIO" = 1 ]; then
  [ "$LEVELS_SET" = 1 ] || LEVELS="moderate,severe"
  [ "$DWELL_SET" = 1 ] || DWELL=90
  dx_info "fps-rung scenario: levels=$LEVELS dwell=${DWELL}s"
  dx_info "  precondition 1/4  host knob abr_ladder_fps=true (and abr_ladder_resolution=true)"
  dx_info "  precondition 2/4  host knob abr_ladder_order in hybrid|res_first|fps_first"
  dx_info "  precondition 3/4  the session launched at >= 120 fps (60 fps has no fps rung)"
  dx_info "  precondition 4/4  the agent image has \`videorate\` (gst-plugins-base:videorate=enabled)"
  dx_info "  read LADDER.md: rung=fps steps carry frame RATES in from/to (120 -> 60)"
fi

# ── Validate --levels up front — a typo mid-sequence should never leave the
# sender half-shaped. ─────────────────────────────────────────────────────────
OLDIFS="$IFS"
IFS=','
# shellcheck disable=SC2206  # word-splitting on comma is deliberate here
LEVEL_LIST=($LEVELS)
IFS="$OLDIFS"
[ "${#LEVEL_LIST[@]}" -gt 0 ] || dx_guard "$TARGET" "--levels produced no tokens (got '$LEVELS')"
for lvl in "${LEVEL_LIST[@]}"; do
  case "$lvl" in
    clean|mild|moderate|severe) ;;
    *) dx_guard "$TARGET" "--levels token '$lvl' must be one of clean|mild|moderate|severe" ;;
  esac
done
N="${#LEVEL_LIST[@]}"
TOTAL_S=$((BASELINE_S + DWELL * N + RECOVERY_S))

# ── Output directory — decided HERE (not left to session_soak.sh's own default)
# so this script can write marks.jsonl into the same directory it passes via
# --out. ───────────────────────────────────────────────────────────────────────
STAMP="$(dx_timestamp)"
[ -n "$OUT" ] || OUT="$DX_DIAG_DIR/abr-ladder/$STAMP"

# ── ONE dispatcher for every side-effecting step: the description is built once
# and either just printed (--dry-run) or also executed — the two modes can never
# drift apart because there is only one call site per step. ──────────────────────
plan_or_run() { # plan_or_run <description> <function-name> [args...]
  local desc="$1"; shift
  dx_info "$desc"
  [ "$DRY" = 1 ] || "$@"
}

plan_or_sleep() { # plan_or_sleep <seconds> <description>
  dx_info "$2 (${1}s)"
  [ "$DRY" = 1 ] || sleep "$1"
}

# mark <impair|clear> [level] — appends the real wall-clock instant to marks.jsonl.
# shellcheck disable=SC2329  # only ever called via plan_or_run's "$@"
mark() {
  local kind="$1" level="${2:-}"
  python3 -c '
import json, sys, time
kind, level, path = sys.argv[1], sys.argv[2], sys.argv[3]
rec = {"ts_unix_ms": int(time.time() * 1000), "mark": kind}
if level:
    rec["level"] = level
with open(path, "a") as f:
    f.write(json.dumps(rec) + "\n")
' "$kind" "$level" "$OUT/marks.jsonl"
}

# annotate <label> — best-effort trace annotation (admin trace viewer). Never
# blocks or fails the run: no token / no API base / no curl / a 4xx are all
# silently swallowed. Skipped for --latest (the real session id is unknown here
# without an extra API round trip this script deliberately doesn't make).
# shellcheck disable=SC2329  # only ever called via plan_or_run's "$@"
annotate() {
  local label="$1"
  [ "$SID_ARG" != "--latest" ] || return 0
  [ -n "${QSES_ADMIN_TOKEN:-}" ] || return 0
  [ -n "$API_BASE" ] || return 0
  dx_have curl || return 0
  local ts
  ts="$(python3 -c 'import time; print(int(time.time() * 1000))')"
  curl -fsS -m 5 -X POST "$API_BASE/v1/admin/sessions/$SID_ARG/trace/annotations" \
    -H "Authorization: Bearer $QSES_ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d "{\"ts_unix_ms\":$ts,\"label\":$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$label"),\"tags\":[\"abr-ladder\"]}" \
    >/dev/null 2>&1 || true
}

SOAK_PID=""
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
start_soak() {
  HOST="$DX_HOST" bash "$DX_DIR/session_soak.sh" "$SID_ARG" --profile observe \
    --duration "$TOTAL_S" --out "$OUT" &
  SOAK_PID=$!
}
# shellcheck disable=SC2329  # only ever called via plan_or_run's "$@"
apply_level() {
  bash "$QNETEM" sender "$1"
  mark impair "$1"
  annotate "abr-ladder: impair $1"
}
# shellcheck disable=SC2329  # only ever called via plan_or_run's "$@"
apply_clear() {
  bash "$QNETEM" sender-clear
  mark clear
  annotate "abr-ladder: clear"
}

if [ "$DRY" != 1 ]; then
  dx_have python3 || { dx_fail python3 "not on PATH"; dx_result "$TARGET"; }
  [ -f "$QNETEM" ] || { dx_fail qnetem "$QNETEM missing"; dx_result "$TARGET"; }
  mkdir -p "$OUT"
  : > "$OUT/marks.jsonl"
fi

# Cleared on every exit path — a Ctrl-C mid-sequence must never leave the sender
# shaped, and the background soak must never be left orphaned.
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
cleanup() {
  [ "$DRY" = 1 ] && return 0
  bash "$QNETEM" sender-clear >/dev/null 2>&1 || true
  if [ -n "$SOAK_PID" ] && kill -0 "$SOAK_PID" 2>/dev/null; then
    kill -TERM "$SOAK_PID" 2>/dev/null || true
    wait "$SOAK_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

dx_info "plan: baseline ${BASELINE_S}s + ${N} level(s) x ${DWELL}s + recovery ${RECOVERY_S}s = ${TOTAL_S}s total, out=$OUT"

plan_or_run "observe soak (whole sequence): HOST=$DX_HOST bash $DX_DIR/session_soak.sh $SID_ARG --profile observe --duration $TOTAL_S --out $OUT" \
  start_soak
plan_or_sleep "$BASELINE_S" "baseline hold, no shaping"

for lvl in "${LEVEL_LIST[@]}"; do
  if [ "$lvl" = clean ]; then
    plan_or_run "qnetem sender-clear (mark clear)" apply_clear
  else
    plan_or_run "qnetem sender $lvl (mark impair level=$lvl)" apply_level "$lvl"
  fi
  plan_or_sleep "$DWELL" "hold at $lvl"
done

plan_or_run "qnetem sender-clear (mark clear, final)" apply_clear
plan_or_sleep "$RECOVERY_S" "recovery hold"

if [ "$DRY" = 1 ]; then
  dx_pass plan "printed above; nothing was PATCHed, shaped, or started"
  dx_result "$TARGET" "levels=$LEVELS" "dwell=$DWELL" "total=${TOTAL_S}s"
fi

dx_info "waiting for the observe soak to finish (should be imminent)"
wait "$SOAK_PID" || dx_warn soak "session_soak.sh exited non-zero — see $OUT/REPORT.md"
SOAK_PID=""

VERDICT=UNKNOWN
STEPS=0
TTFS="-"
TTR="-"
if [ -f "$OUT/summary.json" ]; then
  SUMMARY_LINE="$(python3 - "$OUT/summary.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
t = d.get("timings") or {}
print(d.get("overall", "UNKNOWN"),
      len(d.get("ladder_steps") or []),
      t.get("time_to_first_step_s", "-"),
      t.get("time_to_recover_s", "-"))
PY
)"
  read -r VERDICT STEPS TTFS TTR <<<"$SUMMARY_LINE"
fi

case "$VERDICT" in
  PASS) dx_pass run "verdict=PASS steps=$STEPS ttfs=${TTFS}s ttr=${TTR}s" ;;
  DEGRADED) dx_warn run "verdict=DEGRADED — see $OUT/REPORT.md" ;;
  *) dx_fail run "verdict=$VERDICT — see $OUT/REPORT.md" ;;
esac

# LADDER.md: the same summary.json, cut into per-mark windows so a human can see
# which level each ladder step landed in without re-deriving it from timestamps.
python3 - "$OUT" "$LEVELS" "$DWELL" "$BASELINE_S" > "$OUT/LADDER.md" <<'PY'
import json, os, sys

out, levels_csv, dwell, baseline = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

summary = {}
p = os.path.join(out, "summary.json")
if os.path.exists(p):
    summary = json.load(open(p))

marks = []
mp = os.path.join(out, "marks.jsonl")
if os.path.exists(mp):
    for line in open(mp):
        line = line.strip()
        if line:
            marks.append(json.loads(line))

steps = summary.get("ladder_steps") or []
timings = summary.get("timings") or {}
t0 = marks[0]["ts_unix_ms"] if marks else None


def offset(ts):
    return "%.1f" % ((ts - t0) / 1000.0) if t0 is not None and ts is not None else "-"


print("# ABR ladder -- D8 acceptance sequence")
print()
print("Levels: `%s`  ·  dwell: %ss  ·  baseline: %ss  ·  run: `%s`" % (levels_csv, dwell, baseline, out))
print()
print("Overall verdict: **%s**" % summary.get("overall", "UNKNOWN"))
print()
print("time to first step: %s s" % timings.get("time_to_first_step_s", "-"))
print()
print("time to recover: %s s" % timings.get("time_to_recover_s", "-"))
print()
print("Oscillations (pumping) detected: %d" % len(summary.get("oscillations") or []))
print()
print("| level | mark @ t+s | ladder steps in this window |")
print("|---|---|---|")
for i, m in enumerate(marks):
    nxt = marks[i + 1]["ts_unix_ms"] if i + 1 < len(marks) else None
    lo = m["ts_unix_ms"]
    hi = nxt
    cut = [s for s in steps
           if s.get("ts_unix_ms") is not None and s["ts_unix_ms"] >= lo
           and (hi is None or s["ts_unix_ms"] < hi)]
    label = m.get("mark", "?") + (":" + m["level"] if m.get("level") else "")
    print("| %s | %s | %d |" % (label, offset(lo), len(cut)))
print()
print("Full report: `%s/REPORT.md`" % out)
PY

dx_info "ladder report: $OUT/LADDER.md"
dx_result "$TARGET" "out=$OUT" "levels=$LEVELS" "dwell=$DWELL" "total=${TOTAL_S}s"
