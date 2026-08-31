#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/session_display.sh — drive PATCH /v1/sessions/{id}/display against
# a resolved Quasar stack (local, or a fleet host by role/name) and print the
# result plus the session's current stream.external_*/render_* state.
#
#   scripts/dx/session_display.sh <SID> --stream 1280x720
#   scripts/dx/session_display.sh <SID> --render 1920x1080 --ui-scale 1.25
#   HOST=gpu-test scripts/dx/session_display.sh <SID> --stream 1600x900
#
#   make session-display SID=<id> ARGS='--stream 1280x720'
#
# Host resolution follows every other scripts/dx/*.sh script: HOST=<role-or-
# hostname> (default: local), resolved via dx_resolve_remote()/common.sh
# against .claude/skills/_shared/hosts.json — the SAME registry the
# quasar-session `qses` skill reads, so HOST=gpu-test here and
# `qses ... --stack=gpu-test` name the same box.
#
# API (adaptive external resolution spec): PATCH /v1/sessions/{id}/display,
# body is any combination of:
#   {"stream_width":W,"stream_height":H}   external (client-visible) size
#   {"render_width":W,"render_height":H}   internal (app-render) size
#   {"ui_scale":S}                         KWin UI scale
# admin bearer required. 202 = accepted (async apply); 400/409 = rejected.
# GET /v1/sessions/{id} may not yet carry stream.rungs/external_width/height
# (the control-plane task may not have landed) — this script prints "absent"
# for any missing field rather than failing.
#
# Admin credentials:
#   local:  BOOTSTRAP_ADMIN_EMAIL/PASSWORD env (defaults match
#           deploy/overlays/docker-compose.local.yml: admin@local.test / local-dev-admin)
#   remote: read out of deploy/.env on the resolved host over ssh — same
#           source qses's admin_exec() uses (each stack's creds live in ITS
#           OWN deploy/.env; no other host can read them).
#   QSES_ADMIN_TOKEN (env, either case): a pre-minted admin bearer OVERRIDES
#           the BOOTSTRAP_ADMIN login entirely — same override qses's
#           admin_exec() honors. Needed when the stack has no BOOTSTRAP_ADMIN
#           creds in deploy/.env (e.g. the password was rotated post-deploy),
#           and it's what `qses matrix` passes through so the per-rung PATCH
#           reuses the SAME token its signaling-token reconnect already
#           authenticated with, instead of a second (and on such a stack,
#           failing) login.
#
# Exit: 0 once the PATCH + GET round-trip completed, REGARDLESS of the PATCH's
# own HTTP code (a documented 400/409 is a valid harness observation, not a
# script failure — print it and let the caller judge). Exit 1 only on a
# harness/transport failure (login failed, host unreachable, bad JSON) — on
# any such failure the exact curl invocation that failed is printed to
# stderr (bearer token / password redacted) so it can be re-run by hand.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=session-display
# Reads/writes against a resolved host but never rebuilds/ups/downs/restarts
# the STACK itself — reuse the "status" verb the way scripts/dx/bundle.sh does
# for diagnose-bundle, so this is allowed for local AND remote without forcing
# an explicit HOST= (it PATCHes one session, not the stack).
dx_require_host_scope status

# `make session-display SID=<id> ARGS='--stream 1280x720'` delivers both knobs by
# ENVIRONMENT, not interpolated into the recipe line (#550). SID first, because
# that is the positional this script expects.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" SID ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

usage() { sed -n '3,29p' "$0" | sed 's/^# \{0,1\}//'; }

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  usage; exit 0
fi
if [ $# -lt 1 ]; then
  usage >&2
  echo "usage: session_display.sh <SID> [--stream WxH] [--render WxH] [--ui-scale S]" >&2
  exit 2
fi
SID="$1"; shift
[ -n "$SID" ] || dx_guard "$TARGET" "SID is empty — usage: session_display.sh <SID> [--stream WxH] [--render WxH] [--ui-scale S]"

REPLY_W="" REPLY_H=""
parse_wh() { # $1 = "WxH" -> sets REPLY_W REPLY_H; returns 1 on malformed input
  case "$1" in
    *x*)
      REPLY_W="${1%%x*}"; REPLY_H="${1#*x}"
      case "$REPLY_W" in ''|*[!0-9]*) return 1 ;; esac
      case "$REPLY_H" in ''|*[!0-9]*) return 1 ;; esac
      ;;
    *) return 1 ;;
  esac
}

STREAM_W="" STREAM_H="" RENDER_W="" RENDER_H="" UI_SCALE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --stream)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--stream requires WxH"
      parse_wh "$2" || dx_guard "$TARGET" "--stream must be WxH (got '$2')"
      STREAM_W="$REPLY_W"; STREAM_H="$REPLY_H"; shift 2 ;;
    --render)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--render requires WxH"
      parse_wh "$2" || dx_guard "$TARGET" "--render must be WxH (got '$2')"
      RENDER_W="$REPLY_W"; RENDER_H="$REPLY_H"; shift 2 ;;
    --ui-scale)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--ui-scale requires a value"
      case "$2" in ''|*[!0-9.]*) dx_guard "$TARGET" "--ui-scale must be numeric (got '$2')" ;; esac
      UI_SCALE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/session_display.sh --help" ;;
  esac
