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
archives=/var/cache/apt/archives

mkdir -p "$debs"
sudo tee /etc/apt/apt.conf.d/80-ci-bounded >/dev/null <<'CONF'
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
if ! sudo timeout 120 apt-get update; then
  offline_install && exit 0
  echo "ci-apt-install: apt-get update did not finish and no cached .deb set installs" >&2
  exit 1
fi
for attempt in 1 2 3; do
  sudo timeout 180 apt-get install -y --no-install-recommends --download-only "${packages[@]}" && break
  [ "$attempt" -lt 3 ] || { echo "ci-apt-install: download still stalled after $attempt attempts" >&2; exit 1; }
  echo "ci-apt-install: attempt $attempt did not finish in 3 minutes, resuming"
done
sudo timeout 300 apt-get install -y --no-install-recommends "${packages[@]}"

find "$archives" -maxdepth 1 -name '*.deb' -exec cp --update=none -t "$debs" {} +
echo "debs to cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"
