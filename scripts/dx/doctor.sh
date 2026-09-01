#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/doctor.sh — "can this machine do Quasar work?"
#
#   make doctor
#
# Everything here is advisory except the things that make local work outright
# impossible (no docker). Remote (gpu-test role) reachability in particular is
# ADVISORY: a Mac on a different network is degraded, not broken — the whole
# local loop still works.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_local doctor

TARGET=doctor

# ── docker ───────────────────────────────────────────────────────────────────
if ! dx_have docker; then
  dx_fail docker "not on PATH — install Docker Desktop (macOS) or docker-ce (Linux)"
else
  if docker info >/dev/null 2>&1; then
    dx_pass docker "daemon reachable ($(docker --version 2>/dev/null | head -n1))"
  else
    dx_fail docker "installed but the daemon is not reachable — start Docker Desktop"
  fi
fi

# ── go ───────────────────────────────────────────────────────────────────────
# Control-plane go.mod targets 1.25; a newer toolchain is fine.
if ! dx_have go; then
  dx_warn go "not on PATH — control-plane builds fall back to the container path"
else
  go_ver="$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  go_major="${go_ver%%.*}"
  go_rest="${go_ver#*.}"
  go_minor="${go_rest%%.*}"
  if [ "${go_major:-0}" -gt 1 ] || { [ "${go_major:-0}" -eq 1 ] && [ "${go_minor:-0}" -ge 25 ]; }; then
    dx_pass go "$go_ver (>= 1.25)"
  else
    dx_warn go "$go_ver is below the 1.25 the control-plane targets"
  fi
fi

# ── node ─────────────────────────────────────────────────────────────────────
# vite 6 needs Node >= 22 (and pnpm >= 11 silently skips esbuild's postinstall,
# which is why the repo standardizes on npm).
if ! dx_have node; then
  dx_warn node "not on PATH — web builds fall back to the container path"
else
  node_ver="$(node --version 2>/dev/null | sed 's/^v//')"
  node_major="${node_ver%%.*}"
  if [ "${node_major:-0}" -ge 22 ]; then
    dx_pass node "$node_ver (>= 22)"
  else
    dx_warn node "$node_ver is below 22 — vite 6 requires Node >= 22"
  fi
fi

# ── protocol/ submodule ──────────────────────────────────────────────────────
# A fresh worktree has an EMPTY protocol/ and TestOpenAPIDrift fails with
# "no such file or directory" until it is initialized. This is the single most
# common false alarm in a new worktree, so it gets its own check.
if [ -f "$DX_ROOT/protocol/openapi.yaml" ]; then
  dx_pass submodule "protocol/ is initialized"
else
  dx_warn submodule "protocol/ is NOT initialized — control-plane TestOpenAPIDrift will fail; run: git submodule update --init protocol"
fi

# ── disk space ───────────────────────────────────────────────────────────────
# Image builds in this repo are large; under ~10G free they start failing in
# confusing ways mid-layer.
avail_kb="$(df -Pk "$DX_ROOT" 2>/dev/null | awk 'NR==2 {print $4}')"
if [ -n "${avail_kb:-}" ]; then
  avail_gb=$(( avail_kb / 1024 / 1024 ))
  if [ "$avail_gb" -ge 20 ]; then
    dx_pass disk "${avail_gb}G free on the worktree filesystem"
  elif [ "$avail_gb" -ge 10 ]; then
    dx_warn disk "${avail_gb}G free — image builds want 20G+"
  else
    dx_warn disk "${avail_gb}G free — too little for an image build; free space before make rebuild"
  fi
else
  dx_warn disk "could not determine free space"
fi

# ── Remote reachability, gpu-test role (ADVISORY) ────────────────────────────
if ! dx_have ssh; then
  dx_warn remote "ssh not on PATH — remote targets unavailable"
elif ! dx_have python3; then
  dx_warn remote "python3 not on PATH — cannot resolve .claude/skills/_shared/hosts.json"
elif ! dx_resolve_remote gpu-test; then
  dx_warn remote "no gpu-test role configured in .claude/skills/_shared/hosts.json — remote targets are unavailable from here (advisory, local work is unaffected; see hosts.example.json)"
elif dx_ssh_remote true >/dev/null 2>&1; then
  dx_pass remote "$DX_REMOTE_NAME (gpu-test) reachable"
else
  dx_warn remote "$DX_REMOTE_NAME (gpu-test) not reachable — remote targets are unavailable from here (advisory, local work is unaffected)"
fi

# ── rtk (informational) ──────────────────────────────────────────────────────
# Note only. The dx scripts deliberately call docker/go/cargo DIRECTLY: rtk's
# output filtering masks exit codes, and every check here is exit-code driven.
if dx_have rtk; then
  dx_info "note: rtk is installed; dx scripts intentionally bypass it (it masks exit codes)"
fi

dx_info "instance=$QUASAR_INSTANCE cp=$DX_CP_PORT tls=$DX_CP_TLS_PORT pg=$DX_PG_PORT"

dx_result "$TARGET" "cp_port=$DX_CP_PORT" "pg_port=$DX_PG_PORT"
