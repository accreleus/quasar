#!/usr/bin/env bash
# dev.sh — thin wrapper for building/running/testing Quasar in containers.
# The host has no GStreamer/Rust/Wayland and no Go; everything runs in a
# container with the repo mounted at /workspace. See ../CLAUDE.md.
#
# Usage:
#   scripts/dev/dev.sh image                 Build the quasar-agent-dev:latest image.
#   scripts/dev/dev.sh build [dir]           cargo build       (default dir: node-agent)
#   scripts/dev/dev.sh test  [dir]           cargo test
#   scripts/dev/dev.sh check [dir]           cargo fmt --check + clippy -D warnings
#   scripts/dev/dev.sh cargo <args...>       arbitrary cargo invocation in node-agent
#   scripts/dev/dev.sh go <args...>          go <args> in control-plane (golang image)
#   scripts/dev/dev.sh go-check              go build + vet + test in control-plane
#                                       (NO database — DB integration tests SKIP)
#   scripts/dev/dev.sh go-test-db [args...]  go build + vet + test WITH Postgres wired,
#                                       so the DB integration tests actually run
#                                       (extra args pass through to `go test`).
#                                       WARNING: targets the SHARED, STATEFUL
#                                       quasar-pg3 Postgres — this MIGRATES and
#                                       TRUNCATES it. `make test-db` (ephemeral,
#                                       per-worktree Postgres) is the preferred
#                                       default; reach for this only when you
#                                       specifically need the long-running
#                                       quasar-pg3 container.
#   scripts/dev/dev.sh run <name> [args...]  run scripts/harness/run-<name>.sh in the container
#                                       (e.g. `run st-trace`, `run p5-home`)
#   scripts/dev/dev.sh shell                 interactive bash in the container
#
# Env:
#   IMAGE=quasar-agent-dev:latest       image tag to use (Rust/GStreamer). Built
#                                       from deploy/Dockerfile.vulkan --target dev
#                                       (gst-1.28 lineage on quasar-base).
#   QUASAR_BASE_IMAGE=...               base image for `image` builds; defaults to
#                                       whatever deploy/pins.env sets (currently the
#                                       :latest channel quasar-images publishes from
#                                       `stable`). Edit pins.env, never a copy of it.
#   GO_IMAGE=golang:1.24                image for Go (control-plane); the host
#                                       and quasar-agent-dev have no Go toolchain
#   NET=host                            add --network host (needed for the
#                                       browser WebRTC path on Linux)
#   PG_NET=quasar-p3-test               docker network the test Postgres is on
#   TEST_DATABASE_URL=postgres://...    test DB DSN used by go-test-db

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE="${IMAGE:-quasar-agent-dev:latest}"
GO_IMAGE="${GO_IMAGE:-golang:1.24}"

# Postgres for control-plane DB integration tests (go-test-db). Defaults match
# the long-running test container documented in CLAUDE.md; override either to
# point at a different DB.
PG_NET="${PG_NET:-quasar-p3-test}"
TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://postgres:test@quasar-pg3:5432/quasar?sslmode=disable}"

# Common docker-run flags: mount the repo, work from /workspace.
docker_run_args=(--rm -v "$ROOT":/workspace)
[ -n "${NET:-}" ] && docker_run_args+=(--network "$NET")

# Run a command inside the container at a given workdir.
in_container() {
    local workdir="$1"; shift
    docker run "${docker_run_args[@]}" -w "$workdir" "$IMAGE" "$@"
}

# Run a command in the Go toolchain image, from control-plane. A named volume
# caches the module download across runs. -buildvcs=false avoids a VCS-stamp
# complaint when building inside the mounted repo. bash -c (not -l): a login
# shell would wipe the image's `go` PATH. When GO_PG_NET is set, attach that
# docker network and export TEST_DATABASE_URL so the DB integration tests run
# instead of skipping.
in_go_container() {
    local extra=()
    if [ -n "${GO_PG_NET:-}" ]; then
        extra+=(--network "$GO_PG_NET" -e "TEST_DATABASE_URL=$TEST_DATABASE_URL")
    fi
    # ${extra[@]+...}: an empty array under `set -u` is "unbound" on bash 3.2 (macOS).
    docker run --rm ${extra[@]+"${extra[@]}"} -v "$ROOT":/workspace -v quasar-go-mod:/go/pkg/mod \
        -e GOFLAGS=-buildvcs=false -w /workspace/control-plane "$GO_IMAGE" "$@"
}

