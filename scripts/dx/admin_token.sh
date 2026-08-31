#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/admin_token.sh — THE admin-bearer ladder for the Quasar DX layer.
#
# Prints ONE admin bearer token on stdout. Every diagnostic message goes to
# stderr, so `TOK="$(scripts/dx/admin_token.sh --host devbox)"` is always safe.
#
#   scripts/dx/admin_token.sh [--host <role|name>] [--ttl 1h] [--fresh] [--quiet]
#
#   --host   role or host name from .claude/skills/_shared/hosts.json.
#            Defaults to $HOST / $QUASAR_DEFAULT_HOST / "local".
#   --ttl    requested token lifetime (seconds, or Nm / Nh). Default 1h.
#   --fresh  ignore (and overwrite) the cached token.
#   --quiet  suppress the per-tier progress notes on stderr. Errors still print.
#
# THE LADDER (first hit wins):
#   1. $QUASAR_ADMIN_TOKEN — always wins, never cached. This is how a caller
#      pins the identity that also LAUNCHED a session, which owner-gated verbs
#      need.
#   2. A cached, still-valid token in
#      ${XDG_CACHE_HOME:-~/.cache}/quasar/<host>.token (0600, stores the token
#      and its expiry epoch). "Valid" = more than 60 s of life left.
#   3. Mint one on the host:
#        a. the control-plane container's per-boot dev key
#           (/run/quasar/dev-agent-key) -> POST /v1/dev/agent-session
#           {"role":"admin"} — needs QUASAR_DEV_AGENT_AUTH=1 on that stack.
#        b. the BOOTSTRAP_ADMIN_* pair from that host's own <dir>/deploy/.env
#           -> POST /v1/auth/login.
#      Tier 3 runs over ONE ssh hop and uses the host's OWN local API url
#      (hosts.json `api`), so no credential ever crosses the network. With
#      HOST=local there is no ssh: the key is read from this worktree's own
#      compose stack and the API is http://127.0.0.1:$DX_CP_PORT.
#
# An empty result is EXIT 2 with a message that enumerates every tier tried and
# names the next command. It is never "carry on and see": a request with
# `Authorization: Bearer ` is a 401, and a verb that then reports success has
# told you the opposite of the truth.
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TTL_RAW=1h
FRESH=0
QUIET=0
HOST_ARG=""

usage() { awk 'NR>3 && !/^#/ {exit} NR>3 {sub(/^# ?/,""); print}' "${BASH_SOURCE[0]}" >&2; }

# `make admin-token ARGS='--fresh --ttl 2h'` delivers ARGS by ENVIRONMENT, not
# interpolated into the recipe line (#550). HOST already travelled that way —
# the recipe's old `--host "$(HOST)"` was redundant with the $HOST default
# documented above, and is gone.
[ $# -gt 0 ] || { dx_env_argv admin-token ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

while [ $# -gt 0 ]; do
  case "$1" in
    --host) [ $# -ge 2 ] || { echo "admin_token.sh: --host requires a value" >&2; exit 3; }
            HOST_ARG="$2"; shift 2 ;;
    --host=*) HOST_ARG="${1#*=}"; shift ;;
    --ttl)  [ $# -ge 2 ] || { echo "admin_token.sh: --ttl requires a value" >&2; exit 3; }
            TTL_RAW="$2"; shift 2 ;;
    --ttl=*) TTL_RAW="${1#*=}"; shift ;;
    --fresh) FRESH=1; shift ;;
    --quiet) QUIET=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "admin_token.sh: unknown arg '$1' (see --help)" >&2; exit 3 ;;
  esac
done

# Under `set -e`, a trailing `[ ... ] && x` that tests false kills an if-block
# or a function silently. Always an `if`.
if [ -n "$HOST_ARG" ]; then DX_HOST="$HOST_ARG"; fi
export DX_HOST

note() { [ "$QUIET" = 1 ] || printf 'admin-token: %s\n' "$*" >&2; }
die2() { printf 'admin-token: %s\n' "$*" >&2; exit 2; }

