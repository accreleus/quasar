#!/usr/bin/env bash
# Builds the node-agent lib test binary and prints its path as the last line;
# `scripts/verify.sh uinput` then runs it as root with /dev/uinput passed in.
source scripts/verify/common.sh
cd node-agent
cargo test --lib --no-run --message-format=json \
  | jq -r 'select(.reason == "compiler-artifact" and .profile.test and .target.kind == ["lib"]) | .executable' \
  | tail -n1
