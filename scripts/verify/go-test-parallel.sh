#!/usr/bin/env bash
# go-test-parallel.sh: the control-plane Go tests, several packages at once, each
# package against its own database (#919).
#
#   TEST_DATABASE_URL=postgres://user:pass@host:port/db?sslmode=disable \
#     scripts/verify/go-test-parallel.sh [-j N] [go test flags...]
#
# `go test -p 1 ./...` ran one package at a time because every package shares the one
# database and truncates it in setup. Here each package gets a fresh, empty database
# on the same server (its tests migrate it themselves), so packages cannot see each
# other's rows and run side by side. TEST_DATABASE_URL names the server and a database
# the user can connect to; that database is only used to create the others.
#
# Needs `psql` and `flock`. -j defaults to the CPU count. Flags after -j go to every
# `go test`. Exit status is 1 when any package failed; the failed ones are listed last.
set -euo pipefail

jobs="$(nproc)"
if [ "${1:-}" = -j ]; then jobs="$2"; shift 2; fi
: "${TEST_DATABASE_URL:?TEST_DATABASE_URL must name the Postgres server to create the test databases on}"
for tool in psql flock go; do
  command -v "$tool" >/dev/null || { echo "go-test-parallel: $tool is required" >&2; exit 2; }
done

cd "$(dirname "$0")/../../control-plane"

# postgres://u:p@h:port/db?query -> server part, database, query (kept for every database).
case "$TEST_DATABASE_URL" in
  postgres://*/*|postgresql://*/*) ;;
  *) echo "go-test-parallel: TEST_DATABASE_URL must be postgres://user:pass@host:port/db[?...]" >&2; exit 2 ;;
esac
url="${TEST_DATABASE_URL%%\?*}"
query=""
case "$TEST_DATABASE_URL" in *\?*) query="?${TEST_DATABASE_URL#*\?}" ;; esac
export GTP_SERVER="${url%/*}" GTP_QUERY="$query"
GTP_LOGS="$(mktemp -d)"
export GTP_LOGS
trap 'rm -rf "$GTP_LOGS"' EXIT

# Packages that have tests, the ones with the most test files first so the long ones
# start early and do not finish alone at the end.
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{len .TestGoFiles}} {{len .XTestGoFiles}} {{.ImportPath}}{{end}}' ./... |
  awk 'NF == 3 { print $1 + $2, $3 }' | sort -rn | cut -d' ' -f2 > "$GTP_LOGS/packages"
[ -s "$GTP_LOGS/packages" ] || { echo "go-test-parallel: no test packages found" >&2; exit 2; }

# The packages share one server, so a test that reads the server-wide views
# (pg_stat_activity, pg_locks) sees the other packages' sessions too and can stop waiting
# on someone else's lock. Each read must be scoped to its own database or a known
# backend pid within three lines of the FROM/JOIN.
unscoped="$(find . -name '*_test.go' -exec awk '
  FNR == 1 { w = 0 }
  /(FROM|JOIN)[ \t]+pg_(stat_activity|locks)/ { w = 4; at = FILENAME ":" FNR }
  w > 0 { if ($0 ~ /current_database\(\)|pid ?= ?\$/) w = 0; else if (--w == 0) print at }
' {} +)"
if [ -n "$unscoped" ]; then
  echo "go-test-parallel: these reads of pg_stat_activity/pg_locks are not scoped to current_database():" >&2
  while IFS= read -r u; do echo "  $u" >&2; done <<<"$unscoped"
  exit 2
fi

# Distinct database names, or two packages would share one.
names="$(sed 's#^.*/control-plane/##' "$GTP_LOGS/packages" | tr -c '[:alnum:]\n' '_' | tr '[:upper:]' '[:lower:]' | sed 's/^/gtp_/' | cut -c1-63)"
dupes="$(sort <<<"$names" | uniq -d)"
[ -z "$dupes" ] || { echo "go-test-parallel: two packages map to one database name: $dupes" >&2; exit 2; }

run_package() {
  local pkg=$1; shift
  # A database name from the package path: lower case, [a-z0-9_], at most 63 bytes.
  local db
  db="gtp_$(printf '%s' "${pkg#*/control-plane/}" | tr -c '[:alnum:]' '_' | tr '[:upper:]' '[:lower:]')"
  db="${db:0:63}"
  local log="$GTP_LOGS/$db.log" rc=0
  echo "$pkg" >> "$GTP_LOGS/ran"
  {
    PGOPTIONS="-c client_min_messages=warning" psql "$TEST_DATABASE_URL" -X -q -v ON_ERROR_STOP=1 \
      -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" -c "CREATE DATABASE $db" &&
      TEST_DATABASE_URL="$GTP_SERVER/$db$GTP_QUERY" go test "$@" "$pkg"
  } >"$log" 2>&1 || rc=$?
  # One package's output at a time, never interleaved with another's.
  flock "$GTP_LOGS/.print" cat "$log"
  [ "$rc" -eq 0 ] || echo "$pkg" >> "$GTP_LOGS/failed"
}
export -f run_package

start=$SECONDS
# Compile every test binary first, at full parallelism, so the timing-sensitive DB tests
# below do not run beside four compilers (under that contention a lock-ordering test in
# agentws missed its 10 s wait). -run '^$' runs no test; a compile error stops here.
if ! go test "$@" -run '^$' ./... > "$GTP_LOGS/build.log" 2>&1; then
  cat "$GTP_LOGS/build.log"
  echo "go-test-parallel: the test binaries did not build" >&2
  exit 1
fi
echo "go-test-parallel: test binaries built in $((SECONDS - start)) s"
xargs -P "$jobs" -I{} bash -c 'run_package "$@"' _ {} "$@" < "$GTP_LOGS/packages" || true
echo "go-test-parallel: $(wc -l < "$GTP_LOGS/packages") packages, $jobs at a time, $((SECONDS - start)) s"
# Every package must have been started: a run_package that never ran must not read as green.
ran="$(sort -u "$GTP_LOGS/ran" 2>/dev/null | wc -l)"
if [ "$ran" -ne "$(wc -l < "$GTP_LOGS/packages")" ]; then
  echo "go-test-parallel: only $ran of $(wc -l < "$GTP_LOGS/packages") packages ran" >&2
  exit 1
fi
if [ -s "$GTP_LOGS/failed" ]; then
  echo "go-test-parallel: FAILED packages:"
  sort "$GTP_LOGS/failed" | sed 's/^/  /'
  exit 1
fi