# ── TTL ──────────────────────────────────────────────────────────────────────
ttl_to_seconds() {
  local raw="$1" num
  case "$raw" in
    '') return 1 ;;
    *h) num="${raw%h}"; case "$num" in ''|*[!0-9]*) return 1 ;; esac; echo $(( num * 3600 )) ;;
    *m) num="${raw%m}"; case "$num" in ''|*[!0-9]*) return 1 ;; esac; echo $(( num * 60 )) ;;
    *s) num="${raw%s}"; case "$num" in ''|*[!0-9]*) return 1 ;; esac; echo "$num" ;;
    *[0-9]) case "$raw" in *[!0-9]*) return 1 ;; esac; echo "$raw" ;;
    *) return 1 ;;
  esac
}
TTL_SECONDS="$(ttl_to_seconds "$TTL_RAW")" || {
  echo "admin_token.sh: --ttl must be seconds or Nh/Nm/Ns (got '$TTL_RAW')" >&2; exit 3; }
[ "$TTL_SECONDS" -ge 60 ] || { echo "admin_token.sh: --ttl must be at least 60s" >&2; exit 3; }
[ "$TTL_SECONDS" -le 28800 ] || { echo "admin_token.sh: --ttl must be at most 28800s (8h)" >&2; exit 3; }

# ── Tier 1: the explicit override ────────────────────────────────────────────
if [ -n "${QUASAR_ADMIN_TOKEN:-}" ]; then
  note "tier 1: \$QUASAR_ADMIN_TOKEN (not cached)"
  printf '%s\n' "$QUASAR_ADMIN_TOKEN"
  exit 0
fi

# ── Host resolution ──────────────────────────────────────────────────────────
HOST_NAME="local"
HOST_DIR=""
HOST_API=""
if [ "$DX_HOST" != "local" ]; then
  dx_resolve_remote "$DX_HOST" || die2 \
    "HOST='$DX_HOST' is not a known role or host. Next: check .claude/skills/_shared/hosts.json, or pass --host <role|name>."
  HOST_NAME="$DX_REMOTE_NAME"
  HOST_DIR="$DX_REMOTE_DIR"
  HOST_API="$DX_REMOTE_API"
  [ -n "$HOST_API" ] || die2 \
    "host '$HOST_NAME' has no \`api\` (host-local URL) in .claude/skills/_shared/hosts.json. Next: add it."
fi

# ── Tier 2: the cache ────────────────────────────────────────────────────────
CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/quasar"
CACHE_FILE="$CACHE_DIR/${HOST_NAME}.token"
NOW="$(date -u +%s)"

if [ "$FRESH" = 1 ]; then
  note "--fresh: ignoring any cached token for $HOST_NAME"
  rm -f "$CACHE_FILE" 2>/dev/null || true
elif [ -f "$CACHE_FILE" ]; then
  CACHED_EXP="$(sed -n '1p' "$CACHE_FILE" 2>/dev/null || true)"
  CACHED_TOK="$(sed -n '2p' "$CACHE_FILE" 2>/dev/null || true)"
  case "$CACHED_EXP" in
    ''|*[!0-9]*) CACHED_EXP=0 ;;
  esac
  if [ -n "$CACHED_TOK" ] && [ "$(( CACHED_EXP - NOW ))" -gt 60 ]; then
    note "tier 2: cached token for $HOST_NAME ($(( CACHED_EXP - NOW ))s left) — $CACHE_FILE"
    printf '%s\n' "$CACHED_TOK"
    exit 0
  fi
  note "tier 2: cached token for $HOST_NAME is expired or empty — re-minting"
fi