cmd="${1:-}"; shift || true

case "$cmd" in
    image)
        # Delegates to the single build entrypoint (2026-07-26). This used to be its own
        # `docker build` with `CUDA_ENABLE=0` — a third set of build defaults alongside
        # build-image.sh and build-agent-tower.sh, which is root cause RC-3 in
        # docs/design/plans/2026-07-26-image-lineage-consolidation-spec.md. CUDA_ENABLE is
        # no longer forced off here: the whole lineage is CUDA-built so one /opt/gst and
        # one agent binary serve both vendors, and `dev` shares the same `build` stage.
        # NB build-images.sh always tags the dev role `quasar-agent-dev`; an `IMAGE=` override
        # selects which image the other dev.sh subcommands RUN, not what this builds.
        bash "$ROOT/deploy/build-images.sh" dev
        ;;
    build)
        dir="${1:-node-agent}"
        in_container "/workspace/$dir" cargo build
        ;;
    build-release)
        # #149: the production agent (docker-compose) runs target/release.
        dir="${1:-node-agent}"
        in_container "/workspace/$dir" cargo build --release
        ;;
    test)
        dir="${1:-node-agent}"
        in_container "/workspace/$dir" cargo test
        ;;
    bench)
        # SO-03: Criterion micro-benchmarks (e.g. node-agent encode-metrics hot path).
        # Pass extra args through to `cargo bench` (e.g. --save-baseline before, then
        # --baseline before to compare after).
        dir="${1:-node-agent}"
        shift || true
        in_container "/workspace/$dir" cargo bench "$@"
        ;;
    check)
        dir="${1:-node-agent}"
        in_container "/workspace/$dir" bash -lc \
            'cargo fmt --check && cargo clippy --all-targets -- -D warnings'
        ;;
    cargo)
        in_container "/workspace/node-agent" cargo "$@"
        ;;
    go)
        in_go_container go "$@"
        ;;
    go-check)
        # -p 1 serializes package test binaries: the DB integration tests share
        # one TEST_DATABASE_URL and must not truncate it concurrently.
        # NB: no database is wired here, so the DB tests SKIP — use go-test-db to
        # actually run them.
        echo "DB integration tests: SKIPPED (TEST_DATABASE_URL unset)"
        in_go_container bash -c 'go build ./... && go vet ./... && go test -p 1 ./...'
        ;;
    go-test-db)
        # Same as go-check but with Postgres attached so the control-plane DB
        # integration tests actually execute. Extra args pass through to go test
        # (e.g. `go-test-db -run TestEnsureBootstrapAdmin -v`). -p 1 is required:
        # the tests share one DB and truncate in setup.
        #
        # WARNING: this targets the SHARED, STATEFUL quasar-pg3 Postgres (see
        # PG_NET/TEST_DATABASE_URL above) — it migrates AND truncates that
        # database on every run, so concurrent worktrees contaminate each
        # other's runs. `make test-db` (scripts/dx/testdb.sh) provisions a
        # FRESH ephemeral Postgres per worktree and is the preferred default;
        # use this verb only when you deliberately need the long-running
        # quasar-pg3 container (e.g. reproducing a shared-DB-only issue).
        echo "DB integration tests: RUNNING against $TEST_DATABASE_URL"
        GO_PG_NET="$PG_NET" in_go_container \
            bash -c 'go build ./... && go vet ./... && go test -p 1 "$@" ./...' _ "$@"
        ;;
    run)
        name="${1:-}"; shift || true
        if [ -z "$name" ]; then
            echo "usage: dev.sh run <name>   (e.g. st-trace, codec-validate)" >&2
            exit 2
        fi
        script="scripts/harness/run-${name}.sh"
        if [ ! -f "$ROOT/$script" ]; then
            echo "no such script: $script" >&2
            echo "available:" >&2
            ls "$ROOT"/scripts/harness/run-*.sh | sed "s#$ROOT/#  #" >&2
            exit 2
        fi
        # Use -it so Ctrl-C reaches long-running servers.
        docker run -it "${docker_run_args[@]}" -w /workspace "$IMAGE" \
            bash "/workspace/$script" "$@"
        ;;
    shell)
        docker run -it "${docker_run_args[@]}" -w /workspace "$IMAGE" bash
        ;;
    ""|-h|--help|help)
        sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        ;;
    *)
        echo "unknown command: $cmd (try: scripts/dev/dev.sh help)" >&2
        exit 2
        ;;
esac
