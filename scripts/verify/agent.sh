#!/usr/bin/env bash
source scripts/verify/common.sh
cd node-agent
run "Rust formatting" cargo fmt --all --check
run "Rust clippy" cargo clippy --workspace --all-targets -- -D warnings
run "Rust Linux unit tests" cargo test --workspace --all-targets
skip "GPU/media live tests require the lab host or the aux host; run the applicable scripts/harness/run-*-acceptance script on that host"
skip "gst-interpipe is not in Debian's gst packages, so it is absent from this image; the interpipe-backed leak-regression tests self-skip here (look for their own SKIP lines above) and run for real in the quasar-agent-dev container"
skip "libnice and gst-interpipe are absent from this image; the live two-peer takeover tests (session::pipeline::takeover_tests, #535) self-skip here with their own SKIP line and run for real in the quasar-agent-dev container"
