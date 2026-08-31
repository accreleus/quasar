#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/config_check.sh — validate every compose file set that exists, and
# advise on deploy/.env drift against docs/configuration.md.
#
#   make config-check
#
# `docker compose config -q` is the only thing that catches an interpolation
# typo or a missing required var before a deploy does. The .env key diff is
# ADVISORY (WARN, never FAIL): docs/configuration.md is prose, and a key can
# legitimately exist in one and not the other.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_local config-check

TARGET=config-check
cd "$DX_ROOT/deploy"

if ! dx_have docker; then
  dx_fail docker "not on PATH — cannot validate compose files"
  dx_result "$TARGET"
fi

# Compose validation needs the vars to at least interpolate. Supply throwaway
# values for the `${VAR:?}` required ones so validation reports STRUCTURE
# problems rather than "you have no .env yet".
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-config-check-placeholder}"
export ENROLLMENT_TOKEN="${ENROLLMENT_TOKEN:-config-check-placeholder}"
# The adopt-volumes overlay declares all three volume names `${VAR:?}`. Supplying
# throwaway names here validates its STRUCTURE on every machine; the base file
# deliberately ignores these vars (asserted by scripts/dev/test-compose-overlays.sh),
# so exporting them cannot change what any other set renders.
export QUASAR_POSTGRES_VOLUME="${QUASAR_POSTGRES_VOLUME:-config-check-postgres-vol}"
export QUASAR_AGENT_VOLUME="${QUASAR_AGENT_VOLUME:-config-check-agent-vol}"
export QUASAR_CONTROL_VOLUME="${QUASAR_CONTROL_VOLUME:-config-check-control-vol}"

# Each entry is a file set validated as one unit (an overlay is only valid on
# top of its base).
CONFIG_SETS=(
  "local:overlays/docker-compose.local.yml"
  "devtools:../scripts/verify/docker-compose.devtools.yml"
  # The base file and every overlay that layers on it. Overlays are orthogonal:
  # vendor (nvidia), feature (console, cores, profiling, multiagent), and
  # deployment shape (hardened, dev) compose freely, so this list validates
  # each one over the base plus the combinations a real host actually runs.
  "base:docker-compose.yml"
  "nvidia:docker-compose.yml docker-compose.nvidia.yml"
  "console:docker-compose.yml overlays/docker-compose.console.yml"
  "nvidia-console:docker-compose.yml docker-compose.nvidia.yml overlays/docker-compose.console.yml"
  "hardened:docker-compose.yml docker-compose.hardened.yml"
  "profiling:docker-compose.yml overlays/docker-compose.profiling.yml"
  "cores:docker-compose.yml overlays/docker-compose.cores.yml"
  "dev:docker-compose.yml overlays/docker-compose.dev.yml"
  "nvidia-dev:docker-compose.yml docker-compose.nvidia.yml overlays/docker-compose.dev.yml"
  "multiagent:docker-compose.yml overlays/docker-compose.multiagent.yml"
  # Opt-in but live: redeploy.sh layers this whenever the QUASAR_*_VOLUME
  # adoption path is taken, so it is a real deployment shape and must be
  # validated like the rest.
  "adopt-volumes:docker-compose.yml overlays/docker-compose.adopt-volumes.yml"
)

for entry in "${CONFIG_SETS[@]}"; do
  label="${entry%%:*}"
  files="${entry#*:}"
  args=()
  missing=0
  for f in $files; do
    if [ ! -f "$f" ]; then missing=1; break; fi
    args+=(-f "$f")
  done
  if [ "$missing" -eq 1 ]; then
    dx_info "skip $label — file set not present in this worktree"
    continue
  fi
  if err="$(docker compose "${args[@]}" config -q 2>&1)"; then
    dx_pass "compose:$label" "$files"
  else
    safe_err="$(printf '%s' "$err" | "$DX_DIR/redact.sh" | head -n 3 | tr '\n' ' ')"
    # "required variable X is missing a value" means THIS MACHINE has not been
    # configured for that overlay (hardened needs QUASAR_PUBLIC_HOST, profiling
    # needs an explicit dated QUASAR_PROFILING_IMAGE). That is an operator-input
    # gap, not a broken file — WARN. A structural error is a real FAIL.
    if printf '%s' "$err" | grep -q 'required variable'; then
      dx_warn "compose:$label" "not configured on this machine — $safe_err"
    else
      dx_fail "compose:$label" "$safe_err"
    fi
  fi
done

# ── .env key drift vs docs/configuration.md (ADVISORY) ───────────────────────
ENV_FILE="$DX_ROOT/deploy/.env"
DOCS="$DX_ROOT/docs/configuration.md"
if [ ! -f "$ENV_FILE" ]; then
  dx_info "skip env-drift — deploy/.env absent (run: make init)"
elif [ ! -f "$DOCS" ]; then
  dx_warn env-drift "docs/configuration.md missing — cannot cross-check .env keys"
else
  # Keys only, never values.
  undocumented=""
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    if ! grep -q -- "$key" "$DOCS"; then
      undocumented="$undocumented $key"
    fi
  done < <(sed -n 's/^[[:space:]]*\([A-Z_][A-Z0-9_]*\)=.*/\1/p' "$ENV_FILE" | sort -u)

  if [ -n "$undocumented" ]; then
    dx_warn env-drift "deploy/.env sets keys not mentioned in docs/configuration.md:${undocumented}"
  else
    dx_pass env-drift "every deploy/.env key appears in docs/configuration.md"
  fi
fi

dx_result "$TARGET"
