#!/usr/bin/env bash
# Gather check-release-compatibility.sh's inputs in the release job:
#   OUT/known/<tag>.json   every published release's platform-release-manifest.v2.json
#                          (drafts, releases without the asset, and the candidate's own
#                          tag are skipped, so re-running a half-published release works)
#   OUT/recipes.json       org.quasar.recipe of every image@digest the candidate and the
#                          known manifests name, read from the registry
#   OUT/actor-windows.json the candidate recovery actor's `quasar-recovery recipes`
#   OUT/previous-actor-windows.json
#                          the same from the previous format-2 release's actor (the one
#                          ordering highest below the candidate), when there is one
#
# Needs gh (GH_TOKEN), docker with buildx, and registry read access, so it has no
# offline test beyond `bash -n` and shellcheck; the check it feeds is tested by
# scripts/release/test-release-compatibility.sh.
#
# Exit codes: 0 wrote every input · 1 an input could not be gathered · 2 usage.
set -euo pipefail

manifest=""
repo=""
out=""
asset="platform-release-manifest.v2.json"

usage() {
  cat <<'EOF'
usage: scripts/release/collect-release-compatibility-inputs.sh \
         --manifest platform-release-manifest.v2.json --repo OWNER/NAME --out DIR
EOF
}

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?--manifest needs a path}; shift 2 ;;
    --repo) repo=${2:?--repo needs OWNER/NAME}; shift 2 ;;
    --out) out=${2:?--out needs a directory}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -n "$manifest" && -n "$repo" && -n "$out" ]] || { usage >&2; exit 2; }

fail() { echo "collect-release-compatibility-inputs: $*" >&2; exit 1; }

candidate_tag="v$(jq -er .version "$manifest")" || fail "cannot read the version of $manifest"
mkdir -p "$out/known"

gh release list --repo "$repo" --limit 1000 --json tagName,isDraft \
    --jq '.[] | select(.isDraft | not) | .tagName' > "$out/tags.txt" \
  || fail "cannot list the releases of $repo"
while IFS= read -r tag; do
  [[ -n "$tag" && "$tag" != "$candidate_tag" ]] || continue
  names="$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[].name')" \
    || fail "cannot read the assets of release $tag"
  if ! grep -Fxq "$asset" <<< "$names"; then
    echo "  $tag: no $asset"
    continue
  fi
  gh release download "$tag" --repo "$repo" --pattern "$asset" --output "$out/known/$tag.json" \
    --clobber || fail "cannot download $asset from release $tag"
  echo "  $tag: $asset"
done < "$out/tags.txt"

# Every image@digest named by a format-2 manifest, candidate first.
refs=()
for doc in "$manifest" "$out"/known/*.json; do
  [[ -f "$doc" ]] || continue
  [[ "$(jq -r .format_version "$doc")" == 2 ]] || continue
  mapfile -t -O "${#refs[@]}" refs < <(jq -r '.components[] | "\(.image)@\(.digest)"' "$doc")
done

# A pushed-by-digest image is a single manifest, so .Image is its config; an index
# gives one config per platform, and linux/amd64 is the one the release builds.
mapfile -t unique < <(printf '%s\n' "${refs[@]}" | sort -u)
: > "$out/recipes.tsv"
for ref in "${unique[@]}"; do
  image="$(docker buildx imagetools inspect "$ref" --format '{{json .Image}}')" \
    || fail "cannot inspect $ref"
  rev="$(jq -r 'if has("config") then .config.Labels["org.quasar.recipe"]
                else .["linux/amd64"].config.Labels["org.quasar.recipe"] end // ""' <<< "$image")"
  [[ "$rev" =~ ^[1-9][0-9]*$ ]] \
    || fail "$ref has no integer org.quasar.recipe label (got '${rev}')"
  printf '%s\t%s\n' "$ref" "$rev" >> "$out/recipes.tsv"
done
jq -Rn '[inputs | split("\t") | {(.[0]): (.[1] | tonumber)}] | add // {}' \
  < "$out/recipes.tsv" > "$out/recipes.json" || fail "cannot write recipes.json"

actor="$(jq -er '.components[] | select(.name == "recovery-actor") | "\(.image)@\(.digest)"' \
  "$manifest")" || fail "$manifest names no recovery-actor component"
docker run --rm --pull always --network none "$actor" recipes > "$out/actor-windows.json" \
  || fail "$actor recipes failed"

# The hand-over (check (d)): the previous release's actor renders the candidate's.
previous="$(python3 - "$manifest" "$out/known" <<'PY'
import json, re, sys
from pathlib import Path
SEMVER = re.compile(r"(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?")
def key(v):
    m = SEMVER.fullmatch(v)
    pre = m.group(4)
    ids = () if pre is None else tuple((0, int(i), "") if i.isdigit() else (1, 0, i) for i in pre.split("."))
    return (tuple(int(x) for x in m.groups()[:3]), (1,) if pre is None else (0, ids))
candidate = json.loads(Path(sys.argv[1]).read_text())["version"]
best = None
for path in Path(sys.argv[2]).glob("*.json"):
    doc = json.loads(path.read_text())
    if doc.get("format_version") != 2 or key(doc["version"]) >= key(candidate):
        continue
    if best is None or key(doc["version"]) > key(best["version"]):
        best = doc
if best:
    print(next(f'{c["image"]}@{c["digest"]}' for c in best["components"] if c["name"] == "recovery-actor"))
PY
)" || fail "cannot choose the previous release"
if [[ -n "$previous" ]]; then
  docker run --rm --pull always --network none "$previous" recipes > "$out/previous-actor-windows.json" \
    || fail "$previous recipes failed"
fi

echo "collected: $(find "$out/known" -name '*.json' | wc -l) known manifest(s)," \
  "$(jq length "$out/recipes.json") recipe label(s), actor windows from $actor"
