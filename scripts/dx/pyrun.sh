#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/pyrun.sh — run one of the python DX tools with its knobs taken from
# the ENVIRONMENT instead of from a Makefile recipe line (#550).
#
#   scripts/dx/pyrun.sh <target> <script.py> [--flag VAR]...
#
# Each `--flag VAR` pair contributes exactly two arguments: the literal flag,
# and the value of the environment variable VAR as ONE argument (so a path with
# a space in it still works). `$ARGS` is split into arguments and appended last.
#
# The python tools cannot source common.sh, so this is the trampoline that lets
# `bench-submit`, `bench-budget` and `bench-baseline` keep their DIR=/RUN=/NAME=
# knobs without the Makefile interpolating a caller's value into a shell command
# string. Every value here becomes an element of a bash array — argv, never text
# a shell re-parses.
#
#   make bench-submit DIR=<run dir> ARGS='--suite ... --scenario ...'
#   make bench-budget RUN=<id>|latest ARGS='--baseline ...'
#   make bench-baseline RUN=<id> NAME=<suite/scenario>
set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

[ $# -ge 2 ] || { printf 'usage: pyrun.sh <target> <script.py> [--flag VAR]...\n' >&2; exit 2; }
TARGET="$1"; SCRIPT="$2"; shift 2

[ -f "$DX_DIR/$SCRIPT" ] || dx_guard "$TARGET" "pyrun.sh: no such tool: scripts/dx/$SCRIPT"

PY_ARGV=()
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || dx_guard "$TARGET" "pyrun.sh: '$1' must be followed by the NAME of an environment variable"
  PY_ARGV+=("$1" "${!2:-}")
  shift 2
done

# ARGS is a list, so it is the one knob that is split — and therefore the one
# that is shape-checked (see dx_env_argv in common.sh).
dx_env_argv "$TARGET" ARGS

exec python3 "$DX_DIR/$SCRIPT" \
  ${PY_ARGV[@]+"${PY_ARGV[@]}"} \
  ${DX_ARGV[@]+"${DX_ARGV[@]}"}
