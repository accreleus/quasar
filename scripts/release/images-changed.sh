#!/usr/bin/env bash
# images-changed.sh — which published images does a change touch? (#867)
#
#   git diff --name-only --no-renames <last built tree> HEAD | scripts/release/images-changed.sh [--all]
#
# Reads changed paths on stdin and prints `control_plane=`, `node_agent=` and
# `recovery=` as true/false. The Images workflow's `inputs` job builds an artefact
# only when it says true and otherwise carries the previous validated digest.
# `--all` (a tag release, or a dispatch with build_all) says true to everything.
#
# THE INPUT TABLE. Keep it in step with the Dockerfiles; test-images-changed.sh fails when a
# COPY source of one of them is not covered here.
#
#   quasar-control-plane  control-plane/**  web/**  deploy/enroll-host.sh
#                         deploy/control-entrypoint.sh  deploy/Dockerfile.control.prod
#   quasar-node-agent     node-agent/**  deploy/Dockerfile.vulkan  deploy/patches/**
#                         deploy/audio/**
#   quasar-recovery       node-agent/**  deploy/Dockerfile.recovery
#   all three             deploy/pins.env  deploy/lib/recipe-revision.sh  .dockerignore
#                         .github/workflows/images.yml (its build-args are inputs)
#   none                  docs, CHANGELOG, tests, scripts, CI, `protocol/` (read by tests only),
#                         compose files
#   quasar-gst-toolchain  not here: content-hashed by deploy/toolchain-hash.sh, which the
#                         workflow already uses to reuse or build it.
#
# NOT AN INPUT, on purpose: the moving quasar-base image. A base rebuild alone does not
# rebuild an image carried forward; a tag release always builds everything.
set -euo pipefail

all=false
case "${1:-}" in
  --all) all=true ;;
  '') ;;
  *) echo "usage: images-changed.sh [--all] < changed-paths" >&2; exit 2 ;;
esac

control=$all agent=$all recovery=$all
while IFS= read -r p; do
  case "$p" in
    control-plane/*|web/*|deploy/enroll-host.sh|deploy/control-entrypoint.sh|deploy/Dockerfile.control.prod) control=true ;;
  esac
  case "$p" in
    node-agent/*|deploy/Dockerfile.vulkan|deploy/patches/*|deploy/audio/*) agent=true ;;
  esac
  case "$p" in
    node-agent/*|deploy/Dockerfile.recovery) recovery=true ;;
  esac
  case "$p" in
    deploy/pins.env|deploy/lib/recipe-revision.sh|.dockerignore|.github/workflows/images.yml)
      control=true agent=true recovery=true ;;
  esac
done

printf 'control_plane=%s\nnode_agent=%s\nrecovery=%s\n' "$control" "$agent" "$recovery"
