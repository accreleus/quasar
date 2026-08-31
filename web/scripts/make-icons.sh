#!/usr/bin/env bash
# Regenerate the installable-web-app icons (#435) from the Quasar brand mark.
#
# Source of truth is web/public/icon.svg — the mark the design handoff defines
# (design_handoff_quasar/README.md: "the Quasar mark is an inline SVG, an
# accretion-ellipse around a gradient core", gradient #6A45F5→#5B6BFF→#00C0FF).
# It is the same art already shipped as the favicon; nothing here invents a logo.
#
# Two things the raw mark cannot do on a home screen, which is all this script
# adds:
#   - It is transparent. A transparent home-screen icon renders on black on iOS
#     and on an arbitrary launcher background on Android, so every output gets
#     an opaque --ink-1 (#0c0c12) plate — the same colour as the manifest's
#     background_color, so the icon and the splash are one surface.
#   - It is full-bleed to its own viewBox. Android's maskable spec crops to a
#     centre circle of 80% diameter, so the maskable variant is scaled to 0.85
#     (mark half-extent 15/16 * 0.85 = 12.75 <= the 12.8 safe radius). The
#     "any" and Apple variants are inset less, since neither is centre-cropped.
#
# Usage: bash web/scripts/make-icons.sh   (needs rsvg-convert; brew install librsvg)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$here/public/icon.svg"
out="$here/public/icons"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

command -v rsvg-convert >/dev/null || { echo "rsvg-convert not found (brew install librsvg)" >&2; exit 1; }
[ -f "$src" ] || { echo "missing $src" >&2; exit 1; }

BG="#0c0c12" # --ink-1, tokens.css

# Body of the mark, minus the outer <svg> wrapper.
mark="$(sed -e '1d' -e '$d' "$src")"

plate() { # $1 = scale of the mark within the 32-unit box
  cat <<EOF
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="1024" height="1024">
  <rect width="32" height="32" fill="$BG"/>
  <g transform="translate(16 16) scale($1) translate(-16 -16)">
$mark
  </g>
</svg>
EOF
}

mkdir -p "$out"
plate 0.92 > "$tmp/any.svg"
plate 0.85 > "$tmp/maskable.svg"
plate 0.80 > "$tmp/apple.svg"

render() { rsvg-convert -w "$2" -h "$2" "$tmp/$1.svg" -o "$out/$3"; }
render any      192 icon-192.png
render any      512 icon-512.png
render maskable 512 icon-maskable-512.png
render apple    180 apple-touch-icon-180.png

ls -l "$out"
