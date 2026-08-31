#!/usr/bin/env bash
source scripts/verify/common.sh
# --ignore-submodules=all: protocol/ is its own repository, and its .git pointer
# is relative to the checkout's depth on the host, which /workspace does not
# reproduce. Without this the check dies with "fatal: not a git repository:
# protocol/../../../../.git/..." from any worktree, before it inspects a line.
run "repository whitespace" git diff --check --ignore-submodules=all
bash scripts/verify/control.sh
bash scripts/verify/web.sh
cd /workspace/node-agent
run "Rust formatting" cargo fmt --check
skip "Rust clippy and Linux unit tests are in ./scripts/verify.sh agent or full"
skip "hardware validation requires Tower/hermes and is never included in laptop PASS"
