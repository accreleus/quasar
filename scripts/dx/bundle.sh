#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/bundle.sh — archivable diagnostic bundle for this worktree/stack.
#
#   make diagnose-bundle
#   make diagnose-bundle HOST=gpu-test
#
# Writes .diagnostics/<ts>-<instance>/ (mode 0700) and a .tar.gz beside it.
#
# CONTENTS (see MANIFEST.txt in every bundle):
#   diagnose.txt          scripts/dx/diagnose.sh output
#   logs/<service>.log    docker logs --tail 500, per service
#   inspect/<service>.json  docker inspect State
#   compose-config.yaml   the RESOLVED compose config, piped through redact.sh
#   versions.txt          tool versions
#   git.txt               branch / rev / dirty / submodule state
#   control-plane-bundle.json  the CP's own diagnostic bundle, when obtainable
#
# EXCLUDED BY DEFAULT (and listed as excluded in the manifest, so a reader knows
# what they are NOT looking at rather than wondering): deploy/.env, credentials
# and key material, database data directories, home directories, core dumps.
#
# Everything that can carry a value goes through scripts/dx/redact.sh. The
# bundle is still yours to review before you attach it to an issue.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_host_scope status

TARGET=diagnose-bundle
REDACT="$DX_DIR/redact.sh"
TAIL_LINES="${DX_BUNDLE_TAIL:-500}"
# Reaches a remote `docker logs --tail $TAIL_LINES` UNQUOTED, so no escape is
# needed to inject — a `;` is enough.
dx_require_safe "$TARGET" "DX_BUNDLE_TAIL" "$TAIL_LINES" "$DX_RE_UINT" "It is a line count."

TS="$(dx_timestamp)"
OUT_DIR="$DX_DIAG_DIR/${TS}-${QUASAR_INSTANCE}"
mkdir -p "$DX_DIAG_DIR"
chmod 0700 "$DX_DIAG_DIR" 2>/dev/null || true
mkdir -p "$OUT_DIR/logs" "$OUT_DIR/inspect"
chmod 0700 "$OUT_DIR"

MANIFEST="$OUT_DIR/MANIFEST.txt"
included=()
excluded=()

note_included() { included+=("$1"); }
note_excluded() { excluded+=("$1"); }

# ── diagnose ─────────────────────────────────────────────────────────────────
if bash "$DX_DIR/diagnose.sh" > "$OUT_DIR/diagnose.txt" 2>&1; then
  dx_pass diagnose "diagnose.txt captured"
else
  dx_warn diagnose "diagnose.sh reported problems (captured anyway)"
fi
note_included "diagnose.txt — scripts/dx/diagnose.sh output"