# ── Tier 3: mint one ─────────────────────────────────────────────────────────
# The snippet below runs EITHER on the remote host (over one ssh hop) or, for
# HOST=local, in this shell. It prints the token on stdout and one `tier3=<how>`
# line on stderr; anything else is noise the caller must not confuse for a token.
mint_snippet() { # $1 = api url, $2 = repo dir ('' = skip the .env tier)
  cat <<EOS
API='$1'
QDIR='$2'
TOK=''
# Preflight: a stack that is DOWN must be reported as down, not as "no
# credential" — on 2026-08-23 a stopped tower stack read as a rotated password.
HC=\$(curl -k -s -o /dev/null -w '%{http_code}' --max-time 8 "\$API/health" 2>/dev/null); [ -n "\$HC" ] || HC=000
if [ "\$HC" != 200 ]; then echo "stack=down http=\$HC" >&2; printf '\n'; exit 0; fi
CPC=\$(docker ps --filter name=control-plane --format '{{.Names}}' 2>/dev/null | head -n 1)
if [ -n "\$CPC" ]; then
  DEVKEY=\$(docker exec "\$CPC" cat /run/quasar/dev-agent-key 2>/dev/null | tr -d '\r\n' || true)
  if [ -n "\$DEVKEY" ]; then
    TOK=\$(curl -k -fs --max-time 10 -X POST "\$API/v1/dev/agent-session" \\
      -H 'Content-Type: application/json' -H "X-Quasar-Dev-Key: \$DEVKEY" \\
      -d '{"role":"admin","ttl_seconds":$TTL_SECONDS}' 2>/dev/null \\
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token") or "")' 2>/dev/null || true)
    [ -n "\$TOK" ] && echo 'tier3=dev-agent-key' >&2
  fi
fi
if [ -z "\$TOK" ] && [ -n "\$QDIR" ] && [ -f "\$QDIR/deploy/.env" ]; then
  set -a; . "\$QDIR/deploy/.env" 2>/dev/null || true; set +a
  if [ -n "\${BOOTSTRAP_ADMIN_EMAIL:-}" ] && [ -n "\${BOOTSTRAP_ADMIN_PASSWORD:-}" ]; then
    TOK=\$(curl -k -fs --max-time 10 -X POST "\$API/v1/auth/login" \\
      -H 'Content-Type: application/json' \\
      -d "{\\"email\\":\\"\$BOOTSTRAP_ADMIN_EMAIL\\",\\"password\\":\\"\$BOOTSTRAP_ADMIN_PASSWORD\\"}" 2>/dev/null \\
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token") or "")' 2>/dev/null || true)
    [ -n "\$TOK" ] && echo 'tier3=bootstrap-admin-login' >&2
  fi
fi
printf '%s\n' "\$TOK"
EOS
}

MINT_ERR="$(mktemp)"
trap 'rm -f "$MINT_ERR"' EXIT

if [ "$DX_HOST" = "local" ]; then
  # No ssh. The ephemeral compose stack for THIS worktree, on DX_CP_PORT, with
  # the dev key read out of its own control-plane container (agentcreds.sh's
  # local path). deploy/.env in the worktree is the second tier.
  note "tier 3: minting against the local stack (http://127.0.0.1:$DX_CP_PORT)"
  LOCAL_API="http://127.0.0.1:${DX_CP_PORT}"
  LOCAL_KEY=""
  if dx_have docker && docker info >/dev/null 2>&1; then
    LOCAL_KEY="$(dx_local_compose exec -T control-plane cat /run/quasar/dev-agent-key 2>/dev/null | tr -d '\r\n')" || LOCAL_KEY=""
  fi
  TOKEN=""
  if [ -n "$LOCAL_KEY" ]; then
    TOKEN="$(curl -k -fs --max-time 10 -X POST "$LOCAL_API/v1/dev/agent-session" \
      -H 'Content-Type: application/json' -H "X-Quasar-Dev-Key: $LOCAL_KEY" \
      -d "{\"role\":\"admin\",\"ttl_seconds\":$TTL_SECONDS}" 2>/dev/null \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token") or "")' 2>/dev/null || true)"
    if [ -n "$TOKEN" ]; then note "tier 3a: local dev-agent key -> POST /v1/dev/agent-session"; fi
  fi
  if [ -z "$TOKEN" ] && [ -f "$DX_ROOT/deploy/.env" ]; then
    TOKEN="$(
      set -a; . "$DX_ROOT/deploy/.env" 2>/dev/null || true; set +a
      if [ -n "${BOOTSTRAP_ADMIN_EMAIL:-}" ] && [ -n "${BOOTSTRAP_ADMIN_PASSWORD:-}" ]; then
        curl -k -fs --max-time 10 -X POST "$LOCAL_API/v1/auth/login" \
          -H 'Content-Type: application/json' \
          -d "{\"email\":\"$BOOTSTRAP_ADMIN_EMAIL\",\"password\":\"$BOOTSTRAP_ADMIN_PASSWORD\"}" 2>/dev/null \
          | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token") or "")' 2>/dev/null || true
      fi
    )"
    if [ -n "$TOKEN" ]; then note "tier 3b: deploy/.env BOOTSTRAP_ADMIN_* -> POST /v1/auth/login"; fi
  fi
  NEXT_HINT="Next, pick ONE: export QUASAR_ADMIN_TOKEN=<bearer>; or bring the local stack up with QUASAR_DEV_AGENT_AUTH=1 (make up) so /run/quasar/dev-agent-key exists; or set BOOTSTRAP_ADMIN_EMAIL/BOOTSTRAP_ADMIN_PASSWORD in $DX_ROOT/deploy/.env."
else
  note "tier 3: minting on $HOST_NAME over ssh ($HOST_API)"
  TOKEN="$(dx_ssh_remote "$(mint_snippet "$HOST_API" "$HOST_DIR")" 2>"$MINT_ERR" | tail -n 1 | tr -d '\r')" || TOKEN=""
  if [ "$QUIET" != 1 ] && [ -s "$MINT_ERR" ]; then
    grep -E '^tier3=' "$MINT_ERR" | sed 's/^/admin-token: tier 3 route: /' >&2 || true
  fi
  if grep -q '^stack=down' "$MINT_ERR" 2>/dev/null; then
    printf 'admin-token: the control plane on %s is DOWN (%s returned %s) — no credential tier can mint against it.\n  Next: make status HOST=%s (or bring the stack up), then retry.\n' \
      "$HOST_NAME" "$HOST_API/health" "$(sed -n 's/^stack=down http=//p' "$MINT_ERR" | head -n1)" "$DX_HOST" >&2
    exit 2
  fi
  NEXT_HINT="Next, pick ONE: export QUASAR_ADMIN_TOKEN=<bearer>; or set QUASAR_DEV_AGENT_AUTH=1 on $HOST_NAME and restart its control-plane (the per-boot key then appears at /run/quasar/dev-agent-key); or set BOOTSTRAP_ADMIN_EMAIL/BOOTSTRAP_ADMIN_PASSWORD in $HOST_DIR/deploy/.env on $HOST_NAME."
fi

if [ -z "${TOKEN:-}" ]; then
  {
    printf 'admin-token: no admin bearer could be obtained for host=%s.\n' "$HOST_NAME"
    printf '  tier 1  $QUASAR_ADMIN_TOKEN            — unset or empty\n'
    printf '  tier 2  %s — absent or expired\n' "$CACHE_FILE"
    printf '  tier 3a per-boot dev key -> POST /v1/dev/agent-session   — no key, or the route is off (needs QUASAR_DEV_AGENT_AUTH=1)\n'
    printf '  tier 3b BOOTSTRAP_ADMIN_* -> POST /v1/auth/login          — not in deploy/.env, or the password was rotated\n'
    if [ -s "$MINT_ERR" ]; then
      printf '  ssh/mint stderr: %s\n' "$(tr '\n' ' ' < "$MINT_ERR" | cut -c1-400)"
    fi
    printf '%s\n' "  $NEXT_HINT"
  } >&2
  exit 2
fi

# ── Cache it ─────────────────────────────────────────────────────────────────
# The expiry we record is the REQUESTED ttl minus a small margin. A bootstrap
# login's real ttl is the control plane's, not ours, so the cache is a hint and
# a 401 downstream is always answered by `--fresh`, never by trusting this file.
if mkdir -p "$CACHE_DIR" 2>/dev/null; then
  chmod 700 "$CACHE_DIR" 2>/dev/null || true
  if ( umask 077; printf '%s\n%s\n' "$(( NOW + TTL_SECONDS - 30 ))" "$TOKEN" > "$CACHE_FILE" ); then
    chmod 600 "$CACHE_FILE" 2>/dev/null || true
    note "cached to $CACHE_FILE (expires in $(( TTL_SECONDS - 30 ))s)"
  fi
fi

printf '%s\n' "$TOKEN"
