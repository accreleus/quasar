#!/usr/bin/env bash
source scripts/verify/common.sh
cd web
run "web production dependency audit" npm audit --omit=dev --audit-level=high
cd /workspace
skip "Go vulnerability scan requires govulncheck in devtools; run: govulncheck ./... from control-plane"
skip "Rust vulnerability scan requires cargo-audit in devtools; run: cargo audit from node-agent"
skip "container vulnerability scan requires a scanner; run: trivy image quasar-devtools:local"
