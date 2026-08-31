#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/metrics_manifest.sh — the metric manifest's generated artifacts.
#
#   make docs-metrics-sync   # copy the manifest to its two consumers
#   make docs-trace          # sync, then regenerate the trace-format table
#
# The SOURCE is docs/session-trace/metrics.json, beside thresholds.json, for the
# same reason: a spec a human edits, next to the prose that explains it. Two
# copies exist because neither consumer can reach it — Go's embed cannot leave
# the module, and the web tsconfig includes only `src`. Both copies are
# byte-equality tested (Go: TestMetricsManifestIsInSync / TestWebMetricsManifest-
# IsInSync), so a stale copy is a red build, never a silent divergence.
#
# bash 3.2 compatible (macOS): no associative arrays, no mapfile.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"

SRC="$ROOT/docs/session-trace/metrics.json"
COPIES="$ROOT/control-plane/internal/telemetry/metrics.json $ROOT/web/src/lib/metrics.generated.json"

usage() { printf 'usage: %s sync|check\n' "$(basename "$0")" >&2; exit 2; }

[ -f "$SRC" ] || { printf 'FAIL — missing %s\n' "$SRC" >&2; exit 1; }

# Validate before promoting: a broken manifest must not be copied into two
# consumers where it becomes two broken manifests.
python3 - "$SRC" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
assert doc.get("version"), "manifest has no version"
seen = set()
tax = set()
for e in doc["metrics"]:
    for f in ("key", "source", "unit", "clock", "window", "estimator", "why"):
        assert e.get(f), "%s: empty %s" % (e.get("key"), f)
    ident = (e["source"], e["key"])
    assert ident not in seen, "duplicate %s" % (ident,)
    seen.add(ident)
    if e.get("taxonomy"):
        assert e["taxonomy"] not in tax, "duplicate taxonomy %s" % e["taxonomy"]
        tax.add(e["taxonomy"])
PY

case "${1:-}" in
  sync)
    for dst in $COPIES; do
      mkdir -p "$(dirname "$dst")"
      cp "$SRC" "$dst"
      printf 'synced %s\n' "${dst#"$ROOT"/}"
    done
    ;;
  check)
    rc=0
    for dst in $COPIES; do
      if ! cmp -s "$SRC" "$dst"; then
        printf 'STALE %s — run: make docs-metrics-sync\n' "${dst#"$ROOT"/}" >&2
        rc=1
      fi
    done
    [ "$rc" -eq 0 ] && printf 'metric manifest copies are in sync\n'
    exit "$rc"
    ;;
  *) usage ;;
esac
