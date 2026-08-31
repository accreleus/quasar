#!/usr/bin/env bash
# shellcheck shell=bash
#
# scripts/dx/testdb.sh — run the control-plane DB integration tests against a
# FRESH ephemeral Postgres owned by this worktree.
#
#   make test-db
#
# Why ephemeral and per-instance:
#   * The control-plane DB tests t.Skip() unless TEST_DATABASE_URL is set, so a
#     green run without one means they were SKIPPED, not passed. A CP ticket that
#     touches the DB is not done until this target is green.
#   * They share one database and TRUNCATE in setup, so `-p 1` is mandatory —
#     package binaries must not run concurrently.
#   * A shared long-lived Postgres is stateful across branches (migration version
#     advances, truncation wipes seeds). A fresh container per run removes that
#     whole class of contamination.
#   * Container name and port are instance-derived, so two worktrees can run this
#     at the same time (#398 generalized).
#   * Two CONCURRENT invocations in the SAME worktree also need to not collide
#     (#466): QUASAR_INSTANCE defaults to a hash of the worktree path, which is
#     identical for both, so without per-invocation isolation the second run's
#     `docker rm -f` of "the stale container from a killed run" (below) tears
#     down the FIRST run's live Postgres mid-test (SASL auth failures,
#     connection-refused cascades on whichever run loses the race). Unless the
#     operator pins QUASAR_INSTANCE explicitly (the documented escape hatch for
#     coordinating with one specific concurrent run), this script mixes a
#     pid+random suffix into its OWN instance id so concurrent runs never share
#     a container name or a port-search starting point.
#
# The container is removed on EVERY exit path, signals included.

set -euo pipefail
# shellcheck source=scripts/dx/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

dx_require_local test-db

TARGET=test-db

# Per-invocation instance id (#466). QUASAR_INSTANCE explicitly set by the
# operator wins outright (deterministic name/port, as before); otherwise mix
# in this process's pid and a random component so two `make test-db` runs in
# one worktree never derive the same container name or port hint.
if [ -n "$DX_INSTANCE_EXPLICIT" ]; then
  TESTDB_INSTANCE="$QUASAR_INSTANCE"
else
  if dx_have openssl; then
    TESTDB_RAND="$(openssl rand -hex 4)"
  else
    TESTDB_RAND="$(dx_sha256 "$QUASAR_INSTANCE-$$-$RANDOM-$(date +%s)")"
    TESTDB_RAND="${TESTDB_RAND:0:8}"
  fi
  TESTDB_INSTANCE="${QUASAR_INSTANCE}-$$-${TESTDB_RAND}"
fi

PG_NAME="qpg-${TESTDB_INSTANCE}"
PG_IMAGE="${DX_TESTDB_IMAGE:-postgres:16-alpine}"
PG_USER=postgres
PG_DB=quasar_test

# Port search start: the deterministic per-worktree hint, unless this is an
# unpinned per-invocation run — then spread the starting point so concurrent
# invocations don't all begin scanning (and racing) from the same port. The
# retry loop around `docker run` below is the real collision-safety net
# (dx_free_port's own bindable-check is inherently TOCTOU against another
# process's simultaneous `docker run -p`).
PORT_HINT="$DX_TESTDB_PORT_HINT"
if [ -z "$DX_INSTANCE_EXPLICIT" ]; then
  PORT_HINT=$(( DX_TESTDB_PORT_HINT + (RANDOM % 100) ))
fi

if ! dx_have docker || ! docker info >/dev/null 2>&1; then
  dx_fail docker "daemon not reachable — cannot start the ephemeral Postgres"
  dx_result "$TARGET"
fi
if ! dx_have go; then
  dx_fail go "not on PATH — needed to run the control-plane tests"
  dx_result "$TARGET"
fi

# Never printed, never persisted.
if dx_have openssl; then
  PG_PASS="$(openssl rand -hex 16)"
else
  PG_PASS="$(dx_sha256 "$TESTDB_INSTANCE-$$-$(date +%s)")"
  PG_PASS="${PG_PASS:0:32}"
