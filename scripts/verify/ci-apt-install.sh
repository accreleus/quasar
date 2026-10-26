#!/usr/bin/env bash
# CI: install the packages named in a list file, bounded and cached (#864).
#   ci-apt-install.sh <package-list> <deb-dir>
# <deb-dir> is what actions/cache restores before and saves after this step. The ~390-package
# GStreamer set came from a mirror that stalled for minutes between packages (p90 22 min, max
# 90 min), so every request gets an idle timeout and retries, and the downloaded .debs are put
# back in apt's archive before the install so a cache hit downloads nothing. The idle timeout
# alone did not hold (PR #871: the mirror trickled, so no connection was ever idle): the download
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

find "$debs" -maxdepth 1 -name '*.deb' -exec sudo cp -n -t "$archives" {} +
echo "debs restored from cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"

mapfile -t packages < <(grep -v -e '^#' -e '^$' "$list")
sudo timeout 120 apt-get update
for attempt in 1 2 3; do
  sudo timeout 180 apt-get install -y --no-install-recommends --download-only "${packages[@]}" && break
  [ "$attempt" -lt 3 ] || { echo "ci-apt-install: download still stalled after $attempt attempts" >&2; exit 1; }
  echo "ci-apt-install: attempt $attempt did not finish in 3 minutes, resuming"
done
sudo apt-get install -y --no-install-recommends "${packages[@]}"

find "$archives" -maxdepth 1 -name '*.deb' -exec cp -n -t "$debs" {} +
echo "debs to cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"
