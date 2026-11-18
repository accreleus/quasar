#!/usr/bin/env bash
# Self-test for scripts/verify/ci-apt-install.sh (#1580): on a download stall the script must
# switch to a DIFFERENT apt mirror, not retry the same stalled one. Runs with a stubbed apt and
# no network, no root: the script's apt config and archives dirs are pointed at temp files via
# CI_APT_CONF_DIR / CI_APT_ARCHIVES, and apt-get/sudo are PATH stubs.
#
# Reversion-proof: it asserts the second download used a different mirror than the first. Revert
# the mirror-switching logic (the original loop retries the same mirror) and this assertion fails.
set -uo pipefail

script=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/verify/ci-apt-install.sh}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/ci-apt-selftest.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*" >&2; exit 1; }

CONF="$WORK/conf"            # fake apt config: sources.list + apt.conf.d
DEBS="$WORK/debs"
ARCHIVES="$WORK/archives"
BIN="$WORK/bin"
LOG="$WORK/apt-get.log"
LIST="$WORK/packages.txt"
mkdir -p "$CONF/apt.conf.d" "$CONF/sources.list.d" "$DEBS" "$ARCHIVES" "$BIN"

# A runner whose regional mirror (ae.archive.ubuntu.com) is the one that stalls.
printf 'deb http://ae.archive.ubuntu.com/ubuntu/ noble main\n' > "$CONF/sources.list"
printf 'libgstreamer1.0-dev\nlibgstreamer-plugins-bad1.0-dev\n' > "$LIST"

# sudo: run the real command (strip the leading "sudo "); timeout/tee/cp stay real.
printf '#!/usr/bin/env bash\nexec "$@"\n' > "$BIN/sudo"
# dpkg-query: everything looks installed (only the offline path uses it).
printf '#!/usr/bin/env bash\nprintf "install ok installed\\n"\n' > "$BIN/dpkg-query"
# apt-get: record each call as "<subcommand> <mirror> <download?>", then behave per APT_STALL.
# apt passes global -o options BEFORE the subcommand, so scan every argument rather than reading
# $1; the mirror is read from the -o Dir::Etc::sourcelist= file the script generates for a fallback.
cat > "$BIN/apt-get" <<'STUB'
#!/usr/bin/env bash
log="${APT_LOG:?APT_LOG unset}"
mirror="DEFAULT"; sub=""; dl=""
prev=""; for a in "$@"; do
  [ "$prev" = -o ] && { prev=""; continue; }
  case "$a" in -o) prev=-o ;; -*) ;; *) sub="$a"; break ;; esac
done
for a in "$@"; do
  case "$a" in Dir::Etc::sourcelist=*) f="${a#*=}"; [ -r "$f" ] || continue
    grep -q 'azure\.archive\.ubuntu\.com' "$f" && mirror="azure.archive.ubuntu.com"
    [ "$mirror" = DEFAULT ] && grep -q 'archive\.ubuntu\.com' "$f" && mirror="archive.ubuntu.com" ;; esac
done
for a in "$@"; do [ "$a" = --download-only ] && dl=download; done
printf '%s %s %s\n' "${sub:-?}" "$mirror" "$dl" >> "$log"
# A --download-only install is the step the issue stalls on: it fails only on the runner default
# when APT_STALL=1; any fallback mirror answers. Everything else succeeds.
if [ -n "$dl" ] && [ "${APT_STALL:-0}" = 1 ] && [ "$mirror" = DEFAULT ]; then exit 1; fi
exit 0
STUB
chmod +x "$BIN/apt-get" "$BIN/sudo" "$BIN/dpkg-query"

download_mirrors() { grep ' download$' "$LOG" 2>/dev/null | awk '{print $2}'; }

run_scenario() { # run_scenario <stall 0|1>
  : > "$LOG"
  printf 'deb http://ae.archive.ubuntu.com/ubuntu/ noble main\n' > "$CONF/sources.list"
  APT_STALL="${1}" APT_LOG="$LOG" \
    CI_APT_CONF_DIR="$CONF" CI_APT_ARCHIVES="$ARCHIVES" \
    PATH="$BIN:$PATH" bash "$script" "$LIST" "$DEBS"
}

# ── Scenario 1: the runner's default mirror stalls; the retry must switch mirrors (#1580) ──
run_scenario 1 || fail "script should recover by switching mirror (non-zero exit)"
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -ge 2 ] || fail "expected at least two download attempts, saw: $(download_mirrors | tr '\n' ' ')"
[ "${dms[0]}" = "DEFAULT" ] || fail "first download should use the runner's default mirror, got ${dms[0]}"
[ "${dms[1]}" != "${dms[0]}" ] || fail "did not switch mirror on stall: ${dms[0]} then ${dms[1]}"
[ "${dms[1]}" = "archive.ubuntu.com" ] || fail "expected archive.ubuntu.com on retry, got ${dms[1]}"
pass "stall switches to a different mirror (DEFAULT -> ${dms[1]})"

# ── Scenario 2: happy path — no stall means the mirror is never switched ──
run_scenario 0 || fail "happy path should exit zero"
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -eq 1 ] || fail "happy path should download once, saw ${#dms[@]} attempts: $(download_mirrors | tr '\n' ' ')"
[ "${dms[0]}" = "DEFAULT" ] || fail "happy path should use the default mirror, got ${dms[0]}"
pass "no stall leaves the runner's mirror untouched"

# ── Scenario 3: both fallback mirrors also stall; the script gives up after three ──
cat > "$BIN/apt-get" <<'STUB'
#!/usr/bin/env bash
log="${APT_LOG:?}"
mirror="DEFAULT"; sub=""; dl=""
prev=""; for a in "$@"; do [ "$prev" = -o ] && { prev=""; continue; }; case "$a" in -o) prev=-o ;; -*) ;; *) sub="$a"; break ;; esac; done
for a in "$@"; do case "$a" in Dir::Etc::sourcelist=*) f="${a#*=}"; [ -r "$f" ] && grep -q 'archive\.ubuntu\.com' "$f" && mirror="fallback";; esac; done
for a in "$@"; do [ "$a" = --download-only ] && dl=download; done
printf '%s %s %s\n' "${sub:-?}" "$mirror" "$dl" >> "$log"
[ -n "$dl" ] && exit 1    # every download stalls
exit 0
STUB
chmod +x "$BIN/apt-get"
if run_scenario 1; then
  fail "script should exit non-zero when every mirror stalls"
fi
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -eq 3 ] || fail "expected three download attempts before giving up, saw ${#dms[@]}"
[ "${dms[0]}" = "DEFAULT" ] && [ "${dms[1]}" = "fallback" ] && [ "${dms[2]}" = "fallback" ] \
  || fail "tried default then the two fallback mirrors: $(download_mirrors | tr '\n' ' ')"
pass "all mirrors stall: three distinct mirrors tried, then gives up"

echo "test-ci-apt-install: ok"
