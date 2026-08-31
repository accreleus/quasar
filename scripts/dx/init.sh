#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/init.sh — one command to make a fresh clone/worktree workable.
#
#   make init
#
# Idempotent by construction. In particular it NEVER overwrites an existing
# deploy/.env — that file holds real values on a dev box.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_local init

TARGET=init
cd "$DX_ROOT"

# ── 1. protocol/ submodule ───────────────────────────────────────────────────
# Without this, control-plane's TestOpenAPIDrift fails environmentally in every
# fresh worktree.
if [ -f "$DX_ROOT/protocol/openapi.yaml" ]; then
  dx_pass submodule "protocol/ already initialized"
elif ! dx_have git; then
  dx_fail submodule "git not on PATH"
elif git submodule update --init protocol >/dev/null 2>&1; then
  dx_pass submodule "protocol/ initialized"
else
  dx_warn submodule "git submodule update --init protocol failed — TestOpenAPIDrift will fail until it succeeds"
fi

# ── 2. deploy/.env ───────────────────────────────────────────────────────────
ENV_FILE="$DX_ROOT/deploy/.env"
ENV_EXAMPLE="$DX_ROOT/deploy/.env.example"
if [ -f "$ENV_FILE" ]; then
  dx_pass env "deploy/.env exists — left untouched"
elif [ -f "$ENV_EXAMPLE" ]; then
  cp "$ENV_EXAMPLE" "$ENV_FILE"
  dx_pass env "created deploy/.env from deploy/.env.example"
  dx_info "fill in the required values before any deploy (POSTGRES_PASSWORD, ENROLLMENT_TOKEN, BOOTSTRAP_ADMIN_*)"
else
  dx_warn env "no deploy/.env and no deploy/.env.example to seed it from"
fi

# ── 3. devtools image ────────────────────────────────────────────────────────
# scripts/verify.sh is the canonical containerized build/test entry point; its
# image is what `make test-go|test-rust|test-web` run inside.
if [ "${DX_SKIP_IMAGE:-0}" = "1" ]; then
  dx_info "skip devtools image build (DX_SKIP_IMAGE=1)"
elif ! dx_have docker || ! docker info >/dev/null 2>&1; then
  dx_warn devtools "docker daemon unreachable — skipped the devtools image build"
elif bash "$DX_ROOT/scripts/verify.sh" build; then
  dx_pass devtools "quasar-devtools:local built"
else
  dx_warn devtools "scripts/verify.sh build failed — containerized test targets will not work yet"
fi

# ── 4. doctor ────────────────────────────────────────────────────────────────
dx_info "--- doctor ---"
doctor_rc=0
bash "$DX_DIR/doctor.sh" || doctor_rc=$?
dx_info "--- end doctor (rc=$doctor_rc) ---"
if [ "$doctor_rc" -gt 1 ]; then
  dx_fail doctor "doctor exited $doctor_rc"
elif [ "$doctor_rc" -eq 1 ]; then
  dx_fail doctor "doctor reported a blocking problem"
else
  dx_pass doctor "doctor completed"
fi

dx_result "$TARGET"
