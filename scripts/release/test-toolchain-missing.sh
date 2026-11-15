#!/usr/bin/env bash
# Offline test for scripts/release/toolchain-missing.sh with a fake `docker`: a tag the
# registry says is absent builds, a tag that exists does not, and a registry that fails
# in any other way is an error and never a build.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fail() { echo "toolchain-missing test: $*" >&2; exit 1; }
work="$(mktemp -d /tmp/quasar-toolchain-missing.XXXXXX)"
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat >"$work/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
# FAKE_BASE: ok|fail   FAKE_MANIFEST: present|unknown|name-unknown|denied
case "$1 $2" in
  "buildx imagetools")
    [ "${FAKE_BASE:-ok}" = ok ] || { echo "dial tcp: i/o timeout" >&2; exit 1; }
    echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
  "manifest inspect")
    case "${FAKE_MANIFEST:-present}" in
      present) echo '{}' ;;
      unknown) echo "manifest unknown" >&2; exit 1 ;;
      name-unknown) echo "name unknown: repository name not known to registry" >&2; exit 1 ;;
      nodocker) echo "bash: docker: command not found" >&2; exit 127 ;;
      shnodocker) echo "sh: 1: docker: not found" >&2; exit 127 ;;
      denied) echo "denied: requested access to the resource is denied" >&2; exit 1 ;;
      timeout) echo "Get https://ghcr.io/v2/: net/http: TLS handshake timeout" >&2; exit 1 ;;
    esac ;;
  *) exit 99 ;;
esac
DOCKER
chmod +x "$work/bin/docker"
run() { (cd "$root" && env PATH="$work/bin:$PATH" REGISTRY_NS=ghcr.io/example/quasar "$@" bash scripts/release/toolchain-missing.sh 2>&1); }

out="$(run FAKE_MANIFEST=present)" || fail "present: $out"
grep -qx 'build=false' <<<"$out" || fail "a published tag must not build: $out"
grep -Eq '^image=ghcr.io/example/quasar/quasar-gst-toolchain:[0-9a-f]{16}$' <<<"$out" || fail "image line: $out"
grep -Eq '^base_ref=.*quasar-base@sha256:0123' <<<"$out" || fail "base_ref line: $out"

for m in unknown name-unknown; do
  out="$(run FAKE_MANIFEST=$m)" || fail "$m: $out"
  grep -qx 'build=true' <<<"$out" || fail "$m must build: $out"
done
for m in denied timeout nodocker shnodocker; do
  rc=0; out="$(run FAKE_MANIFEST=$m)" || rc=$?
  [ "$rc" -ne 0 ] || fail "$m must be an error, got: $out"
  grep -q '^build=' <<<"$out" && fail "$m printed a build decision: $out"
done
rc=0; out="$(run FAKE_BASE=fail)" || rc=$?
[ "$rc" -ne 0 ] || fail "an unreadable base must be an error: $out"
echo "PASS toolchain-missing: absent builds, present reuses, a registry failure is an error"
