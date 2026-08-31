#!/usr/bin/env bash
set -euo pipefail

pass() { printf 'PASS — %s\n' "$*"; }
skip() { printf 'SKIP — %s\n' "$*"; }
run() {
  local label="$1"; shift
  printf 'RUN  — %s\n' "$label"
  if "$@"; then pass "$label"; else printf 'FAIL — %s\n' "$label" >&2; return 1; fi
}

cd /workspace
mkdir -p "$HOME"
# In a git WORKTREE, /workspace/.git is a *file* pointing at an absolute gitdir
# outside the bind mount; verify.sh bind-mounts the git common dir at its host
# path so those references resolve. Every path involved is owned by the host
# user, not the container's, so mark all of them safe rather than just
# /workspace — otherwise git refuses with "detected dubious ownership" and any
# git call (quick.sh's whitespace check, and repo discovery itself) dies. This
# HOME is a throwaway inside an ephemeral container.
git config --global --add safe.directory '*'
