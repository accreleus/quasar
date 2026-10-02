#!/usr/bin/env bash
# Offline contract tests for validate-image.sh's REFERENCE handling: --require-digest-ref
# and --pull. A mock docker on PATH records every invocation, so these assert the two
# properties CI depends on without a daemon, a registry or an image:
#
#   1. a mutable reference is refused BEFORE anything touches docker, and
#   2. the string that gets pulled is byte-identical to the string that gets inspected
#      and reported — the artifact validated is the artifact named.
#
# The contract verdict itself is out of scope (#415): the mock answers no assertion, so
# an accepted reference ends in CONTRACT VIOLATED (exit 1), and a growing image contract
# must never break these tests. What separates "refused" from "accepted" is the exit
# code: a refused reference or a failed pull is a prerequisite error (exit 2, before any
# verdict); an accepted one reaches the verdict (exit 0 or 1).
#
# Run: bash deploy/test-validate-image-refs.sh (make verify runs it).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root/deploy/validate-image.sh"
tmp="$(mktemp -d /tmp/quasar-validate-image-refs.XXXXXX)"
digest="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
image="ghcr.io/accreleus/quasar/quasar-control-plane@sha256:$digest"

cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
# Records the argv of every call, then answers just enough of docker's surface for
# validate-image.sh to reach its report. Deliberately dumb: these tests are about
# WHICH reference is passed and in what order, not about contract assertions.
set -uo pipefail
printf '%s\n' "$*" >>"${MOCK_DOCKER_LOG:?}"
case "$1" in
  pull)   [ "${MOCK_PULL_OK:-1}" = 1 ] || { echo 'mock: pull refused' >&2; exit 1; }; exit 0 ;;
  volume) exit 0 ;;
  run)    exit 0 ;;   # no RESULT lines: every in-image assertion is simply absent
  image)
    [ "$2" = inspect ] || exit 99
    [ "${MOCK_IMAGE_PRESENT:-1}" = 1 ] || exit 1
    fmt=""
    for a in "$@"; do [ "${prev:-}" = "--format" ] && fmt="$a"; prev="$a"; done
    case "$fmt" in
      "")            echo '[{}]' ;;
      *.Size*)       echo 123456789 ;;
      *.Id*)         echo 'sha256:deadbeef' ;;
      *Healthcheck*) echo '{"Test":["CMD","true"]}' ;;
      *.Config.User*) echo 'quasar' ;;
      *Entrypoint*)  echo '["/usr/local/bin/quasar-control"] null' ;;
      *)             echo '' ;;   # Env / Labels sweeps: nothing declared
    esac
    exit 0 ;;
esac
exit 99
EOF
chmod +x "$tmp/bin/docker"

run() { # run <log-name> <args...>
  local name=$1; shift
  PATH="$tmp/bin:$PATH" MOCK_DOCKER_LOG="$tmp/$name.log" "$@"
}

fail() { echo "FAIL: $*" >&2; exit 1; }

# accepted <log-name> <args...>: the reference got past the reference checks and the run
# reached a contract verdict, whatever that verdict was.
accepted() {
  local name=$1 rc=0; shift
  run "$name" "$@" >"$tmp/$name.out" 2>&1 || rc=$?
  case "$rc" in 0|1) ;; *) fail "$name: exited $rc, expected a verdict: $(cat "$tmp/$name.out")" ;; esac
  grep -Eq 'CONTRACT (SATISFIED|VIOLATED)' "$tmp/$name.out" || fail "$name: no verdict line: $(cat "$tmp/$name.out")"
}

# refused <log-name> <args...>: a prerequisite error (exit 2), not a verdict.
refused() {
  local name=$1 rc=0; shift
  run "$name" "$@" >"$tmp/$name.out" 2>&1 || rc=$?
  [ "$rc" -eq 2 ] || fail "$name: exited $rc, expected 2 (refused): $(cat "$tmp/$name.out")"
  if grep -Eq 'CONTRACT (SATISFIED|VIOLATED)' "$tmp/$name.out"; then fail "$name: refused, yet reached a verdict"; fi
}

# ── 1. A mutable tag is refused, and docker is never reached ──────────────────
refused tagref bash "$script" --image quasar-control-plane:latest --role control \
  --require-digest-ref --no-gpu
grep -q 'must be exactly name@sha256' "$tmp/tagref.out" || fail "wrong diagnostic: $(cat "$tmp/tagref.out")"
[ ! -e "$tmp/tagref.log" ] || fail "docker was invoked before the reference was rejected"

# ── 2. repo:tag@sha256:... is legal Docker but still carries a mutable component ──
refused bothref bash "$script" --image "quasar-control-plane:v1@sha256:$digest" --role control \
  --require-digest-ref --no-gpu
[ ! -e "$tmp/bothref.log" ] || fail "docker was invoked for a repo:tag@digest ref"

# ── 3. A bare digest reference is accepted ────────────────────────────────────
accepted digestref bash "$script" --image "$image" --role control \
  --require-digest-ref --no-gpu

# ── 4. --pull pulls the EXACT reference, before the inspect ───────────────────
accepted pulled bash "$script" --image "$image" --role control \
  --pull --require-digest-ref --no-gpu
grep -Fxq -- "pull --quiet $image" "$tmp/pulled.log" \
  || fail "did not pull the exact reference; log: $(cat "$tmp/pulled.log")"
first_pull=$(grep -n '^pull ' "$tmp/pulled.log" | head -1 | cut -d: -f1)
first_inspect=$(grep -n '^image inspect' "$tmp/pulled.log" | head -1 | cut -d: -f1)
[ "$first_pull" -lt "$first_inspect" ] || fail "pull did not precede the local inspect"
# Every reference docker was handed is the one the caller named — nothing re-resolved.
strays="$(grep -E '^(pull|image inspect)' "$tmp/pulled.log" | grep -Fv -- "$image" || true)"
[ -z "$strays" ] || fail "a reference other than the named digest reached docker: $strays"

# ── 5. Without --pull nothing is fetched (the local-build path is unchanged) ──
accepted nopull bash "$script" --image "$image" --role control --no-gpu
if grep -q '^pull ' "$tmp/nopull.log"; then fail "pulled without --pull"; fi

# ── 6. A failed pull is a prerequisite error (exit 2), not a contract verdict ──
refused pullfail env MOCK_PULL_OK=0 bash "$script" --image "$image" --role control \
  --pull --no-gpu
grep -q 'could not pull' "$tmp/pullfail.out" || fail "wrong diagnostic: $(cat "$tmp/pullfail.out")"

echo 'validate-image.sh reference handling: PASS'
