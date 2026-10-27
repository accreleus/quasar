#!/usr/bin/env bash
# publish-preview-manifest.sh against a throwaway local registry:2: the artifact's
# shape, the read-back, --current, and that the tag only moves forward. Needs
# docker (it starts and removes one registry container on a loopback port); run it
# by hand like the other scripts/release tests.
set -euo pipefail
release="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
publish="$release/publish-preview-manifest.sh"
fixture="$release/../../testdata/release/platform-release-manifest.preview.json"
fail() { echo "publish preview manifest test: $*" >&2; exit 1; }
docker info >/dev/null 2>&1 || fail "needs docker (it runs a local registry:2)"

work="$(mktemp -d /tmp/quasar-publish-preview.XXXXXX)"
container="quasar-publish-preview-test-$$"
trap 'docker rm -f "$container" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
docker run -d --name "$container" -p 127.0.0.1::5000 registry:2 >/dev/null
port="$(docker port "$container" 5000/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/v2/" >/dev/null && break; sleep 0.2; done
ref="127.0.0.1:$port/accreleus/quasar/quasar-release-manifest:preview"
api="http://127.0.0.1:$port/v2/accreleus/quasar/quasar-release-manifest"

# A history to publish from: A -> B, and C on a side branch off A.
cd "$work"
g() { git -c user.name=t -c user.email=t@example.invalid -c init.defaultBranch=develop -c commit.gpgsign=false "$@"; }
g init -q .
g commit -q --allow-empty -m A; a="$(git rev-parse HEAD)"
g commit -q --allow-empty -m B; b="$(git rev-parse HEAD)"
g checkout -q -b side "$a"
g commit -q --allow-empty -m C; c="$(git rev-parse HEAD)"
export a b c # read through ${!x} below
for x in a b c; do
  python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); d["source_commit"]=sys.argv[2]; open(sys.argv[3],"w").write(json.dumps(d, indent=2)+"\n")' \
    "$fixture" "${!x}" "$work/$x.json"
done
pub() { bash "$publish" "$1" "$ref" --source-url https://github.com/accreleus/quasar --plain-http; }
cur() { bash "$publish" --current "$ref" --plain-http; }

[[ -z "$(cur)" ]] || fail "--current on an unpublished tag must print nothing"

digest="$(pub "$work/a.json")" || fail "first publish failed"
[[ "$(cur)" == "$a" ]] || fail "--current after publishing A: $(cur)"
curl -fsS -H 'Accept: application/vnd.oci.image.manifest.v1+json' -o "$work/m.json" "$api/manifests/preview"
[[ "sha256:$(sha256sum "$work/m.json" | cut -d' ' -f1)" == "$digest" ]] || fail "printed digest is not the tag's"
python3 - "$work/m.json" <<'PY' || fail "artifact shape"
import json, sys
m = json.load(open(sys.argv[1]))
assert m["mediaType"] == "application/vnd.oci.image.manifest.v1+json", m
assert m["config"]["mediaType"] == "application/vnd.oci.empty.v1+json", m
assert len(m["layers"]) == 1 and "manifests" not in m, m
PY
layer="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["layers"][0]["digest"])' "$work/m.json")"
curl -fsS "$api/blobs/$layer" | cmp -s - "$work/a.json" || fail "the layer is not the document"

pub "$work/b.json" >/dev/null || fail "a newer commit must move the tag"
pub "$work/b.json" >/dev/null || fail "the same commit again (a re-run of the newest run) must be accepted"
for older in a c; do
  out="$(pub "$work/$older.json" 2>&1)" && fail "publishing ${older^^} over B was accepted"
  [[ "$out" == *"refusing to move the preview back"* ]] || fail "missing refusal for ${older^^}: $out"
done
[[ "$(cur)" == "$b" ]] || fail "a refused publish moved the tag: $(cur)"

rc=0; bash "$publish" "$work/a.json" "127.0.0.1:$port/no-tag" --source-url x --plain-http >/dev/null 2>&1 || rc=$?
[[ $rc -eq 1 ]] || fail "a ref with no tag must be refused, got $rc"

echo "Publish preview manifest: PASS"
