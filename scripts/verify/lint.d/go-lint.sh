#!/usr/bin/env bash
# golangci-lint on control-plane/ (control-plane/.golangci.yml). The image is pinned by
# digest: v2.12.2, built with Go 1.26 like control-plane/go.mod. GOTOOLCHAIN=local, so a
# go.mod newer than the image fails here instead of downloading a toolchain.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit
image=golangci/golangci-lint@sha256:5cceeef04e53efe1470638d4b4b4f5ceefd574955ab3941b2d9a68a8c9ad5240
command -v docker >/dev/null || { echo "docker is not installed" >&2; exit 1; }
lint() {
  docker run --rm -e GOTOOLCHAIN=local -v "$PWD/control-plane:/src:ro" -w /src \
    "$image" golangci-lint run "$@"
}

# The planted fixture first: each ban still fires, on its "want" lines and no others.
fixture=testdata/lint/clients.go
want="$(grep -n '// want$' "control-plane/$fixture" | cut -d: -f1)"
rc=0
out="$(lint ./testdata/lint 2>&1)" || rc=$?
got="$(sed -n "s|^$fixture:\([0-9]*\):.*(forbidigo)\$|\1|p" <<<"$out" | sort -nu)"
if [ "$rc" -ne 1 ] || [ -z "$want" ] || [ "$got" != "$want" ]; then
  printf '%s\nFAIL go-lint: %s: want forbidigo on lines %s; got %s (exit %s)\n' \
    "$out" "$fixture" "${want//$'\n'/ }" "${got//$'\n'/ }" "$rc" >&2
  exit 1
fi

lint ./...
echo "go-lint: control-plane clean, planted fixture caught"
