#!/usr/bin/env bash
# make verify. It only WARNs when one of these is missing, which would pass here unchecked.
for tool in shellcheck node python3; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
done
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit
exec bash scripts/dx/tests/run.sh