done

[ -n "$STREAM_W$RENDER_W$UI_SCALE" ] || dx_guard "$TARGET" \
  "nothing to do — pass at least one of --stream WxH / --render WxH / --ui-scale S"

dx_have python3 || { dx_fail python3 "not on PATH — needed to build the PATCH body and parse responses"; dx_result "$TARGET"; }
dx_have curl || { dx_fail curl "not on PATH"; dx_result "$TARGET"; }

BODY="$(python3 - "$STREAM_W" "$STREAM_H" "$RENDER_W" "$RENDER_H" "$UI_SCALE" <<'PY'
import json, sys
sw, sh, rw, rh, scale = sys.argv[1:6]
d = {}
if sw:
    d["stream_width"] = int(sw)
    d["stream_height"] = int(sh)
if rw:
    d["render_width"] = int(rw)
    d["render_height"] = int(rh)
if scale:
    d["ui_scale"] = float(scale)
print(json.dumps(d))
PY
)"

print_get_summary() { # $1 = raw GET body (may be malformed/empty)
  python3 -c '
import json, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw).get("session", {})
except Exception as e:
    print(f"GET session parse failed: {e}")
    sys.exit(0)
s = d.get("stream", {}) or {}
def g(k, alt=None):
    v = s.get(k, s.get(alt) if alt else None)
    return v if v is not None else "absent"
print("stream.external: %sx%s" % (g("external_width"), g("external_height")))
print("stream.render:   %sx%s" % (g("render_width", "width"), g("render_height", "height")))
print("stream.rungs:    %s" % g("rungs"))
print("ui_scale:        %s" % (d.get("ui_scale", "absent")))
' <<<"$1"
}

print_curl_debug() { # $1 = label, $2.. = curl argv (as literal words to echo)
  {
    printf 'CURL (%s): curl' "$1"; shift
    for a in "$@"; do printf ' %q' "$a"; done
    printf '\n'
  } >&2
}

run_patch() {
  # ONE path for local and fleet stacks: the bearer comes from THE ladder
  # (scripts/dx/admin_token.sh — $QUASAR_ADMIN_TOKEN → cache → mint on the
  # host) and the PATCH/GET are issued from this workstation against the
  # stack's external URL, exactly as `make session-*` does. This replaced a
  # local BOOTSTRAP login plus a remote ssh snippet that sourced deploy/.env
  # and logged in with the bootstrap password — which 401s the moment that
  # password is rotated (devbox, 2026-08-23).
  local base resp_file hdr_file code token get_body
  if [ "$DX_HOST" = local ]; then
    base="http://127.0.0.1:${DX_CP_PORT}"
  else
    base="${DX_REMOTE_API_EXTERNAL:-$DX_REMOTE_API}"
  fi
  base="${base%/}"
  token="$(QUASAR_ADMIN_TOKEN="${QUASAR_ADMIN_TOKEN:-${QSES_ADMIN_TOKEN:-}}" \
    bash "$DX_DIR/admin_token.sh" --host "$DX_HOST" --quiet)" || token=""
  [ -n "$token" ] || { dx_fail login "no admin bearer for host=$DX_HOST (the ladder printed every tier it tried above). Next: scripts/dx/admin_token.sh --host $DX_HOST --fresh"; dx_result "$TARGET"; }

  hdr_file="$(mktemp)"; chmod 600 "$hdr_file"
  printf 'Authorization: Bearer %s\n' "$token" > "$hdr_file"

  resp_file="$(mktemp)"
  code="$(curl -sS -k -o "$resp_file" -w '%{http_code}' --max-time 15 \
    -X PATCH "$base/v1/sessions/$SID/display" -H @"$hdr_file" -H 'Content-Type: application/json' \
    -d "$BODY" 2>/dev/null || true)"
  case "$code" in
    ''|000|*[!0-9]*)
      print_curl_debug patch-transport-failure -sS -k -X PATCH "$base/v1/sessions/$SID/display" \
        -H 'Authorization: Bearer [REDACTED]' -H 'Content-Type: application/json' -d "$BODY" ;;
  esac
  echo "HTTP $code $(cat "$resp_file")"
  rm -f "$resp_file"

  get_body="$(curl -sS -k --max-time 15 "$base/v1/sessions/$SID" -H @"$hdr_file" 2>/dev/null || true)"
  rm -f "$hdr_file"
  print_get_summary "$get_body"
  case "$code" in
    2*) ;;
    *) dx_fail patch "PATCH /v1/sessions/$SID/display -> HTTP $code"; dx_result "$TARGET" ;;
  esac
}

run_patch

dx_pass patch "PATCH /v1/sessions/$SID/display issued (see HTTP line above for the outcome)"
dx_result "$TARGET" "sid=$SID"
