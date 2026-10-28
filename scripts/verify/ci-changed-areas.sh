#!/usr/bin/env bash
# ci-changed-areas.sh: which CI test areas does a change touch? (#919)
#
#   gh api .../pulls/N/files --jq '.[].filename' | scripts/verify/ci-changed-areas.sh
#
# Reads changed paths on stdin and prints `go=`, `web=`, `rust=` and `site=` as
# true/false. ci.yml runs a test job on a pull request into develop only when its area
# says true. A push to develop runs every job regardless, so a miss here is caught on
# develop, never shipped.
#
# THE TABLE. An area's own tree, plus every file outside it that its tests read. When
# you add a test that reads a file outside its own tree, add the path here.
#
#   go    control-plane/**  protocol  testdata/**  deploy/**  docs/session-trace/**
#         web/index.html  web/src/lib/metrics.generated.json
#   web   web/**  testdata/**  deploy/**  docs/session-trace/**  docs/configuration.md
#         docs/upgrading.md
#   rust  node-agent/**  testdata/**  deploy/**  docs/session-trace/**  docs/configuration.md
#         control-plane/internal/buildinfo/recipe.go  (rust + the engine suite)
#   site  site/**  protocol  testdata/**  deploy/**
#   all   .github/**  scripts/verify/ci-*  .gitmodules
#   none  everything else: docs, CHANGELOG, scripts outside CI, design files
#
# Patterns are shell `case` globs, where `*` also matches `/`.
set -euo pipefail

[ $# -eq 0 ] || { echo "usage: ci-changed-areas.sh < changed-paths" >&2; exit 2; }

go=false web=false rust=false site=false
while IFS= read -r p; do
  case "$p" in
    .github/*|scripts/verify/ci-*|.gitmodules) go=true web=true rust=true site=true ;;
    testdata/*|deploy/*) go=true web=true rust=true site=true ;;
    scripts/verify/go-test-parallel.sh) go=true ;;
    docs/session-trace/*) go=true web=true rust=true ;;
    docs/configuration.md) web=true rust=true ;;
    docs/upgrading.md) web=true ;;
    protocol|protocol/*) go=true site=true ;;
    control-plane/internal/buildinfo/recipe.go) go=true rust=true ;;
    control-plane/*) go=true ;;
    web/index.html|web/src/lib/metrics.generated.json) go=true web=true ;;
    web/*) web=true ;;
    node-agent/*) rust=true ;;
    site/*) site=true ;;
  esac
done
printf 'go=%s\nweb=%s\nrust=%s\nsite=%s\n' "$go" "$web" "$rust" "$site"
