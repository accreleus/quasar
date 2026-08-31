#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/session_soak.sh — an on-demand "bad connection" soak for adaptive
# EXTERNAL (stream) resolution. Michael launches a game in his own browser and
# plays it; this is then pointed at that LIVE session and forcibly walks the
# external size DOWN the rung ladder and back UP over a fixed duration,
# capturing telemetry for post-hoc optimisation review.
#
#   scripts/dx/session_soak.sh <SID>            # 180 s ladder walk
#   scripts/dx/session_soak.sh --latest --duration 300 --profile sawtooth
#   HOST=gpu-test scripts/dx/session_soak.sh --latest
#   scripts/dx/session_soak.sh --dry-run --launch 1920x1080   # schedule only
#
#   make session-soak SID=latest ARGS='--duration 180' HOST=gpu-test
#
# Nothing here is wired into ABR. This is a MANUAL degradation simulator: it
# drives PATCH /v1/sessions/{id}/display with stream_width/stream_height only
# and NEVER sends render_width/render_height, because proving the internal
# (app-render) size stays put across an external resize is the point.
#
# Options
#   <SID> | --latest     the session to soak; --latest = newest `running`
#                        session from GET /v1/admin/sessions
#   --duration N         total wall-clock seconds for the whole walk (180)
#   --profile P          ladder (default) | sawtooth | floor | observe
#                          ladder   hold launch, step down rung by rung, hold
#                                   the bottom, step back up, finish at launch
#                          sawtooth launch <-> bottom repeatedly at --dwell
#                          floor    launch, hold the bottom, restore
#                          observe  NO PATCHes at all — hold the launch size and
#                                   record what the host's ABR ladder does by itself
#                                   (the D8 acceptance mode; pair with qnetem)
#   --dwell N            seconds per rung (default: fitted to --duration)
#   --out DIR            output dir (default .diagnostics/soak/<SID8>-<stamp>)
#   --launch WxH         dry-run only: compute the ladder locally, no API at all
#   --rungs W1xH1,...    override the ladder
#   --dry-run            print the computed step schedule and stop
#
# Host resolution is HOST=<role-or-hostname> (default local) via
# dx_resolve_remote()/common.sh against .claude/skills/_shared/hosts.json. The
# admin bearer comes from scripts/dx/admin_token.sh — THE one ladder for the DX
# layer; QSES_ADMIN_TOKEN is still honoured and simply feeds its tier 1. The remote
# path ships scripts/dx/session_soak_driver.py over ONE ssh into `python3 -`:
# a 200 ms echo poll plus a 2 s metrics poll for 180 s is ~1000 requests, which
# has to happen ON the stack host, not as 1000 ssh round trips.
#
# Output (in --out):
#   session.json   the resolved session (launch size, ladder, codec, host)
#   steps.jsonl    one record per step: PATCH code/latency, echo latency,
#                  render readback before and after
#   metrics.jsonl  deduped agent + browser telemetry samples across the run
#   trace.json     GET /v1/admin/sessions/{id}/trace/window for the run, if any
#   verdict.json   GET /v1/admin/sessions/{id}/verdict for the run, if any (ST-09)
#   raw.ndjson     the driver's unsplit output (the source of the four above)
#   summary.json   the analysis (scripts/dx/session_soak_report.py)
#   REPORT.md      the human report: step table, boundary analysis, timeline,
#                  internal-untouched verdict, optimisation candidates
#
# Exit: 0 when the walk ran (a FAILED step is DATA — read REPORT.md, which is
# written even on Ctrl-C); 2 usage; 3 the encoder cannot live-resize.
#
# Ctrl-C is safe: the session is PATCHed back to its launch size and the report
# is still generated.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=session-soak
# Read/PATCH against one session; never touches the STACK (no up/down/rebuild),
# so it rides the "status" scope exactly like scripts/dx/session_display.sh and
# scripts/dx/bundle.sh do.
dx_require_host_scope status

usage() { sed -n '3,55p' "$0" | sed 's/^# \{0,1\}//'; }

