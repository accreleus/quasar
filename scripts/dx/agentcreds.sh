#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/agentcreds.sh — mint a throwaway dev-agent identity (#399) via
# POST /v1/dev/agent-session. Universal DX verb: no Claude-specific
# assumptions, usable by any dev in any environment that has a stack running
# with QUASAR_DEV_AGENT_AUTH=1.
#
#   scripts/dx/agentcreds.sh [--role user|admin] [--ttl <seconds|30m|2h>]
#                            [--url <control-plane base>] [--key <dev key>]
#                            [--storage-state <file>]
#
# Key resolution order:
#   1. --key
#   2. $QUASAR_DEV_AGENT_KEY
#   3. (local stack only) docker exec into this instance's control-plane
#      container for /run/quasar/dev-agent-key
# If none resolve, this fails with a clear message: the target stack needs
# QUASAR_DEV_AGENT_AUTH=1 set (see docs/configuration.md). A remote/non-local
# stack (e.g. Tower) has no local docker to exec into — fetch its key with
# `ssh <host> 'docker compose exec -T quasar-control-plane cat /run/quasar/dev-agent-key'`
# (or the stack's own compose service name) and pass it via --key or
# $QUASAR_DEV_AGENT_KEY.
#
# --url defaults to this worktree's local instance (like other dx scripts),
# falling back to http://localhost:8080 if that is somehow unset.
#
# Default output: the `storage_keys` object from the response, as JSON on
# stdout (pretty-printed with jq when available, compact otherwise). Nothing
# else goes to stdout, so the output is safe to pipe/capture. All PASS/WARN/
# FAIL/RESULT status lines go to stderr for this script specifically (unlike
# most dx/*.sh, whose stdout is not meant to be captured as data).
#
# --storage-state <file>: instead of printing to stdout, writes a Playwright
# storageState JSON file: {"cookies":[],"origins":[{"origin":"<url origin>",
# "localStorage":[{"name":...,"value":...}, ...]}]}.
#
# NEVER echoes the dev key or the minted access token to any log line.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=agent-creds

# `make agent-creds ARGS='--role admin --ttl 1h'` delivers ARGS by ENVIRONMENT,
# not interpolated into the recipe line (#550) — dx_env_argv splits and checks
# it. Arguments passed directly on the command line always win.
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

usage() {
  sed -n '3,32p' "$0" | sed 's/^# \{0,1\}//'
}

ROLE=user
TTL_RAW=30m
URL=""
KEY="${QUASAR_DEV_AGENT_KEY:-}"
STORAGE_STATE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --role)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--role requires a value (user|admin)"
      ROLE="$2"; shift 2 ;;
    --ttl)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--ttl requires a value (seconds or 30m/2h style)"
      TTL_RAW="$2"; shift 2 ;;
    --url)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--url requires a value"
      URL="$2"; shift 2 ;;
    --key)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--key requires a value"
      KEY="$2"; shift 2 ;;
    --storage-state)
      [ $# -ge 2 ] && [ -n "$2" ] || dx_guard "$TARGET" "--storage-state requires a path"
      STORAGE_STATE="$2"; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/agentcreds.sh --help" ;;
  esac
done

case "$ROLE" in
  user|admin) ;;
  *) dx_guard "$TARGET" "--role must be 'user' or 'admin' (got '$ROLE')" ;;
esac

# --ttl: bare integer seconds, or Nh / Nm / Ns shorthand. bash 3.2 compatible
# (no associative arrays, no ${var,,}) — plain case/parameter-expansion only.
ttl_to_seconds() {
  local raw="$1" num
  case "$raw" in
    '') return 1 ;;
    *[!0-9hms]*) return 1 ;;
  esac
  case "$raw" in
    *h)
      num="${raw%h}"
      case "$num" in ''|*[!0-9]*) return 1 ;; esac
      echo $(( num * 3600 )) ;;
    *m)
      num="${raw%m}"
      case "$num" in ''|*[!0-9]*) return 1 ;; esac
      echo $(( num * 60 )) ;;
    *s)
      num="${raw%s}"
      case "$num" in ''|*[!0-9]*) return 1 ;; esac
      echo "$num" ;;
    *[0-9])
      echo "$raw" ;;
    *) return 1 ;;
  esac
}

TTL_SECONDS="$(ttl_to_seconds "$TTL_RAW")" || dx_guard "$TARGET" \
  "--ttl must be seconds or Nh/Nm/Ns (got '$TTL_RAW')"
case "$TTL_SECONDS" in ''|*[!0-9]*) dx_guard "$TARGET" "--ttl resolved to a non-numeric value" ;; esac
[ "$TTL_SECONDS" -ge 60 ] || dx_guard "$TARGET" "--ttl must be at least 60 seconds (got ${TTL_SECONDS}s)"
[ "$TTL_SECONDS" -le 28800 ] || dx_guard "$TARGET" "--ttl must be at most 28800 seconds / 8h (got ${TTL_SECONDS}s)"

if [ -z "$URL" ]; then
  if [ -n "${DX_CP_PORT:-}" ]; then
    URL="http://127.0.0.1:${DX_CP_PORT}"
  else
    URL="http://localhost:8080"
  fi
fi
URL="${URL%/}"

