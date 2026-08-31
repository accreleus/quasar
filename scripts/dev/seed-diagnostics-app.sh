#!/usr/bin/env bash
# seed-diagnostics-app.sh — seed/refresh the Quasar stream diagnostics app.
#
# The app image runs Chromium in Wayland kiosk mode and opens the bundled
# quasar-stream-diagnostics.html. It is intentionally separate from quasar-agent-dev so
# stream diagnostics exercise a lightweight app container instead of the build
# toolbox image.
#
# Usage:
#   bash scripts/dev/seed-diagnostics-app.sh [postgres_container]
#
# Env:
#   QUASAR_DIAGNOSTICS_APP_IMAGE=quasar-diagnostics-app:latest

set -euo pipefail

PG="${1:-deploy-quasar-postgres-1}"
IMAGE="${QUASAR_DIAGNOSTICS_APP_IMAGE:-quasar-diagnostics-app:latest}"
NAME="${QUASAR_DIAGNOSTICS_APP_NAME:-Quasar Stream Diagnostics}"
DESC="Fullscreen animated diagnostics page for stream, keyboard, and controller validation."

spec="$(IMAGE="$IMAGE" python3 - <<'PYEOF'
import json
import os

print(json.dumps({
    "image": os.environ["IMAGE"],
    "args": [],
    "env": {"QUASAR_DIAGNOSTICS_KEEPALIVE_ON_EXIT": "1"},
    "mounts": [],
    "gpu": True,
}))
PYEOF
)"

docker exec -i "$PG" psql -U quasar -d quasar >/dev/null <<SQL
INSERT INTO apps (
  name, description, runtime_spec,
  default_vram_mb, default_encode_slots,
  default_width, default_height, default_fps, default_bitrate_kbps,
  enabled
)
SELECT
  '${NAME}', '${DESC}', '${spec}'::jsonb,
  256, 1,
  1920, 1080, 60, 8000,
  true
WHERE NOT EXISTS (SELECT 1 FROM apps WHERE name = '${NAME}');

UPDATE apps
SET description = '${DESC}',
    runtime_spec = '${spec}'::jsonb,
    default_width = 1920,
    default_height = 1080,
    default_fps = 60,
    default_bitrate_kbps = 8000,
    enabled = true
WHERE name = '${NAME}';

-- ENTITLEMENT (steam-library-discovery Phase 2, migration 0043, spec §6.4).
-- Without this the seeded app is INVISIBLE and, more importantly, UNLAUNCHABLE:
-- the SPT-06 certification bench resolves this app by name
-- (GetDiagnosticsAppID, internal/session/encoder_cert.go) and launches it
-- through ScheduleAndCreate, which refuses an unentitled app with
-- "not entitled to this app" and writes no cert row.
--
-- Only a database seeded BEFORE 0043 gets the row from the migration backfill.
-- This script is how the app arrives on a FRESH stack, a rebuilt dev box, or a
-- restored-then-reseeded host — every case the backfill cannot reach. The
-- INSERT above only fires when the app is absent, so a re-run of this script on
-- an already-seeded box would not re-create the entitlement either; this
-- statement is therefore unconditional-with-ON-CONFLICT rather than nested
-- inside the same NOT EXISTS guard, so it also REPAIRS a box seeded between
-- 0043 landing and this fix.
--
-- granted_by='migration', not 'admin': no operator performed this grant, and
-- granted_by_user would be NULL, which is exactly the false attribution the
-- 0043 comment says 'migration' exists to avoid. The meaning is identical to
-- the backfill's — "this app is public because it always was".
INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by)
SELECT 'all', NULL, id, 'migration' FROM apps WHERE name = '${NAME}'
ON CONFLICT DO NOTHING;
SQL

echo "seeded: ${NAME} (${IMAGE})"