# `make session-soak SID=<id>|latest ARGS='--duration 180'` delivers both knobs
# by ENVIRONMENT, not interpolated into the recipe line (#550). This has to run
# BEFORE the defaults below: `SID=""` would shadow the environment value.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" SID ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

SID=""
DURATION=180
PROFILE=ladder
DWELL=""
OUT=""
LAUNCH=""
RUNGS=""
DRY=0

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
    --duration)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--duration requires seconds"
      case "$2" in ''|*[!0-9]*) dx_guard "$TARGET" "--duration must be an integer (got '$2')" ;; esac
      DURATION="$2"; shift 2 ;;
    --profile)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--profile requires ladder|sawtooth|floor|observe"
      case "$2" in ladder|sawtooth|floor|observe) ;; *) dx_guard "$TARGET" "--profile must be ladder|sawtooth|floor|observe (got '$2')" ;; esac
      PROFILE="$2"; shift 2 ;;
    --dwell)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--dwell requires seconds"
      case "$2" in ''|*[!0-9.]*) dx_guard "$TARGET" "--dwell must be numeric (got '$2')" ;; esac
      DWELL="$2"; shift 2 ;;
    --out)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--out requires a directory"
      OUT="$2"; shift 2 ;;
    --launch)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--launch requires WxH"
      case "$2" in *x*) ;; *) dx_guard "$TARGET" "--launch must be WxH (got '$2')" ;; esac
      LAUNCH="$2"; shift 2 ;;
    --rungs)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--rungs requires W1xH1,W2xH2,..."
      RUNGS="$2"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/session_soak.sh --help" ;;
  esac
done

if [ -z "$SID" ] && [ -z "$LAUNCH" ]; then
  dx_guard "$TARGET" "no session — pass a <SID>, or --latest, or (dry-run only) --launch WxH"
fi

dx_have python3 || { dx_fail python3 "not on PATH — the soak driver and report are python3 stdlib"; dx_result "$TARGET"; }

DRIVER="$DX_DIR/session_soak_driver.py"
REPORTER="$DX_DIR/session_soak_report.py"
[ -f "$DRIVER" ] || { dx_fail driver "$DRIVER missing"; dx_result "$TARGET"; }

# ── Driver argv (shared by the local and remote paths) ───────────────────────
DRV=(--duration "$DURATION" --profile "$PROFILE")
if [ "$SID" = "--latest" ]; then DRV+=(--latest)
elif [ -n "$SID" ]; then DRV+=(--sid "$SID"); fi
[ -n "$DWELL" ] && DRV+=(--dwell "$DWELL")
[ -n "$RUNGS" ] && DRV+=(--rungs "$RUNGS")
[ -n "$LAUNCH" ] && DRV+=(--launch "$LAUNCH")
[ "$DRY" = 1 ] && DRV+=(--dry-run)

# ── Offline dry run: no output dir, no stack, no auth. ───────────────────────
if [ "$DRY" = 1 ] && [ -n "$LAUNCH" ]; then
  python3 "$DRIVER" "${DRV[@]}" >/dev/null
  dx_pass schedule "computed offline from --launch $LAUNCH (no API contacted)"
  dx_result "$TARGET" "profile=$PROFILE" "duration=$DURATION"
fi

# ── Output directory ─────────────────────────────────────────────────────────
STAMP="$(dx_timestamp)"
SID_SHORT="$(printf '%s' "${SID:-nosid}" | tr -cd '[:alnum:]-' | cut -c1-8)"
[ -n "$OUT" ] || OUT="$DX_DIAG_DIR/soak/${SID_SHORT}-${STAMP}"
mkdir -p "$OUT"
RAW="$OUT/raw.ndjson"
: > "$RAW"