# Key resolution: --key / $QUASAR_DEV_AGENT_KEY already captured in $KEY.
if [ -z "$KEY" ] && [ "$DX_HOST" = "local" ] && dx_have docker && docker info >/dev/null 2>&1; then
  KEY="$(dx_local_compose exec -T control-plane cat /run/quasar/dev-agent-key 2>/dev/null | tr -d '\r\n')" || KEY=""
fi

if [ -z "$KEY" ]; then
  dx_fail key "no dev key found — pass --key, set \$QUASAR_DEV_AGENT_KEY, or run this against a local stack booted with QUASAR_DEV_AGENT_AUTH=1 (the key is then read from the control-plane container's /run/quasar/dev-agent-key). See docs/configuration.md." >&2
  dx_result "$TARGET" >&2
fi

if [ "$ROLE" = admin ]; then
  dx_warn role "minting role=admin — the control plane logs this at WARN with the request source" >&2
fi

dx_have curl || { dx_fail curl "not on PATH — cannot call $URL/v1/dev/agent-session" >&2; dx_result "$TARGET" >&2; }

BODY="{\"role\":\"$ROLE\",\"ttl_seconds\":$TTL_SECONDS}"
RESP_FILE="$(mktemp)"
# The dev key goes to curl via a header FILE (`-H @file`, curl >=7.55 — macOS
# ships newer), never in argv: any local user can read another user's argv via
# `ps`, and a literal `-H "X-Quasar-Dev-Key: $KEY"` would put the secret there
# for the life of the curl process.
HDR_FILE="$(mktemp)"
chmod 600 "$HDR_FILE"
printf 'X-Quasar-Dev-Key: %s\n' "$KEY" > "$HDR_FILE"
trap 'rm -f "$RESP_FILE" "$HDR_FILE"' EXIT

HTTP_CODE="$(curl -sS -k -o "$RESP_FILE" -w '%{http_code}' --max-time 10 \
  -X POST "$URL/v1/dev/agent-session" \
  -H @"$HDR_FILE" \
  -H 'Content-Type: application/json' \
  -d "$BODY" 2>/dev/null || true)"

case "$HTTP_CODE" in
  200) ;;
  404)
    dx_fail request "404 from $URL/v1/dev/agent-session — the route is not registered on that stack; set QUASAR_DEV_AGENT_AUTH=1 and restart it" >&2
    dx_result "$TARGET" >&2 ;;
  401)
    dx_fail request "401 from $URL/v1/dev/agent-session — the dev key is wrong or stale (it is per-boot; re-fetch it rather than reusing an old one)" >&2
    dx_result "$TARGET" >&2 ;;
  ""|000)
    dx_fail request "no response from $URL/v1/dev/agent-session — is the stack up? (make up / make status)" >&2
    dx_result "$TARGET" >&2 ;;
  *)
    dx_fail request "HTTP $HTTP_CODE from $URL/v1/dev/agent-session" >&2
    dx_result "$TARGET" >&2 ;;
esac

dx_pass request "$URL/v1/dev/agent-session -> 200 (role=$ROLE ttl=${TTL_SECONDS}s)" >&2

if ! dx_have python3; then
  dx_fail python3 "not on PATH — needed to parse the response and never echoes the token to a log" >&2
  dx_result "$TARGET" >&2
fi

if [ -n "$STORAGE_STATE" ]; then
  # Create the file with 0600 BEFORE writing it — it holds a live bearer
  # token. python's open(path, "w") truncates an existing file without
  # touching its mode, but a fresh file would otherwise inherit the process
  # umask (often 022 -> world-readable). The subshell umask keeps this
  # atomic: no window where the file exists at the default mode.
  if ! ( umask 077; : > "$STORAGE_STATE" ); then
    dx_fail storage-state "could not create $STORAGE_STATE" >&2
  fi
  # umask only governs CREATION — a pre-existing world-readable file keeps its
  # mode through the truncate, so force it. The file holds a live bearer token.
  if ! chmod 600 "$STORAGE_STATE"; then
    dx_fail storage-state "could not chmod 600 $STORAGE_STATE" >&2
    dx_result "$TARGET" >&2
  fi
  if python3 - "$RESP_FILE" "$URL" "$STORAGE_STATE" <<'PY'
import json, sys
from urllib.parse import urlsplit

resp_path, url, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
with open(resp_path, encoding="utf-8") as f:
    data = json.load(f)
storage_keys = data["storage_keys"]
origin_parts = urlsplit(url)
origin = "%s://%s" % (origin_parts.scheme, origin_parts.netloc)
state = {
    "cookies": [],
    "origins": [
        {
            "origin": origin,
            "localStorage": [
                {"name": name, "value": value} for name, value in storage_keys.items()
            ],
        }
    ],
}
with open(out_path, "w", encoding="utf-8") as f:
    json.dump(state, f)
PY
  then
    dx_pass storage-state "written to $STORAGE_STATE" >&2
  else
    dx_fail storage-state "failed to write $STORAGE_STATE" >&2
  fi
  dx_result "$TARGET" >&2
fi

# Default output: storage_keys JSON on stdout, and stdout only.
STORAGE_KEYS_JSON="$(python3 - "$RESP_FILE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    data = json.load(f)
print(json.dumps(data["storage_keys"]))
PY
)"

if dx_have jq; then
  printf '%s' "$STORAGE_KEYS_JSON" | jq '.'
else
  printf '%s\n' "$STORAGE_KEYS_JSON"
fi

dx_result "$TARGET" >&2
