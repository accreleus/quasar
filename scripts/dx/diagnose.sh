#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/diagnose.sh — one page that answers "what state am I actually in?"
#
#   make diagnose            (local)
#   make diagnose HOST=gpu-test (read-only view of the fleet box)
#
# Everything is bounded: this is meant to be pasted into an issue or read by an
# agent, not to dump a log file into a context window. For an archivable
# artifact use `make diagnose-bundle` (scripts/dx/bundle.sh).
#
# Output passes through scripts/dx/redact.sh wherever it can carry values.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_host_scope status

TARGET=diagnose
REDACT="$DX_DIR/redact.sh"
ERR_LINES="${DX_ERR_LINES:-20}"
# Reaches a remote `... | tail -n $ERR_LINES` UNQUOTED — same class as
# DX_BUNDLE_TAIL: no escape needed to inject.
dx_require_safe "$TARGET" "DX_ERR_LINES" "$ERR_LINES" "$DX_RE_UINT" "It is a line count."

section() { printf '\n== %s ==\n' "$1"; }

# ── git ──────────────────────────────────────────────────────────────────────
section "git"
if dx_have git && git -C "$DX_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  printf 'branch:   %s\n' "$(git -C "$DX_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
  printf 'rev:      %s\n' "$(git -C "$DX_ROOT" rev-parse --short HEAD 2>/dev/null || echo '?')"
  dirty_n="$(git -C "$DX_ROOT" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
  printf 'dirty:    %s file(s)\n' "$dirty_n"
  if [ -f "$DX_ROOT/protocol/openapi.yaml" ]; then
    printf 'protocol: initialized\n'
  else
    printf 'protocol: NOT initialized (TestOpenAPIDrift will fail)\n'
  fi
  dx_pass git "branch + rev captured"
else
  dx_warn git "not a git worktree, or git is missing"
fi

# ── instance ─────────────────────────────────────────────────────────────────
section "instance"
printf 'root:     %s\n' "$DX_ROOT"
printf 'instance: %s\n' "$QUASAR_INSTANCE"
printf 'host:     %s\n' "$DX_HOST"
printf 'ports:    cp=%s tls=%s pg=%s\n' "$DX_CP_PORT" "$DX_CP_TLS_PORT" "$DX_PG_PORT"
dx_pass instance "$QUASAR_INSTANCE"

# ── tool versions ────────────────────────────────────────────────────────────
section "versions"
for tool in docker go node npm cargo rustc git curl; do
  if ! dx_have "$tool"; then
    printf '%-7s (absent)\n' "$tool"
  elif [ "$tool" = "go" ]; then
    # go takes `go version`, not `--version`.
    printf '%-7s %s\n' "$tool" "$(go version 2>&1 | head -n1)"
  else
    printf '%-7s %s\n' "$tool" "$("$tool" --version 2>&1 | head -n1)"
  fi
done
dx_pass versions "tool versions captured"

# ── stack state ──────────────────────────────────────────────────────────────
section "stack ($DX_HOST)"
SERVICES=()
if [ "$DX_HOST" != "local" ]; then
  if dx_ssh_remote "cd '$DX_REMOTE_DIR/deploy' && docker compose $(dx_remote_compose_args) ps --all" 2>&1 | "$REDACT"; then
    dx_pass stack "$DX_REMOTE_NAME container states captured"
  else
    dx_warn stack "could not read $DX_REMOTE_NAME container states"
  fi
  while IFS= read -r c; do [ -n "$c" ] && SERVICES+=("$c"); done < <(
    dx_ssh_remote "docker ps --format '{{.Names}}' --filter name=quasar" 2>/dev/null || true
  )
elif dx_have docker && docker info >/dev/null 2>&1; then
  dx_local_compose ps --all 2>&1 | "$REDACT" || true
  dx_pass stack "local container states captured"
  while IFS= read -r c; do [ -n "$c" ] && SERVICES+=("$c"); done < <(
    dx_local_compose ps --all --format '{{.Name}}' 2>/dev/null || true
  )
else
  dx_warn stack "docker daemon not reachable — no container state"
fi

# ── health ───────────────────────────────────────────────────────────────────
section "health"
if [ "$DX_HOST" != "local" ]; then
  if dx_ssh_remote "curl -fsS --max-time 5 http://localhost:8080/health" 2>/dev/null | "$REDACT"; then
    printf '\n'
    dx_pass health "$DX_REMOTE_NAME /health responded"
  else
    dx_warn health "$DX_REMOTE_NAME /health did not respond"
  fi
elif dx_have curl; then
  if curl -fsS --max-time 5 "http://127.0.0.1:${DX_CP_PORT}/health" 2>/dev/null | "$REDACT"; then
    printf '\n'
    dx_pass health "control-plane /health responded on :$DX_CP_PORT"
  else
    dx_warn health "control-plane /health did not respond on :$DX_CP_PORT (stack down?)"
  fi
else
  dx_warn health "curl not on PATH"
fi

# ── recent errors, per service, bounded ──────────────────────────────────────
section "recent errors (last $ERR_LINES ERROR-level lines per service)"
if [ "${#SERVICES[@]}" -eq 0 ]; then
  printf '(no containers)\n'
else
  # bash 3.2 (macOS): "${a[@]}" on an empty array trips `set -u`; the loop is
  # already guarded by the count above, but keep the safe idiom.
  for svc in ${SERVICES[@]+"${SERVICES[@]}"}; do
    printf -- '--- %s ---\n' "$svc"
    if [ "$DX_HOST" != "local" ]; then
      dx_ssh_remote "docker logs --tail 2000 '$svc' 2>&1 | grep -iE '\\b(error|fatal|panic)\\b' | tail -n $ERR_LINES" 2>/dev/null \
        | "$REDACT" || true
    else
      docker logs --tail 2000 "$svc" 2>&1 \
        | grep -iE '\b(error|fatal|panic)\b' | tail -n "$ERR_LINES" | "$REDACT" || true
    fi
  done
  dx_pass logs "bounded error tail captured for ${#SERVICES[@]} service(s)"
fi

printf '\n'
dx_result "$TARGET" "services=${#SERVICES[@]}"