# ── Split raw.ndjson into the per-kind artefacts, then analyse. ──────────────
split_and_report() {
  python3 - "$RAW" "$OUT" <<'PY'
import json, sys
raw, out = sys.argv[1], sys.argv[2]
steps, metrics, session, trace, verdict = [], [], None, None, None
with open(raw) as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            r = json.loads(line)
        except ValueError:
            continue
        k = r.get("kind")
        if k == "step":
            steps.append(r)
        elif k == "metric":
            metrics.append(r)
        elif k == "session" and session is None:
            session = r
        elif k == "trace":
            trace = r.get("data")
        elif k == "verdict":
            verdict = r.get("data")
def dump_lines(path, rows):
    with open(path, "w") as f:
        for r in rows:
            f.write(json.dumps(r, separators=(",", ":")) + "\n")
dump_lines(out + "/steps.jsonl", steps)
dump_lines(out + "/metrics.jsonl", sorted(metrics, key=lambda m: m.get("ts_unix_ms") or 0))
with open(out + "/session.json", "w") as f:
    json.dump(session or {}, f, indent=2, sort_keys=True)
with open(out + "/trace.json", "w") as f:
    json.dump(trace, f, indent=2, sort_keys=True)
with open(out + "/verdict.json", "w") as f:
    json.dump(verdict, f, indent=2, sort_keys=True)
print("%d steps, %d samples, trace=%s" % (len(steps), len(metrics), "yes" if trace else "no"))
PY
  python3 "$REPORTER" --dir "$OUT" >/dev/null
}

# Restore the launch size from the LOCAL side too. The driver restores on its
# own exit path, but an ssh'd driver never sees a local Ctrl-C (no tty, no
# signal forwarding), so this is the belt to that braces: it reuses
# session_display.sh rather than re-implementing the PATCH.
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
restore_from_raw() {
  local sid wh
  sid="$(python3 - "$RAW" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    try:
        r = json.loads(line)
    except ValueError:
        continue
    if r.get("kind") == "session":
        L = r.get("launch") or []
        if r.get("id") and len(L) == 2:
            print("%s %dx%d" % (r["id"], L[0], L[1]))
        break
PY
)" || return 0
  [ -n "$sid" ] || return 0
  wh="${sid#* }"; sid="${sid%% *}"
  dx_info "restoring $sid to $wh"
  HOST="$DX_HOST" bash "$DX_DIR/session_display.sh" "$sid" --stream "$wh" >/dev/null 2>&1 || true
}

SOAK_DONE=0
DRIVER_PID=""
# shellcheck disable=SC2329  # invoked from the EXIT/INT/TERM trap
on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$SOAK_DONE" != 1 ]; then
    echo >&2
    dx_info "interrupted — stopping the walk, restoring, and writing the report"
    # SIGTERM lands on the driver's own restore-then-exit path (local run). For
    # a remote run this only kills ssh, which is why restore_from_raw below
    # re-issues the restore through session_display.sh regardless.
    if [ -n "$DRIVER_PID" ]; then
      kill -TERM "$DRIVER_PID" 2>/dev/null || true
      wait "$DRIVER_PID" 2>/dev/null || true
    fi
    if [ -s "$RAW" ]; then
      restore_from_raw
      split_and_report || true
      dx_warn soak "interrupted; partial report at $OUT/REPORT.md"
      dx_result "$TARGET" "out=$OUT"
    fi
  fi
  exit "$rc"
}
trap on_exit EXIT INT TERM

# ── Run the driver ───────────────────────────────────────────────────────────
# Backgrounded + `wait`ed deliberately: bash defers a trap until the FOREGROUND
# child returns, so a foreground driver would swallow Ctrl-C for the rest of the
# soak. `wait` is interruptible, so the trap fires immediately.
DRIVER_RC=0
# ONE admin-bearer ladder for the whole DX layer (scripts/dx/admin_token.sh):
# $QUASAR_ADMIN_TOKEN -> cache -> mint on the host (per-boot dev key, else
# BOOTSTRAP_ADMIN_*). The soak used to re-implement the last two tiers inline in
# its remote snippet; it no longer does. QSES_ADMIN_TOKEN stays as the historical
# override name and is fed into tier 1.
SOAK_TOKEN="$(QUASAR_ADMIN_TOKEN="${QUASAR_ADMIN_TOKEN:-${QSES_ADMIN_TOKEN:-}}" \
  bash "$DX_DIR/admin_token.sh" --host "$DX_HOST" --quiet)" || SOAK_TOKEN=""
