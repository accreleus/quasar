#!/usr/bin/env bash
# Self-test for scripts/verify/ci-apt-install.sh (#1580, #1581 r2). Network-free, no root: the
# script's apt config and archives dirs are pointed at temp files via CI_APT_CONF_DIR /
# CI_APT_ARCHIVES, and apt-get/sudo/dpkg-query are PATH stubs.
#
# Guards asserted here, each reversion-proof (revert only the linked fix and the assertion fails):
#   * argv (finding 1, #1581 r2): every apt -o option is a standalone "-o" followed by one
#     "Dir::Etc::...=value"; the script never passes "-o Dir::Etc::...=value" as one glued arg,
#     which real apt treats as an unknown option and ignores, so the mirror switch would silently
#     not apply.
#   * the final real install uses the SAME mirror the successful download used (finding 2): after a
#     switched-mirror update apt's lists hold that mirror's indices, so the default-sources install
#     would report "Unable to locate package".
#   * the EXIT cleanup trap removes only the script's own $work, nothing beside it (finding 3).
#   * with CI_APT_CONF_DIR unset, sudo tee targets the real /etc/apt path (finding 3).
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
SUDO_LOG="$WORK/sudo.log"
LIST="$WORK/packages.txt"
mkdir -p "$CONF/apt.conf.d" "$CONF/sources.list.d" "$DEBS" "$ARCHIVES" "$BIN"
printf 'libgstreamer1.0-dev\nlibgstreamer-plugins-bad1.0-dev\n' > "$LIST"

# sudo: by default run the real command (strip the leading "sudo "); timeout/tee/cp stay real.
# %% so the outer printf emits a literal %s into the stub.
sudo_exec() {
  printf '#!/usr/bin/env bash\n[ -n "${SUDO_LOG:-}" ] && printf "SUDO %%s\n" "$@" >> "$SUDO_LOG"\nexec "$@"\n' > "$BIN/sudo"
  chmod +x "$BIN/sudo"
}
# sudo: record its args only, run nothing. Lets the unset-path scenario point tee at the real
# /etc/apt without actually writing there.
sudo_capture() {
  printf '#!/usr/bin/env bash\nprintf "SUDO %%s\n" "$@" >> "$SUDO_LOG"\n' > "$BIN/sudo"
  chmod +x "$BIN/sudo"
}
# dpkg-query: everything looks installed (only the offline path uses it).
printf '#!/usr/bin/env bash\nprintf "install ok installed\\n"\n' > "$BIN/dpkg-query"; chmod +x "$BIN/dpkg-query"

# apt-get stub: record "<subcommand> <mirror> <download?>", plus "argv BAD <detail>" if the
# script's -o argv is glued, then behave per APT_STALL / STALL_ALL. apt passes global -o options
# BEFORE the subcommand, so scan every argument; the mirror is read from the -o Dir::Etc::sourcelist=
# file the script generates for a fallback.
write_stub() {
cat > "$BIN/apt-get" <<'STUB'
#!/usr/bin/env bash
log="${APT_LOG:?APT_LOG unset}"
mirror="DEFAULT"; sub=""; dl=""
# finding 1: every -o standalone, followed by one Dir::Etc::...=value; no glued "-o X".
args=("$@"); n=${#args[@]}; ok=1; bad=""
for ((i=0; i<n; i++)); do
  if [ "${args[i]}" = -o ]; then
    if [ $((i+1)) -ge "$n" ]; then ok=0; bad="bare -o no value";
    else case "${args[i+1]}" in Dir::Etc::*=*) ((i++)) ;; *) ok=0; bad="-o value not key=value:${args[i+1]}";; esac; fi
  else
    case "${args[i]}" in -o*) ok=0; bad="glued -o option:${args[i]}";; esac
  fi
done
[ "$ok" = 1 ] || printf 'argv BAD %s\n' "$bad" >> "$log"
prev=""; for a in "$@"; do [ "$prev" = -o ] && { prev=""; continue; }; case "$a" in -o) prev=-o ;; -*) ;; *) sub="$a"; break ;; esac; done
for a in "$@"; do case "$a" in Dir::Etc::sourcelist=*) f="${a#*=}"; [ -r "$f" ] || continue
  grep -q 'azure\.archive\.ubuntu\.com' "$f" && mirror="azure.archive.ubuntu.com"
  [ "$mirror" = DEFAULT ] && grep -q 'archive\.ubuntu\.com' "$f" && mirror="archive.ubuntu.com" ;; esac; done
for a in "$@"; do [ "$a" = --download-only ] && dl=download; done
printf '%s %s %s\n' "${sub:-?}" "$mirror" "$dl" >> "$log"
# A --download-only install is the step the issue stalls on.
if [ -n "$dl" ]; then
  [ "${STALL_ALL:-0}" = 1 ] && exit 1
  [ "${APT_STALL:-0}" = 1 ] && [ "$mirror" = DEFAULT ] && exit 1
fi
exit 0
STUB
chmod +x "$BIN/apt-get"
}

download_mirrors() { grep ' download$' "$LOG" 2>/dev/null | awk '{print $2}'; }
# mirror of the final real install (the last install call with no --download-only); empty if none.
final_install_mirror() { awk '$1=="install" && $3=="" {m=$2} END{printf "%s", m}' "$LOG"; }

