#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/bench_retro.sh — replay an archived run manifest into quasar-bench.
#
#   scripts/dx/bench_retro.sh [--manifest FILE] [--dry-run] [--only <substring>]
#   make bench-retro
#
# The manifest (default docs/reports/2026-08-16-abr-ladder/bench-retro-manifest.json)
# lists every run directory that predates the results service, with the tags and
# verdicts derived from that campaign's VALIDATION.md. Each entry becomes one
# `scripts/dx/bench_submit.py` invocation.
#
# IDEMPOTENT: bench_submit.py stamps every run with a deterministic
# `bench_ext_id` and reuses the matching run on a re-run, so replaying the whole
# manifest twice converges instead of duplicating.
#
# Needs BENCH_URL + BENCH_KEY in the environment (never committed).
#
# Exit: 0 all entries submitted, 1 any failure, 2 usage.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

TARGET=bench-retro

# `make bench-retro ARGS=...` delivers ARGS by ENVIRONMENT, not interpolated
# into the recipe line (#550).
[ $# -gt 0 ] || { dx_env_argv "$TARGET" ARGS; set -- ${DX_ARGV[@]+"${DX_ARGV[@]}"}; }

dx_require_local "$TARGET"

usage() { sed -n '3,22p' "$0" | sed 's/^# \{0,1\}//'; }

MANIFEST="$DX_ROOT/docs/reports/2026-08-16-abr-ladder/bench-retro-manifest.json"
DRY=0
ONLY=""

while [ $# -gt 0 ]; do
  case "$1" in
    --manifest)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--manifest requires a file"
      MANIFEST="$2"; shift 2 ;;
    --only)
      [ $# -ge 2 ] || dx_guard "$TARGET" "--only requires a substring"
      ONLY="$2"; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) dx_guard "$TARGET" "unknown arg '$1' — see: scripts/dx/bench_retro.sh --help" ;;
  esac
done

dx_have python3 || { dx_fail python3 "not on PATH"; dx_result "$TARGET"; }
[ -f "$MANIFEST" ] || dx_guard "$TARGET" "no such manifest: $MANIFEST"

# One line per entry: dir<TAB>suite<TAB>scenario<TAB>verdict<TAB>notes<TAB>k=v k=v ...
PLAN="$(python3 - "$MANIFEST" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
default_suite = m.get("_suite", "abr-ladder")
for r in m["runs"]:
    tags = " ".join("%s=%s" % (k, v) for k, v in sorted((r.get("tags") or {}).items()))
    print("\t".join([r["dir"], r.get("_suite", default_suite), r["scenario"],
                     r.get("verdict", "INFO"),
                     (r.get("notes") or "").replace("\t", " "), tags]))
PY
)"

N=0
while IFS=$'\t' read -r dir suite scenario verdict notes tags; do
  [ -n "$dir" ] || continue
  case "$dir" in "$ONLY"*|*"$ONLY"*) ;; *) continue ;; esac
  [ -d "$DX_ROOT/$dir" ] || { dx_warn "$dir" "directory missing — skipped"; continue; }
  N=$((N + 1))
  ARGS=(--dir "$DX_ROOT/$dir" --suite "$suite" --scenario "$scenario"
        --verdict "$verdict" --notes "$notes" --host devbox)
  for kv in $tags; do ARGS+=(--tag "$kv"); done
  [ "$DRY" = 1 ] && ARGS+=(--dry-run)
  printf '\n── %s\n' "$dir"
  if python3 "$DX_DIR/bench_submit.py" "${ARGS[@]}"; then
    dx_pass "$(basename "$dir")" "submitted"
  else
    dx_fail "$(basename "$dir")" "bench_submit.py failed"
  fi
done <<< "$PLAN"

dx_result "$TARGET" "manifest=$(basename "$MANIFEST")" "entries=$N" "dry_run=$DRY"