if [ -z "$SOAK_TOKEN" ]; then
  dx_fail auth "no admin bearer for host=$DX_HOST. Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh (it names every tier it tried)."
  dx_result "$TARGET" "out=$OUT"
fi
# The remote path exports this into a shell snippet as QSOAK_TOKEN='$SOAK_TOKEN'
# — single-quoted, which a `'` in the token would close. Validate before it can
# reach the remote shell.
dx_require_safe "$TARGET" "admin bearer token" "$SOAK_TOKEN" "$DX_RE_TOKEN" \
  "A bearer token is base64url text. Re-mint with: scripts/dx/admin_token.sh --host $DX_HOST --fresh"

if [ "$DX_HOST" = local ]; then
  dx_info "soaking against the local stack (http://127.0.0.1:$DX_CP_PORT)"
  QSOAK_API="http://127.0.0.1:$DX_CP_PORT" \
  QSOAK_TOKEN="$SOAK_TOKEN" \
    python3 "$DRIVER" "${DRV[@]}" >>"$RAW" &
  DRIVER_PID=$!
else
  # Quote every driver arg for the remote shell; bash 3.2 has printf %q.
  RARGS=""
  for a in "${DRV[@]}"; do RARGS="$RARGS $(printf '%q' "$a")"; done
  dx_info "soaking $DX_REMOTE_NAME over ssh (driver runs on the host, one connection)"
  dx_ssh_remote "cd '$DX_REMOTE_DIR'
export QSOAK_API='$DX_REMOTE_API'
export QSOAK_TOKEN='$SOAK_TOKEN'
exec python3 -$RARGS" < "$DRIVER" >>"$RAW" &
  DRIVER_PID=$!
fi
wait "$DRIVER_PID" || DRIVER_RC=$?
DRIVER_PID=""
SOAK_DONE=1

if [ "$DRIVER_RC" = 3 ]; then
  # Deliberate deviation from the common.sh 0|1|2 exit contract: "this encoder
  # cannot live-resize" is a distinct, actionable precondition failure, not a
  # generic harness failure, and a caller scripting the soak needs to tell them
  # apart. The single terminal RESULT line is still emitted.
  dx_fail encoder "stream.external_resize_supported=false — this host's encoder cannot resize a live stream (Vulkan). Set QUASAR_ENCODER=nvenc (NVIDIA) or va (AMD/Intel), redeploy, relaunch the session, then re-run."
  printf 'RESULT status=failed target=%s host=%s instance=%s pass=0 warn=0 fail=1 out=%s reason=external_resize_unsupported\n' \
    "$TARGET" "$DX_HOST" "$QUASAR_INSTANCE" "$OUT"
  exit 3
fi

if [ "$DRY" = 1 ]; then
  dx_pass schedule "printed above; nothing was PATCHed"
  dx_result "$TARGET" "profile=$PROFILE" "duration=$DURATION"
fi

if [ "$DRIVER_RC" != 0 ]; then
  dx_fail driver "soak driver exited $DRIVER_RC (see the messages above; raw output kept at $RAW)"
  [ -s "$RAW" ] && split_and_report || true
  dx_result "$TARGET" "out=$OUT"
fi

VERDICT="$(split_and_report | tail -1)"
[ -f "$OUT/REPORT.md" ] || { dx_fail report "no REPORT.md produced"; dx_result "$TARGET" "out=$OUT"; }
OVERALL="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["overall"])' "$OUT/summary.json" 2>/dev/null || echo UNKNOWN)"

dx_info "$VERDICT"
dx_info "report: $OUT/REPORT.md"
case "$OVERALL" in
  PASS) dx_pass soak "every step echoed; internal untouched" ;;
  DEGRADED) dx_warn soak "walk completed but the internal-untouched check is unproven — see REPORT.md" ;;
  *) dx_fail soak "verdict=$OVERALL — see $OUT/REPORT.md" ;;
esac
dx_result "$TARGET" "out=$OUT" "verdict=$OVERALL" "profile=$PROFILE" "duration=$DURATION"
