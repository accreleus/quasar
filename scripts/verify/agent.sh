#!/usr/bin/env bash
source scripts/verify/common.sh
cd node-agent
run "Rust formatting" cargo fmt --check
run "Rust clippy" cargo clippy --all-targets -- -D warnings
run "Rust Linux unit tests" cargo test --all-targets
skip "GPU/media live tests require Tower or hermes; run the applicable scripts/harness/run-*-acceptance script on that host"
skip "gst-interpipe is not in Debian's gst packages, so it is absent from this image; the interpipe-backed leak-regression tests self-skip here (look for their own SKIP lines above) and run for real in the quasar-agent-dev container"
