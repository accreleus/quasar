#!/usr/bin/env bash
source scripts/verify/common.sh
run "PostgreSQL readiness" pg_isready -d "$TEST_DATABASE_URL"
cd control-plane
run "serialized PostgreSQL integration tests" go test -p 1 ./...