# ── git ──────────────────────────────────────────────────────────────────────
{
  if dx_have git && git -C "$DX_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    printf 'branch: %s\n' "$(git -C "$DX_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
    printf 'rev:    %s\n' "$(git -C "$DX_ROOT" rev-parse HEAD 2>/dev/null || echo '?')"
    printf 'status:\n'
    git -C "$DX_ROOT" status --porcelain 2>/dev/null || true
    printf 'submodules:\n'
    git -C "$DX_ROOT" submodule status 2>/dev/null || true
  else
    printf 'not a git worktree\n'
  fi
} > "$OUT_DIR/git.txt" 2>&1
note_included "git.txt — branch, rev, dirty files, submodule pins"

# ── versions ─────────────────────────────────────────────────────────────────
{
  for tool in docker go node npm cargo rustc git curl shellcheck; do
    if ! dx_have "$tool"; then
      printf '%-10s (absent)\n' "$tool"
    elif [ "$tool" = "go" ]; then
      printf '%-10s %s\n' "$tool" "$(go version 2>&1 | head -n1)"
    else
      printf '%-10s %s\n' "$tool" "$("$tool" --version 2>&1 | head -n1)"
    fi
  done
  printf '%-10s %s\n' "uname" "$(uname -a)"
} > "$OUT_DIR/versions.txt" 2>&1
note_included "versions.txt — tool versions and kernel"

# ── resolved compose config (REDACTED) ───────────────────────────────────────
# `docker compose config` interpolates every ${VAR}, so this file WOULD contain
# the .env values verbatim. It is piped through redact.sh unconditionally.
if dx_have docker && docker info >/dev/null 2>&1 && [ "$DX_HOST" = "local" ]; then
  if dx_local_compose config 2>&1 | "$REDACT" > "$OUT_DIR/compose-config.yaml"; then
    dx_pass compose "resolved compose config captured (redacted)"
  else
    dx_warn compose "could not resolve the compose config"
  fi
  note_included "compose-config.yaml — RESOLVED compose config, piped through scripts/dx/redact.sh"
else
  printf 'not captured (docker unavailable, or HOST=%s)\n' "$DX_HOST" > "$OUT_DIR/compose-config.yaml"
  note_included "compose-config.yaml — SKIPPED (docker unavailable or non-local host)"
fi

# ── per-service logs + inspect ───────────────────────────────────────────────
SERVICES=()
if [ "$DX_HOST" != "local" ]; then
  while IFS= read -r c; do [ -n "$c" ] && SERVICES+=("$c"); done < <(
    dx_ssh_remote "docker ps -a --format '{{.Names}}' --filter name=quasar" 2>/dev/null || true
  )
elif dx_have docker && docker info >/dev/null 2>&1; then
  while IFS= read -r c; do [ -n "$c" ] && SERVICES+=("$c"); done < <(
    dx_local_compose ps --all --format '{{.Name}}' 2>/dev/null || true
  )
fi

# ${a[@]+"${a[@]}"} — macOS ships bash 3.2, where a plain "${a[@]}" on an EMPTY
# array is an unbound-variable error under `set -u`.
for svc in ${SERVICES[@]+"${SERVICES[@]}"}; do
  safe="$(printf '%s' "$svc" | tr -c 'A-Za-z0-9._-' '_')"
  if [ "$DX_HOST" != "local" ]; then
    dx_ssh_remote "docker logs --tail $TAIL_LINES '$svc' 2>&1" 2>/dev/null \
      | "$REDACT" > "$OUT_DIR/logs/${safe}.log" || true
    dx_ssh_remote "docker inspect --format '{{json .State}}' '$svc'" 2>/dev/null \
      | "$REDACT" > "$OUT_DIR/inspect/${safe}.json" || true
  else
    docker logs --tail "$TAIL_LINES" "$svc" 2>&1 \
      | "$REDACT" > "$OUT_DIR/logs/${safe}.log" || true
    docker inspect --format '{{json .State}}' "$svc" 2>/dev/null \
      | "$REDACT" > "$OUT_DIR/inspect/${safe}.json" || true
  fi
done
if [ "${#SERVICES[@]}" -gt 0 ]; then
  dx_pass logs "captured tail=$TAIL_LINES + inspect State for ${#SERVICES[@]} service(s)"
else
  dx_warn logs "no containers found — logs/ and inspect/ are empty"
fi
note_included "logs/<service>.log — docker logs --tail $TAIL_LINES, redacted"
note_included "inspect/<service>.json — docker inspect State, redacted"

# ── control-plane's own diagnostic bundle ────────────────────────────────────
# GET /v1/admin/sessions/{id}/diagnostic-bundle (control-plane/internal/session/
# handler.go) is ADMIN-GATED and per-session: it needs both an admin bearer token
# and a session id. When either is absent this is SKIPPED and said so — a
# missing optional artifact must never fail the bundle.
CP_BUNDLE="$OUT_DIR/control-plane-bundle.json"
# The bearer comes from THE one ladder (scripts/dx/admin_token.sh); this file
# does not carry a second one. A failure is not fatal here — the CP bundle is an
# OPTIONAL artifact and its absence is recorded, never a failed diagnostic bundle.
CP_TOKEN="${QUASAR_ADMIN_TOKEN:-}"
if [ -z "$CP_TOKEN" ] && [ "$DX_HOST" = "local" ] && [ -n "${QUASAR_SESSION_ID:-}" ]; then
  CP_TOKEN="$(bash "$DX_DIR/admin_token.sh" --host local --quiet 2>/dev/null)" || CP_TOKEN=""
fi
CP_SESSION="${QUASAR_SESSION_ID:-}"
if [ "$DX_HOST" != "local" ]; then
  printf '{"skipped":"HOST=%s; fetch the CP bundle from the box itself"}\n' "$DX_HOST" > "$CP_BUNDLE"
  note_included "control-plane-bundle.json — SKIPPED (non-local host)"
elif ! dx_have curl; then
  printf '{"skipped":"curl not on PATH"}\n' > "$CP_BUNDLE"
  note_included "control-plane-bundle.json — SKIPPED (curl absent)"
elif [ -z "$CP_TOKEN" ] || [ -z "$CP_SESSION" ]; then
  printf '{"skipped":"needs QUASAR_ADMIN_TOKEN and QUASAR_SESSION_ID; GET /v1/admin/sessions/{id}/diagnostic-bundle is admin-gated and per-session"}\n' > "$CP_BUNDLE"
  note_included "control-plane-bundle.json — SKIPPED (no admin token and/or session id available)"
  dx_info "control-plane diagnostic bundle skipped: set QUASAR_ADMIN_TOKEN + QUASAR_SESSION_ID to include it"
else
  # The admin bearer token goes to curl via a header FILE (`-H @file`), never in
  # argv: any local user can read another process's argv via `ps auxww`, and a
  # literal `-H "Authorization: Bearer $CP_TOKEN"` would expose the token there
  # for the life of the curl process. Create it 0600 and remove on exit.
  CP_HDR_FILE="$(mktemp)"
  chmod 600 "$CP_HDR_FILE"
  printf 'Authorization: Bearer %s\n' "$CP_TOKEN" > "$CP_HDR_FILE"
  trap 'rm -f "$CP_HDR_FILE"' EXIT
  if curl -fsS --max-time 15 \
        -H @"$CP_HDR_FILE" \
        "http://127.0.0.1:${DX_CP_PORT}/v1/admin/sessions/${CP_SESSION}/diagnostic-bundle" 2>/dev/null \
      | "$REDACT" > "$CP_BUNDLE"; then
    dx_pass cp-bundle "control-plane diagnostic bundle fetched for session $CP_SESSION"
    note_included "control-plane-bundle.json — GET /v1/admin/sessions/$CP_SESSION/diagnostic-bundle"
  else
    printf '{"skipped":"request failed (unreachable, unauthorized, or unknown session)"}\n' > "$CP_BUNDLE"
    dx_warn cp-bundle "control-plane diagnostic-bundle request failed — recorded as SKIPPED"
    note_included "control-plane-bundle.json — SKIPPED (request failed)"
  fi
  rm -f "$CP_HDR_FILE"
  trap - EXIT
fi

# ── exclusions ───────────────────────────────────────────────────────────────
note_excluded "deploy/.env — real credentials; never bundled"
note_excluded "TLS private keys / certificates and any PEM material"
note_excluded "database data directories (postgres volumes) — never bundled"
note_excluded "user home directories and per-session home volumes"
note_excluded "core dumps and process memory captures"
note_excluded "bearer tokens, API keys, and passwords — masked by scripts/dx/redact.sh wherever they appear in captured output"

# ── manifest ─────────────────────────────────────────────────────────────────
{
  printf 'Quasar diagnostic bundle\n'
  printf 'generated: %s\n' "$TS"
  printf 'instance:  %s\n' "$QUASAR_INSTANCE"
  printf 'host:      %s\n' "$DX_HOST"
  printf 'root:      %s\n' "$DX_ROOT"
  printf '\nINCLUDED\n'
  for i in "${included[@]}"; do printf '  + %s\n' "$i"; done
  printf '\nEXCLUDED BY DEFAULT\n'
  for e in "${excluded[@]}"; do printf '  - %s\n' "$e"; done
  printf '\nAll captured output is filtered through scripts/dx/redact.sh.\n'
  printf 'Review before sharing: redaction is a safety net, not a guarantee.\n'
} > "$MANIFEST"
dx_pass manifest "MANIFEST.txt lists ${#included[@]} included and ${#excluded[@]} excluded item(s)"

# ── archive ──────────────────────────────────────────────────────────────────
ARCHIVE="$DX_DIAG_DIR/${TS}-${QUASAR_INSTANCE}.tar.gz"
if tar -czf "$ARCHIVE" -C "$DX_DIAG_DIR" "${TS}-${QUASAR_INSTANCE}" 2>/dev/null; then
  chmod 0600 "$ARCHIVE" 2>/dev/null || true
  dx_pass archive "$ARCHIVE"
else
  dx_warn archive "could not create the tar.gz — the directory $OUT_DIR is still there"
fi

dx_info "bundle: $OUT_DIR"
dx_result "$TARGET" "bundle=$OUT_DIR" "services=${#SERVICES[@]}"
