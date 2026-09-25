#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE=(docker compose -f "$ROOT/scripts/verify/docker-compose.devtools.yml")
export DEVTOOLS_UID="${DEVTOOLS_UID:-$(id -u)}"
export DEVTOOLS_GID="${DEVTOOLS_GID:-$(id -g)}"

# A git worktree's .git is a FILE holding "gitdir: <abs path>" that points into
# the main clone's .git/worktrees/<name> — outside the /workspace bind mount. So
# every git call in the container dies with "fatal: not a git repository", which
# takes down verify before a single test runs. Bind the git common dir at its
# own host path (the gitdir lives under it, and the refs inside it are absolute)
# so worktrees verify exactly like a plain clone. Read-write: git writes index
# and config state there.
GIT_MOUNT=()
if [ -f "$ROOT/.git" ]; then
  git_common="$(git -C "$ROOT" rev-parse --git-common-dir 2>/dev/null || true)"
  case "$git_common" in
    "") ;;
    /*) GIT_MOUNT=(-v "$git_common:$git_common") ;;
    *)  GIT_MOUNT=(-v "$ROOT/$git_common:$ROOT/$git_common") ;;
  esac
fi

# Every use below expands GIT_MOUNT as ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"}, never
# as a bare "${GIT_MOUNT[@]}".
#
# In a plain clone (not a worktree) GIT_MOUNT stays empty, and bash 3.2 — which
# is the /bin/bash macOS still ships — treats "${empty_array[@]}" as an unset
# variable under `set -u` and aborts:
#
#   scripts/verify.sh: line NN: GIT_MOUNT[@]: unbound variable
#
# That killed every `make test-rust` / `make test-go` run from a Mac before a
# single test ran. bash >= 4.4 dropped the misbehaviour, so this is invisible on
# the Linux boxes and only ever bites locally. The ${x[@]+"${x[@]}"} form is the
# portable guard: it expands to nothing when the array is empty, and to the
# properly-quoted elements when it is not.

cmd="${1:-quick}"
case "$cmd" in
  build)
    export DEVTOOLS_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    "${COMPOSE[@]}" build devtools
    ;;
  versions)
    "${COMPOSE[@]}" run --rm --no-deps ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"} devtools bash scripts/verify/versions.sh
    docker image inspect quasar-devtools:local --format 'image digest: {{.Id}}\nimage created: {{.Created}}\nimage size bytes: {{.Size}}'
    ;;
  reset-db)
    "${COMPOSE[@]}" down --remove-orphans
    ;;
  db|full)
    if [ "$cmd" = full ]; then
      if "${COMPOSE[@]}" config --quiet; then
        echo "PASS — Compose configuration"
      else
        echo "FAIL — Compose configuration" >&2
        exit 1
      fi
    fi
    "${COMPOSE[@]}" up -d --wait postgres
    trap '"${COMPOSE[@]}" stop postgres >/dev/null 2>&1 || true' EXIT
    "${COMPOSE[@]}" run --rm ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"} devtools bash "scripts/verify/$cmd.sh"
    ;;
  quick|web|control|agent)
    "${COMPOSE[@]}" run --rm --no-deps ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"} devtools bash "scripts/verify/$cmd.sh"
    ;;
  uinput)
    # The node-agent's real-kernel uinput tests (#[ignore]d in `make test-rust`).
    # `compose run` cannot pass a device or a cgroup rule, so the binary is built
    # through compose and run with plain `docker run`, as root: /dev/uinput, the
    # fake-udev mknod and /run/udev/data all need it.
    if [ ! -c /dev/uinput ]; then
      echo "FAIL — /dev/uinput is absent on this host: sudo modprobe uinput, or run on a host that has it" >&2
      exit 1
    fi
    bin="$("${COMPOSE[@]}" run --rm --no-deps -T ${GIT_MOUNT[@]+"${GIT_MOUNT[@]}"} devtools bash scripts/verify/uinput.sh | tail -n1)"
    case "$bin" in
      /cache/cargo-target/*) ;;
      *) echo "FAIL — could not build the node-agent test binary (got '$bin')" >&2; exit 1 ;;
    esac
    docker run --rm --ulimit core=0 --user 0:0 \
      --device /dev/uinput --device-cgroup-rule 'c 13:* rmw' \
      -e QUASAR_REQUIRE_UINPUT=1 -v quasar-cargo-target:/cache/cargo-target:ro \
      quasar-devtools:local "$bin" uinput_ --ignored --test-threads=1
    ;;
  *)
    echo "usage: $0 [quick|web|control|agent|uinput|db|full|versions|build|reset-db]" >&2
    exit 2
    ;;
esac
