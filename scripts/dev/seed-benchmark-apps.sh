#!/usr/bin/env bash
# seed-benchmark-apps.sh — idempotently seed Phase 4 benchmark apps into the catalog.
#
# Creates three deterministic workloads the P4-07 troubleshooting harness launches
# by name for comparable, run-to-run telemetry numbers:
#
#   Quasar Bench: Static   — pattern=smpte, zero motion, steady-frame baseline.
#   Quasar Bench: Ball     — pattern=ball, predictable constant motion (the classic
#                            test card); exercises the typical streaming encode path.
#   Quasar Bench: Snow     — pattern=snow, full-frame-change every frame; worst-case
#                            for encoder bitrate and encode latency.
#   Quasar Bench: Colour Ripple — animated zone-plate + heat colour map (torture
#                            test): dense rippling high-frequency field that pushes
#                            encode bitrate and the client decode/display path hard.
#
# Image: defaults to quasar-agent-dev:latest (hermes/AMD); set QUASAR_APP_IMAGE=quasar-node-agent:latest
# on the NVENC box (Tower).
#
# Each pins framerate=60/1 in the caps per the CLAUDE.md gotcha (videotestsrc
# defaults to 30 fps; the cap here ensures the compositor capture matches intent).
# The apps render into the session compositor via waylandsink; the compositor
# captures frames via waylanddisplaysrc — resolution is session-controlled, not
# set in the app.
#
# Overlay compatibility: the P4-03 deep-trace overlay stamps the top-left 192×48
# band in the Y plane after encode — it overwrites whatever the app renders there.
# All three videotestsrc patterns are compatible (no critical visual content in
# that band, and the overlay is always on top).
#
# Usage:
#   bash scripts/dev/seed-benchmark-apps.sh
# Or from the Mac:
#   qstack p4-bench
#
# Idempotent: apps that already exist (by name) are skipped, not duplicated.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
set -a; . "$ROOT/deploy/.env"; set +a
# QUASAR_TLS=auto (the default) 308-redirects every plain-HTTP path except /health
# and the agent routes, and curl without -L follows no redirect — so the default
# must be the HTTPS listener. That cert is self-signed, hence -k on every curl
# below (harmless when QUASAR_TLS=off drops back to plain HTTP).
if [ "${QUASAR_TLS:-auto}" = "off" ]; then
  API="${API:-http://localhost:${CONTROL_PORT:-8080}}"
else
  API="${API:-https://localhost:${QUASAR_TLS_PORT:-8443}}"
fi
# App container image. Defaults to the AMD dev image (hermes); override for the
# NVENC box, e.g. QUASAR_APP_IMAGE=quasar-node-agent:latest on an NVIDIA host.
IMAGE="${QUASAR_APP_IMAGE:-quasar-agent-dev:latest}"

col_green='\033[0;32m'; col_yellow='\033[0;33m'; col_cyan='\033[0;36m'; col_reset='\033[0m'
say()    { printf "\n${col_cyan}== %s ==${col_reset}\n" "$*"; }
created(){ printf "${col_green}  CREATED${col_reset} %s (id=%s)\n" "$1" "$2"; }
skipped(){ printf "${col_yellow}  EXISTS ${col_reset} %s (id=%s) — skipped\n" "$1" "$2"; }
die()    { echo "FATAL: $*" >&2; exit 1; }

# ── Login ──────────────────────────────────────────────────────────────────────
# QUASAR_ADMIN_TOKEN (a bearer from scripts/dx/admin_token.sh) skips the
# BOOTSTRAP_ADMIN login — stacks running dev-agent auth carry no such creds.
say "login as admin"
ADMIN_TOK="${QUASAR_ADMIN_TOKEN:-}"
[ -n "$ADMIN_TOK" ] || ADMIN_TOK=$(curl -fsk -X POST "$API/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"${BOOTSTRAP_ADMIN_EMAIL:?}\",\"password\":\"${BOOTSTRAP_ADMIN_PASSWORD:?}\"}" \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])") \
  || die "admin login failed — is the stack up? (try: qstack up)"
echo "  logged in"

# ── Fetch existing apps ────────────────────────────────────────────────────────
say "checking existing catalog"
EXISTING=$(curl -fsk "$API/v1/admin/apps" -H "Authorization: Bearer $ADMIN_TOK" \
  | python3 -c "import sys,json; [print(a['name']+'|'+a['id']) for a in json.load(sys.stdin).get('items',[])]")

app_id_by_name() {
  echo "$EXISTING" | awk -F'|' -v name="$1" '$1==name{print $2}'
}

