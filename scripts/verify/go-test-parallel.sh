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
# GTP_ONLY and GTP_SKIP, extended regexes on the import path, choose a subset of the
# packages, so CI can give a slow package a runner of its own (#928).
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
if [ -n "${GTP_ONLY:-}" ]; then grep -E "$GTP_ONLY" "$GTP_LOGS/packages" > "$GTP_LOGS/picked" || true; mv "$GTP_LOGS/picked" "$GTP_LOGS/packages"; fi
if [ -n "${GTP_SKIP:-}" ]; then grep -vE "$GTP_SKIP" "$GTP_LOGS/packages" > "$GTP_LOGS/picked" || true; mv "$GTP_LOGS/picked" "$GTP_LOGS/packages"; fi
[ -s "$GTP_LOGS/packages" ] || { echo "go-test-parallel: no test packages found (GTP_ONLY='${GTP_ONLY:-}' GTP_SKIP='${GTP_SKIP:-}')" >&2; exit 2; }

# Packages that run alone, after the others. An entry is a package whose tests missed a
# wall-clock wait beside other packages on the shared server and never alone; it names its
# issue and comes off when that issue is fixed. agentws: two 10 s waits missed (#925).
# This is a stopgap per package, not a fix: the fix is in the test.
alone='/internal/agentws$'
grep -E "$alone" "$GTP_LOGS/packages" > "$GTP_LOGS/alone" || true
grep -vE "$alone" "$GTP_LOGS/packages" > "$GTP_LOGS/together" || true

# The packages share one server, so a test that reads the server-wide views
# (pg_stat_activity, pg_locks) sees the other packages' sessions too and can stop waiting
# on someone else's lock. Each read must be scoped, in the rest of its SQL string, to its
# own database (current_database()) or a known backend (pid = $n). Go and SQL line
# comments are dropped first so a comment cannot satisfy the rule.
unscoped="$(find . -name '*_test.go' -exec perl -0777 -ne '
  s#//[^\n]*##g; s#--[^\n]*##g;
  while (/(?:FROM|JOIN)\s+pg_(?:stat_activity|locks)\b(?=([^`"]*))/g) {
    my $rest = $1;
    next if $rest =~ /current_database\(\)|\bpid\s*=\s*\$/;
    my $line = 1 + (substr($_, 0, $-[0]) =~ tr/\n//);
    print "$ARGV:$line\n";
  }' {} +)"
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
# Compile every test binary first, at full parallelism: a compile error stops here with
# one message instead of in whichever packages it reaches, and the timed runs below do not
# share the CPU with compilers. -run '^$' runs no test, only each package's TestMain.
mapfile -t picked < "$GTP_LOGS/packages"
if ! go test "$@" -run '^$' "${picked[@]}" > "$GTP_LOGS/build.log" 2>&1; then
  cat "$GTP_LOGS/build.log"
  echo "go-test-parallel: the test binaries did not build (or a TestMain failed)" >&2
  exit 1
fi
echo "go-test-parallel: test binaries built in $((SECONDS - start)) s"
xargs -P "$jobs" -I{} bash -c 'run_package "$@"' _ {} "$@" < "$GTP_LOGS/together" || true
xargs -P 1 -I{} bash -c 'run_package "$@"' _ {} "$@" < "$GTP_LOGS/alone" || true
alone_note=""
[ -s "$GTP_LOGS/alone" ] && alone_note=", then alone: $(tr '\n' ' ' < "$GTP_LOGS/alone")"
echo "go-test-parallel: $(wc -l < "$GTP_LOGS/together") packages $jobs at a time${alone_note} ($((SECONDS - start)) s)"
# Every package must have been started: a run_package that never ran must not read as green.
touch "$GTP_LOGS/ran"
ran="$(sort -u "$GTP_LOGS/ran" | wc -l)"
if [ "$ran" -ne "$(wc -l < "$GTP_LOGS/packages")" ]; then
  echo "go-test-parallel: only $ran of $(wc -l < "$GTP_LOGS/packages") packages ran" >&2
  exit 1
fi
if [ -s "$GTP_LOGS/failed" ]; then
  echo "go-test-parallel: FAILED packages:"
  sort "$GTP_LOGS/failed" | sed 's/^/  /'
  exit 1
fi
