#!/usr/bin/env bash
# Publish the preview manifest as a one-layer OCI artifact at a registry tag
# (protocol/control-api.md "Platform-release preview channel"): an OCI image
# manifest whose config is the OCI empty descriptor and whose ONE layer is the
# document's bytes, unchanged. The control plane reads it with FetchArtifact,
# which refuses any other media type or layer count.
#
# Plain registry API over curl, so no tool beyond the runner's own sits beside
# the push token. The tag is written last (blobs first), then read back: the
# manifest the tag resolves to must hash to the one written, and its layer must
# be the document byte for byte, or the script exits 1.
#
# Credentials come from REGISTRY_USER / REGISTRY_TOKEN (the workflow's
# GITHUB_TOKEN); they go to curl through a mode-600 config file, never argv.
# With neither set the registry is used anonymously (a local test registry).
#
# Exit codes: 0 published and read back · 1 refused or failed · 2 usage.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/release/publish-preview-manifest.sh DOCUMENT REGISTRY/REPOSITORY:TAG \
         --source-url URL [--plain-http]

Pushes DOCUMENT (already validated with validate-platform-release-manifest.sh
--preview) as the single layer of an OCI image manifest and points TAG at it.
--source-url is recorded as the org.opencontainers.image.source annotation, which
links the package to its repository. --plain-http is for a local test registry.
Prints the manifest digest on success.
EOF
}

doc="" ref="" source_url="" scheme=https
while (($#)); do
  case "$1" in
    --source-url) source_url=${2:?--source-url needs a value}; shift 2 ;;
    --plain-http) scheme=http; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage >&2; exit 2 ;;
    *) if [[ -z "$doc" ]]; then doc=$1; elif [[ -z "$ref" ]]; then ref=$1; else usage >&2; exit 2; fi
       shift ;;
  esac
done
[[ -n "$doc" && -n "$ref" && -n "$source_url" ]] || { usage >&2; exit 2; }
[[ -f "$doc" ]] || { echo "publish-preview-manifest: no such document: $doc" >&2; exit 1; }

host="${ref%%/*}"
rest="${ref#*/}"
name="${rest%:*}"
tag="${rest##*:}"
# A tag, never a digest; the OCI tag grammar.
if [[ "$host" == "$ref" || "$name" == "$rest" || "$rest" == *@* ]] \
   || ! [[ "$tag" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
  echo "publish-preview-manifest: $ref is not REGISTRY/REPOSITORY:TAG" >&2
  exit 1
fi
base="$scheme://$host/v2/$name"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
umask 077
: > "$work/curl.conf"
fail() { echo "publish-preview-manifest: $*" >&2; exit 1; }
sha() { printf 'sha256:%s' "$(sha256sum "$1" | cut -d' ' -f1)"; }
req() { curl -sS --fail-with-body -K "$work/curl.conf" "$@"; }

# Bearer token from the registry's challenge (GHCR: /token, scope pull,push).
challenge="$(curl -sS -o /dev/null -D - "$scheme://$host/v2/" | tr -d '\r' \
  | sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: *[Bb]earer *//p')"
if [[ -n "$challenge" ]]; then
  realm="$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"$challenge")"
  service="$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"$challenge")"
  [[ -n "$realm" ]] || fail "unreadable auth challenge from $host"
  auth_conf="$work/auth.conf"
  : > "$auth_conf"
  [[ -z "${REGISTRY_TOKEN:-}" ]] || printf 'user = "%s:%s"\n' "${REGISTRY_USER:-token}" "$REGISTRY_TOKEN" > "$auth_conf"
  token="$(curl -sS --fail-with-body -K "$auth_conf" -G "$realm" \
    --data-urlencode "service=$service" --data-urlencode "scope=repository:$name:pull,push" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("token") or d.get("access_token") or "")')"
  [[ -n "$token" ]] || fail "the registry gave no token for $name"
  printf 'header = "Authorization: Bearer %s"\n' "$token" > "$work/curl.conf"
fi

printf '{}' > "$work/config"
cp "$doc" "$work/layer"

push_blob() { # push_blob <file>: monolithic upload, POST then PUT ?digest=
  local file=$1 digest location
  digest="$(sha "$file")"
  location="$(req -X POST -o /dev/null -D - "$base/blobs/uploads/" | tr -d '\r' \
    | sed -n 's/^[Ll]ocation: *//p')"
  [[ -n "$location" ]] || fail "no upload location for $name"
  [[ "$location" == http* ]] || location="$scheme://$host$location"
  [[ "$location" == *\?* ]] && location="$location&" || location="$location?"
  req -X PUT -o /dev/null -H 'Content-Type: application/octet-stream' \
    --data-binary "@$file" "${location}digest=$digest"
}
push_blob "$work/config"
push_blob "$work/layer"

python3 - "$work/manifest" "$(sha "$work/config")" "$(sha "$work/layer")" \
  "$(wc -c < "$work/layer")" "$source_url" <<'PY'
import json, sys
out, config, layer, size, source = sys.argv[1:]
json.dump({
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "artifactType": "application/vnd.quasar.release-manifest.v2+json",
    "config": {"mediaType": "application/vnd.oci.empty.v1+json", "digest": config, "size": 2},
    "layers": [{
        "mediaType": "application/json",
        "digest": layer,
        "size": int(size),
        "annotations": {"org.opencontainers.image.title": "platform-release-manifest.preview.json"},
    }],
    "annotations": {"org.opencontainers.image.source": source},
}, open(out, "w"), separators=(",", ":"))
PY
want="$(sha "$work/manifest")"
req -X PUT -o /dev/null -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
  --data-binary "@$work/manifest" "$base/manifests/$tag"

# Read back through the tag, as a reader would.
req -o "$work/got-manifest" -H 'Accept: application/vnd.oci.image.manifest.v1+json' "$base/manifests/$tag"
[[ "$(sha "$work/got-manifest")" == "$want" ]] || fail "$ref resolves to $(sha "$work/got-manifest"), not the manifest written ($want)"
req -L -o "$work/got-layer" "$base/blobs/$(sha "$work/layer")"
cmp -s "$work/got-layer" "$doc" || fail "the layer at $ref is not the document"
echo "$want"