run_scenario() { # run_scenario <stall 0|1> <stall_all 0|1> — returns the script's exit status
  : > "$LOG"
  printf 'deb http://ae.archive.ubuntu.com/ubuntu/ noble main\n' > "$CONF/sources.list"
  sudo_exec; write_stub
  local td="$WORK/tmp"; mkdir -p "$td" && echo keep > "$td/decoy.txt"   # beside the script's $work
  local rc=0
  STALL_ALL="${2}" APT_STALL="${1}" APT_LOG="$LOG" SUDO_LOG="$SUDO_LOG" TMPDIR="$td" \
    CI_APT_CONF_DIR="$CONF" CI_APT_ARCHIVES="$ARCHIVES" PATH="$BIN:$PATH" \
    bash "$script" "$LIST" "$DEBS" || rc=$?
  # finding 3: the EXIT trap removed only the script's $work, not the decoy beside it.
  [ -f "$td/decoy.txt" ] || fail "cleanup trap removed the decoy beside \$work (finding 3)"
  return $rc
}

# ── Scenario 1: the runner's default mirror stalls on download; the retry switches mirror (#1580)
#    and the final install reuses that mirror (finding 2).
run_scenario 1 0 || fail "script should recover by switching mirror (non-zero exit)"
grep -q 'argv BAD' "$LOG" && fail "apt -o argv was glued, not separate -o key=value (finding 1): $(grep 'argv BAD' "$LOG")"
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -ge 2 ] || fail "expected at least two download attempts, saw: $(download_mirrors | tr '\n' ' ')"
[ "${dms[0]}" = "DEFAULT" ] || fail "first download should use the runner's default mirror, got ${dms[0]}"
[ "${dms[1]}" != "${dms[0]}" ] || fail "did not switch mirror on stall: ${dms[0]} then ${dms[1]}"
[ "${dms[1]}" = "archive.ubuntu.com" ] || fail "expected archive.ubuntu.com on retry, got ${dms[1]}"
[ "$(final_install_mirror)" = "${dms[1]}" ] || fail "final install did not reuse the successful mirror ${dms[1]} (finding 2): saw '$(final_install_mirror)'"
pass "stall switches mirror (DEFAULT -> ${dms[1]}) and the final install reuses it (findings 1, 2)"

# ── Scenario 2: happy path — no stall means the mirror is never switched, and the final install
#    stays on the runner's default (no -o overrides).
run_scenario 0 0 || fail "happy path should exit zero"
grep -q 'argv BAD' "$LOG" && fail "apt -o argv was glued, not separate -o key=value (finding 1): $(grep 'argv BAD' "$LOG")"
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -eq 1 ] || fail "happy path should download once, saw ${#dms[@]} attempts: $(download_mirrors | tr '\n' ' ')"
[ "${dms[0]}" = "DEFAULT" ] || fail "happy path should use the default mirror, got ${dms[0]}"
[ "$(final_install_mirror)" = "DEFAULT" ] || fail "happy path final install should stay on the default mirror (no -o override), saw '$(final_install_mirror)'"
pass "no stall leaves the runner's mirror untouched and the final install on default"

# ── Scenario 3: both fallback mirrors also stall; the script gives up after three.
run_scenario 1 1 && fail "script should exit non-zero when every mirror stalls"
grep -q 'argv BAD' "$LOG" && fail "apt -o argv was glued, not separate -o key=value (finding 1): $(grep 'argv BAD' "$LOG")"
mapfile -t dms < <(download_mirrors)
[ "${#dms[@]}" -eq 3 ] || fail "expected three download attempts before giving up, saw ${#dms[@]}"
[ "${dms[0]}" = "DEFAULT" ] && [ "${dms[1]}" = "archive.ubuntu.com" ] && [ "${dms[2]}" = "azure.archive.ubuntu.com" ] \
  || fail "tried default then the two fallback mirrors: $(download_mirrors | tr '\n' ' ')"
pass "all mirrors stall: three distinct mirrors tried, then gives up"

# ── Scenario 4: with CI_APT_CONF_DIR unset, sudo tee targets the real /etc/apt path (finding 3).
: > "$LOG"; : > "$SUDO_LOG"
printf 'deb http://ae.archive.ubuntu.com/ubuntu/ noble main\n' > "$CONF/sources.list"
sudo_capture; write_stub
TD="$WORK/tmp4"; mkdir -p "$TD" && echo keep > "$TD/decoy.txt"
if APT_LOG="$LOG" SUDO_LOG="$SUDO_LOG" TMPDIR="$TD" \
     env -u CI_APT_CONF_DIR CI_APT_ARCHIVES="$ARCHIVES" PATH="$BIN:$PATH" \
     bash "$script" "$LIST" "$DEBS"; then :; fi
[ -f "$TD/decoy.txt" ] || fail "cleanup trap removed the decoy beside \$work (finding 3)"
grep -q '/etc/apt/apt.conf.d/80-ci-bounded' "$SUDO_LOG" \
  || fail "with CI_APT_CONF_DIR unset, sudo tee did not target /etc/apt (finding 3): $(grep SUDO "$SUDO_LOG" | head)"
pass "CI_APT_CONF_DIR unset: sudo tee targets the real /etc/apt path (finding 3)"

echo "test-ci-apt-install: ok"
