#!/usr/bin/env bash
# CI: install the packages named in a list file, bounded and cached (#864).
#   ci-apt-install.sh <package-list> <deb-dir>
# <deb-dir> is what actions/cache restores before and saves after this step. The ~390-package
# GStreamer set came from a mirror that stalled for minutes between packages (p90 22 min, max
# 90 min), so every request gets an idle timeout and retries, and the downloaded .debs are put
# back in apt's archive before the install so a cache hit downloads nothing. The idle timeout
# alone did not hold (#864: the mirror trickled, so no connection was ever idle): the download
# is also killed and restarted after 3 minutes, which keeps every finished and partial .deb.
set -euo pipefail

list=$1
debs=$2
# Overridable so the self-test can point at a fake apt config and archives dir and run with a
# stubbed apt and no network. In CI these are unset and the real paths are used unchanged.
aptconf=${CI_APT_CONF_DIR:-/etc/apt}
archives=${CI_APT_ARCHIVES:-/var/cache/apt/archives}

work="$(mktemp -d "${TMPDIR:-/tmp}/ci-apt-install.XXXXXX")"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

mkdir -p "$debs" "$archives"
sudo tee "$aptconf/apt.conf.d/80-ci-bounded" >/dev/null <<'CONF'
Acquire::http::Timeout "20";
Acquire::https::Timeout "20";
Acquire::Retries "3";
CONF

find "$debs" -maxdepth 1 -name '*.deb' -exec sudo cp --update=none -t "$archives" {} +
echo "debs restored from cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"

[ -s "$list" ] || { echo "ci-apt-install: missing or empty package list: $list" >&2; exit 2; }
mapfile -t packages < <(grep -v -e '^#' -e '^$' "$list")
[ "${#packages[@]}" -gt 0 ] || { echo "ci-apt-install: $list names no packages" >&2; exit 2; }
# The mirrors can stop answering altogether (2026-11-11: the runner's first mirror timed out and
# the archive did not resolve, on every run). With the .deb set restored from the cache nothing
# needs them: install the cached files as they are. apt takes the named files as its
# candidates and resolves their dependencies among them and what is installed.
# shortcut: this installs every cached .deb, a superset of the list, so a package the list no
# longer names (or never did) still installs here and only shows on the next online run;
# install from a local flat repository of the cache if that ever hides a real omission.
offline_install() {
  local n
  n=$(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)
  [ "$n" -gt 0 ] || return 1
  echo "ci-apt-install: apt-get update did not finish; installing the $n cached .debs offline"
  (cd "$debs" && sudo timeout 300 apt-get install -y --no-install-recommends --no-download ./*.deb) || return 1
  # The restored set may be another job's or an older list's: every package asked for must be in.
  local p
  for p in "${packages[@]}"; do
    dpkg-query -W -f='${Status}' "${p%%=*}" 2>/dev/null | grep -qx 'install ok installed' ||
      { echo "ci-apt-install: the cached set lacks $p" >&2; return 1; }
  done
}
# apt chooses the mirror from sources.list, so switching one means regenerating the sources with
# the next host and refreshing its indices before downloading (the .deb URLs in the old indices
# still name the stalled host). swap_sources rewrites only the mirror host on each deb/deb-src and
# deb822 URIs line, preserving suite, components and format; it reads the active sources under
# $aptconf so the self-test can point it at a fake one.
swap_sources() { # swap_sources <target-mirror> -> regenerated sources under $work/parts
  local target="$1"
  local host="${target#*://}" f
  local -a files=()
  [ -f "$aptconf/sources.list" ] && files+=("$aptconf/sources.list")
  for f in "$aptconf"/sources.list.d/*.sources "$aptconf"/sources.list.d/*.list; do
    [ -f "$f" ] && files+=("$f")
  done
  [ "${#files[@]}" -gt 0 ] || return 1
  # One output file per input, keeping its name and so its format: a deb822 .sources file must
  # stay a .sources file (apt reads the one-line format only from .list files and sourcelist).
  rm -rf "$work/parts"; mkdir -p "$work/parts"
  for f in "${files[@]}"; do
    local out
    out="$work/parts/$(basename "$f")"
    [ "$f" = "$aptconf/sources.list" ] && out="$work/parts/00-sources.list"
    sed -E \
      -e "s#(://)[a-z0-9.-]*archive\.ubuntu\.com#\1$host#g" \
      -e "s#(://)security\.ubuntu\.com#\1$host#g" \
      "$f" > "$out"
  done
}
# apt_get <mirror> <timeout> <apt-args...>: run apt with the system sources, or with a regenerated
# sources at <mirror> (an empty mirror means the runner's default). Global -o options go before the
# subcommand, so apt parses them. Each -o takes its directive as the NEXT argument (-o key=value),
# never glued to it ("-o key=value" as one arg is an unknown option apt ignores, so the override
# would silently not apply): emit them as separate argv entries. The regenerated sources live in
# $work/parts (Dir::Etc::sourceparts), one file per original so deb822 .sources keep their format
# (#1580: concatenating them into the one-line sourcelist made apt reject 'Types:'); sourcelist
# points at an empty file so the runner's own sources are not read as well.
apt_get() { # apt_get <mirror> <timeout> <apt-args...>
  local mirror="$1" timeout="$2"; shift 2
  local -a opts=()
  if [ -n "$mirror" ]; then
    swap_sources "$mirror" || return 1
    : > "$work/empty.list"
    opts=(-o "Dir::Etc::sourcelist=$work/empty.list" -o "Dir::Etc::sourceparts=$work/parts")
  fi
  sudo timeout "$timeout" apt-get "${opts[@]+"${opts[@]}"}" "$@"
}
if ! apt_get "" 120 update; then
  offline_install && exit 0
  echo "ci-apt-install: apt-get update did not finish and no cached .deb set installs" >&2
  exit 1
fi
# On a stall advance to the next mirror instead of retrying the same one (#1580): the runner's
# regional mirror (e.g. us-east-2.ec2.archive.ubuntu.com) can hang on the large ROCm/HIP packages
# while another archive host answers. The first attempt keeps the runner's default mirror; the
# next two switch to archive.ubuntu.com then azure.archive.ubuntu.com.
mirrors=( "" "https://archive.ubuntu.com" "https://azure.archive.ubuntu.com" )
for attempt in 1 2 3; do
  mirror="${mirrors[$((attempt - 1))]}"
  download_ok=1
  if [ -n "$mirror" ]; then
    # Switched mirror: its .deb URLs live in its own indices, so refresh those before downloading
    # (the download URL is taken from the index, not the sources.list host).
    if ! apt_get "$mirror" 120 update; then
      download_ok=0
    fi
  fi
  if [ "$download_ok" -eq 1 ] && ! apt_get "$mirror" 180 install -y --no-install-recommends --download-only "${packages[@]}"; then
    download_ok=0
  fi
  [ "$download_ok" -eq 1 ] && break
  [ "$attempt" -lt 3 ] || { echo "ci-apt-install: download still stalled after $attempt attempts" >&2; exit 1; }
  echo "ci-apt-install: attempt $attempt stalled, switching mirror; resuming"
done
# The successful download left this mirror's indices in apt's lists directory (update drops the
# lists not named by the active sources). Install against the SAME mirror, or the default sources
# name the runner's host while its lists are gone and apt reports "Unable to locate package".
apt_get "$mirror" 300 install -y --no-install-recommends "${packages[@]}"

find "$archives" -maxdepth 1 -name '*.deb' -exec cp --update=none -t "$debs" {} +
echo "debs to cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"