# ── Seed one app ───────────────────────────────────────────────────────────────
# seed_app NAME DESCRIPTION PATTERN
seed_app() {
  local name="$1" desc="$2" pattern="$3"
  local existing
  existing=$(app_id_by_name "$name")
  if [ -n "$existing" ]; then
    skipped "$name" "$existing"
    return
  fi

  # Build the request body via Python to avoid shell quoting nightmares with JSON.
  # runtime_spec: gst-launch-1.0 videotestsrc pattern=X ! caps ! videoconvert ! waylandsink
  # framerate=60/1 in caps per CLAUDE.md gotcha (videotestsrc default is 30 fps without it).
  local body
  body=$(python3 - <<PYEOF
import json
spec = {
    "image": "${IMAGE}",
    "args": [
        "gst-launch-1.0", "-e",
        "videotestsrc", "pattern=${pattern}",
        "!", "video/x-raw,framerate=60/1",
        "!", "videoconvert",
        "!", "waylandsink"
    ],
    "gpu": False
}
payload = {
    "name": "${name}",
    "description": "${desc}",
    "default_width": 1920,
    "default_height": 1080,
    "default_fps": 60,
    "default_bitrate_kbps": 8000,
    "runtime_spec": spec
}
print(json.dumps(payload))
PYEOF
)

  local resp
  resp=$(curl -fsk -X POST "$API/v1/apps" \
    -H "Authorization: Bearer $ADMIN_TOK" \
    -H 'Content-Type: application/json' \
    -d "$body") || die "HTTP error creating app: $name"

  local id
  id=$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('app',d).get('id',''))") \
    || die "failed to parse response for: $name"
  [ -n "$id" ] || die "no id returned for $name — response: $resp"

  # Enable the app (create defaults enabled=false; PATCH sets it).
  curl -fsk -X PATCH "$API/v1/apps/$id" \
    -H "Authorization: Bearer $ADMIN_TOK" \
    -H 'Content-Type: application/json' \
    -d '{"enabled":true}' >/dev/null \
    || die "failed to enable app: $name ($id)"

  created "$name" "$id"
}

# ── Seed an app with a fully custom gst-launch pipeline ──────────────────────────
# seed_app_pipeline NAME DESCRIPTION  (pipeline args read from the ARGS_JSON env var,
# a JSON array of gst-launch tokens). For workloads the simple pattern path can't
# express (filters, mixers, custom caps).
seed_app_pipeline() {
  local name="$1" desc="$2" existing
  existing=$(app_id_by_name "$name")
  if [ -n "$existing" ]; then skipped "$name" "$existing"; return; fi

  local body
  body=$(IMAGE="$IMAGE" NAME="$name" DESC="$desc" python3 - <<'PYEOF'
import json, os
print(json.dumps({
    "name": os.environ["NAME"],
    "description": os.environ["DESC"],
    "default_width": 1920, "default_height": 1080,
    "default_fps": 60, "default_bitrate_kbps": 8000,
    "runtime_spec": {
        "image": os.environ["IMAGE"],
        "args": json.loads(os.environ["ARGS_JSON"]),
        "gpu": False,
    },
}))
PYEOF
)
  local resp id
  resp=$(curl -fsk -X POST "$API/v1/apps" -H "Authorization: Bearer $ADMIN_TOK" \
    -H 'Content-Type: application/json' -d "$body") || die "HTTP error creating app: $name"
  id=$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('app',d).get('id',''))")
  [ -n "$id" ] || die "no id returned for $name — response: $resp"
  curl -fsk -X PATCH "$API/v1/apps/$id" -H "Authorization: Bearer $ADMIN_TOK" \
    -H 'Content-Type: application/json' -d '{"enabled":true}' >/dev/null \
    || die "failed to enable app: $name ($id)"
  created "$name" "$id"
}

# ── The three benchmark workloads ──────────────────────────────────────────────
say "seeding benchmark apps"

seed_app \
  "Quasar Bench: Static" \
  "Zero-motion scene (smpte pattern). Steady-frame baseline for encode/telemetry." \
  "smpte"

seed_app \
  "Quasar Bench: Ball" \
  "Constant predictable motion (bouncing ball). Standard streaming encode workload." \
  "ball"

seed_app \
  "Quasar Bench: Snow" \
  "Full-frame-change every frame (random snow). Worst-case encode latency/bitrate." \
  "snow"

# Torture test: a full-frame animated zone-plate (dense concentric ripples) run
# through a heat colour map. High-frequency spatial detail + constant full-frame
# motion + shifting colour — a deliberate codec stressor (drives encode bitrate to
# the ceiling and exercises the client decode/display path, unlike the trivial
# patterns). Renders 720p and videoscales to fill the session resolution: a plain
# capsfilter→waylandsink negotiation does NOT propagate the output size back
# through `coloreffects`, so videotestsrc would otherwise fall back to its tiny
# default size in a corner. (Surfaced PR/issue: NVENC CBR overshoots the ABR
# setpoint on this content — see the issue linked in the commit.)
ARGS_JSON='["gst-launch-1.0","-e","videotestsrc","is-live=true","pattern=zone-plate","kx2=80","ky2=80","kt=2","!","video/x-raw,width=1280,height=720,framerate=60/1","!","coloreffects","preset=heat","!","videoconvert","!","videoscale","!","waylandsink"]' \
seed_app_pipeline \
  "Quasar Bench: Colour Ripple" \
  "Torture test — animated colour-mapped zone-plate (rippling high-frequency field). Pushes encode bitrate + client decode/display far harder than the static patterns."

say "done"
echo "  Seed complete. Launch via the UI or API; use with P4-07 harness for telemetry."
