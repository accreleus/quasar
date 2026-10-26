#!/usr/bin/env bash
# CI: install the packages named in a list file, bounded and cached (#864).
#   ci-apt-install.sh <package-list> <deb-dir>
# <deb-dir> is what actions/cache restores before and saves after this step. The ~390-package
# GStreamer set came from a mirror that stalled for minutes between packages (p90 22 min, max
# 90 min), so every request gets an idle timeout and retries, and the downloaded .debs are put
# back in apt's archive before the install so a cache hit downloads nothing.
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
sudo apt-get update
sudo apt-get install -y --no-install-recommends "${packages[@]}"

find "$archives" -maxdepth 1 -name '*.deb' -exec cp -n -t "$debs" {} +
echo "debs to cache: $(find "$debs" -maxdepth 1 -name '*.deb' | wc -l)"
