#!/usr/bin/env bash
# images-changed.sh — which published images does a change touch? (#867)
#
#   git diff --name-only --no-renames <last built tree> HEAD | scripts/release/images-changed.sh
#
# Reads changed paths on stdin and prints `control_plane=`, `node_agent=` and
# `recovery=` as true/false: does the change feed that image? The Images workflow's
# `inputs` job builds every image when ANY says true (a build is one commit for every
# image) and builds nothing when all say false.
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
# NOT A PATH INPUT, on purpose: the moving quasar-base image and deploy/toolchain-hash.sh.
# They move the toolchain's content hash instead, and the workflow reads that: a run whose
# toolchain tag is not published builds everything, as do a tag release and a `force`
# dispatch. Those decisions are the workflow's, not this script's.
#
# Patterns are shell `case` globs, where `*` also matches `/`: `deploy/audio/*` is every
# file under it, which is what the `**` above means.
set -euo pipefail

[ $# -eq 0 ] || { echo "usage: images-changed.sh < changed-paths" >&2; exit 2; }

control=false agent=false recovery=false
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