fi

# shellcheck disable=SC2329  # invoked by the trap below, not by name
cleanup() {
  docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM HUP

# A stale container from a killed run would hold the name.
docker rm -f "$PG_NAME" >/dev/null 2>&1 || true

# Pick a port and start the container, retrying with the next port on
# failure (#466): dx_free_port's bind-check happens before `docker run`
# publishes the port, so two concurrent invocations can both see a port as
# free and then race to claim it. A handful of retries absorbs that race
# without needing a global lock.
PG_START_ATTEMPTS=0
PG_START_MAX=10
PG_STARTED=0
PORT=""
while [ "$PG_START_ATTEMPTS" -lt "$PG_START_MAX" ]; do
  CANDIDATE_PORT="$(dx_free_port "$PORT_HINT")" || {
    dx_fail port "no free port found near $PORT_HINT"
    dx_result "$TARGET"
  }
  if docker run -d --name "$PG_NAME" \
       -e POSTGRES_PASSWORD="$PG_PASS" \
       -e POSTGRES_USER="$PG_USER" \
       -e POSTGRES_DB="$PG_DB" \
       -p "127.0.0.1:${CANDIDATE_PORT}:5432" \
       "$PG_IMAGE" >/dev/null 2>&1; then
    PORT="$CANDIDATE_PORT"
    PG_STARTED=1
    break
  fi
  docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
  PORT_HINT=$(( CANDIDATE_PORT + 1 ))
  PG_START_ATTEMPTS=$(( PG_START_ATTEMPTS + 1 ))
done

if [ "$PG_STARTED" -eq 1 ]; then
  dx_pass postgres "ephemeral $PG_IMAGE started as $PG_NAME on 127.0.0.1:$PORT"
else
  dx_fail postgres "could not start $PG_IMAGE as $PG_NAME after $PG_START_MAX port attempts (contention near $DX_TESTDB_PORT_HINT)"
  dx_result "$TARGET"
fi

# Wait for readiness (pg_isready inside the container — no host client needed).
ready=0
for _ in $(seq 1 60); do
  if docker exec "$PG_NAME" pg_isready -U "$PG_USER" -d "$PG_DB" >/dev/null 2>&1; then
    ready=1; break
  fi
  sleep 1
done
if [ "$ready" -eq 1 ]; then
  dx_pass readiness "postgres accepted connections"
else
  dx_fail readiness "postgres never became ready — check: docker logs $PG_NAME"
  dx_result "$TARGET"
fi

# The URL carries the password, so it is exported but never echoed.
export TEST_DATABASE_URL="postgres://${PG_USER}:${PG_PASS}@127.0.0.1:${PORT}/${PG_DB}?sslmode=disable"
dx_info "TEST_DATABASE_URL exported (value withheld) -> 127.0.0.1:${PORT}/${PG_DB}"

if [ ! -f "$DX_ROOT/protocol/openapi.yaml" ]; then
  dx_warn submodule "protocol/ is not initialized — TestOpenAPIDrift will fail environmentally; run: make init"
fi

# `go test` invoked directly (not through a wrapper that could mask the exit
# code). -p 1 is mandatory: the packages share this database.
#
# -count=1 is equally mandatory, and for a reason this repo has already paid
# for: a CACHED TestOpenAPIDrift result reported green on a branch where the
# test genuinely failed (2026-08-07, the setup/image-catalog branch), because
# the source had not changed even though the protocol/ submodule pin had. Go's
# test cache does not key on a submodule's contents, so any test that reads a
# file outside its package — the drift test reads protocol/openapi.yaml — can
# be served a stale pass. A verification target that can report a cached green
# is not a verification target.
cd "$DX_ROOT/control-plane"
if go test -p 1 -count=1 ./...; then
  dx_pass tests "control-plane go test -p 1 -count=1 ./... green (DB tests actually ran, no cache)"
else
  dx_fail tests "control-plane go test -p 1 -count=1 ./... failed"
fi

dx_result "$TARGET" "pg_port=$PORT" "pg_name=$PG_NAME"
