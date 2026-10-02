#!/usr/bin/env bash
# recipe-revision.sh — the recipe revision a platform image needs (ADR 0008), read from
# the constant in that service's own source tree. The ONE derivation of the
# org.quasar.recipe label: deploy/build-images.sh and .github/workflows/images.yml both
# call it, and deploy/image-contract.json asserts the label on every platform image.
#
#   deploy/lib/recipe-revision.sh <role> [repo-root]    # runtime | control | recovery
#
# Prints the integer. Exit 2 for a role that is not a platform image, or when the source
# holds no single `... RECIPE_REVISION ... = <n>` line.
set -euo pipefail

role="${1:?usage: recipe-revision.sh <runtime|control|recovery> [repo-root]}"
root="${2:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

case "$role" in
  runtime)  rel="node-agent/src/recipe.rs" ;;
  recovery) rel="node-agent/crates/quasar-recovery/src/recipe/revision.rs" ;;
  control)  rel="control-plane/internal/buildinfo/recipe.go" ;;
  *) echo "recipe-revision: role '$role' is not a platform image" >&2; exit 2 ;;
esac

[ -f "$root/$rel" ] || { echo "recipe-revision: $root/$rel is missing" >&2; exit 2; }
n="$(sed -n -E 's/^(pub )?const (RECIPE_REVISION: u32|RecipeRevision) = ([0-9]+);?$/\3/p' "$root/$rel")"
case "$n" in
  ''|*[!0-9]*)
    echo "recipe-revision: $rel must hold exactly one line like 'pub const RECIPE_REVISION: u32 = 1;' (found: ${n:-none})" >&2
    exit 2 ;;
esac
printf '%s\n' "$n"
