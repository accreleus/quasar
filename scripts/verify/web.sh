#!/usr/bin/env bash
source scripts/verify/common.sh
source scripts/verify/web-checks.sh
cd web
run "web dependency install" npm ci

run "web schema.d.ts drift vs protocol/openapi.yaml" schema_drift_check

run "web typecheck" npm run typecheck
run "web unit tests" npm test -- run
run "web production build" npm run build

run "web build ships /enroll-host.sh and not /prepare-host.sh" served_scripts_check
