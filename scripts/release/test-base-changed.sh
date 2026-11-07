#!/usr/bin/env bash
# Offline test for scripts/release/base-changed.sh with a fake `docker`.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "base-changed test: $*" >&2; exit 1; }
work="$(mktemp -d /tmp/quasar-base-changed.XXXXXX)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat >"$work/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
# FAKE_LABEL: the label value ("" = empty, "<no value>" = absent)   FAKE_ERR: registry error text
[ -z "${FAKE_ERR:-}" ] || { echo "$FAKE_ERR" >&2; exit 1; }
echo "${FAKE_LABEL-}"
DOCKER
chmod +x "$work/bin/docker"
OLD=sha256:1111111111111111111111111111111111111111111111111111111111111111
NEW=sha256:2222222222222222222222222222222222222222222222222222222222222222
B=ghcr.io/example/quasar-base
run() { env PATH="$work/bin:$PATH" "$@" bash "$root/scripts/release/base-changed.sh" ghcr.io/example/quasar/quasar-node-agent@sha256:aaaa "$B@$NEW" 2>&1; }
expect() { # expect <true|false|error> <why substring> <env...>
  local want=$1 sub=$2 out rc=0
  shift 2
  out="$(run "$@")" || rc=$?
  if [ "$want" = error ]; then
    [ "$rc" -ne 0 ] || fail "want an error, got: $out"
    grep -q '^rebuild=' <<<"$out" && fail "an error printed a decision: $out"
  else
    [ "$rc" -eq 0 ] || fail "rc=$rc: $out"
    grep -qx "rebuild=$want" <<<"$out" || fail "want rebuild=$want: $out"
  fi
  grep -qF -- "$sub" <<<"$out" || fail "want '$sub' in: $out"
}
expect false "unchanged" "FAKE_LABEL=$B@$NEW"
expect true "the base moved" "FAKE_LABEL=$B@$OLD"
expect true "no org.quasar.pins.base label" "FAKE_LABEL="
expect true "no org.quasar.pins.base label" "FAKE_LABEL=<no value>"
expect true "not a digest" "FAKE_LABEL=$B:latest"
expect true "not in the registry" "FAKE_ERR=ghcr.io/x: not found"
expect true "not in the registry" "FAKE_ERR=manifest unknown"
expect error "TLS handshake timeout" "FAKE_ERR=net/http: TLS handshake timeout"
expect error "denied" "FAKE_ERR=denied: requested access to the resource is denied"
out="$(env PATH="$work/bin:$PATH" FAKE_LABEL="$B@$NEW" bash "$root/scripts/release/base-changed.sh" img "$B:latest" 2>&1)"
grep -qx 'rebuild=true' <<<"$out" || fail "a base that is not a digest must rebuild: $out"
echo "PASS base-changed: same digest skips, a moved base, a missing label or image rebuilds, a registry error fails"
