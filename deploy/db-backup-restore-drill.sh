#!/usr/bin/env bash
# Docker-only, disposable Postgres backup/restore rehearsal for Wave 5.
# Never points at deploy/.env, a host stack, or a named production volume.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="$ROOT/scripts/verify/docker-compose.devtools.yml"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

run_control() {
    local database_url="$1" port="$2" log="$3"
    DATABASE_URL="$database_url" \
    ENROLLMENT_TOKEN="db-rehearsal-only-token" \
    LISTEN_ADDR="127.0.0.1:${port}" \
    /tmp/quasar-control >"$log" 2>&1 &
    CONTROL_PID=$!
    for _ in $(seq 1 60); do
        if curl -fsS "http://127.0.0.1:${port}/health" >/dev/null 2>&1; then
            return 0
        fi
        if ! kill -0 "$CONTROL_PID" 2>/dev/null; then
            cat "$log" >&2
            fail "control-plane binary exited before /health"
        fi
        sleep 0.25
    done
    cat "$log" >&2
    fail "control-plane binary did not become healthy"
}

stop_control() {
    if [ -n "${CONTROL_PID:-}" ] && kill -0 "$CONTROL_PID" 2>/dev/null; then
        kill -TERM "$CONTROL_PID"
        wait "$CONTROL_PID" || true
    fi
    CONTROL_PID=""
}

inside() {
    : "${DATABASE_URL:?compose must provide DATABASE_URL}"
    # All databases in this disposable Compose project use this fixed test
    # credential. Prevent libpq from ever prompting if a URL-less command is
    # added or a connection fails.
    export PGPASSWORD=quasar_test
    export PGCONNECT_TIMEOUT=5
    command -v pg_dump >/dev/null || fail "pg_dump missing from devtools image"
    command -v pg_restore >/dev/null || fail "pg_restore missing from devtools image"
    command -v psql >/dev/null || fail "psql missing from devtools image"

    # Head comes from tracked migration filenames, then source is compiled into the
    # exact binary used for both preflights. Do not replace this with raw SQL.
    local expected_head
    expected_head="$(find /workspace/control-plane/migrations -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.up.sql' \
        -exec basename {} \; | LC_ALL=C sort | tail -1 | sed -E 's/^0*([0-9]+)_.*/\1/')"
    [ -n "$expected_head" ] || fail "could not derive migration head"

    (
        cd /workspace/control-plane
        go build -buildvcs=false -o /tmp/quasar-control ./cmd/quasar-control
    )

    local work source_state restored_state source_schema restore_schema
    work="$(mktemp -d)"
    trap 'stop_control; rm -rf "${work:-}"' EXIT

    run_control "$DATABASE_URL" 18080 "$work/source-control.log"
    source_state="$(psql "$DATABASE_URL" -XAtqc "SELECT version::text || ':' || dirty::text FROM schema_migrations")"
    [ "$source_state" = "${expected_head}:false" ] || \
        fail "source migration head is $source_state; expected ${expected_head}:false"

    # Seed real cross-table application records, not rehearsal-only metadata.
    psql "$DATABASE_URL" -Xv ON_ERROR_STOP=1 <<'SQL'
INSERT INTO users (id, email, username, password_hash, role)
VALUES ('11111111-1111-1111-1111-111111111111', 'backup-rehearsal@example.invalid',
        'backup_rehearsal', '$argon2id$v=19$m=65536,t=3,p=1$rehearsal$rehearsal', 'admin');
INSERT INTO apps (id, name, description, runtime_spec, default_vram_mb, default_encode_slots,
                  default_width, default_height, default_fps, default_bitrate_kbps)
VALUES ('22222222-2222-2222-2222-222222222222', 'Backup rehearsal app', 'ephemeral validation row',
        '{"image":"example.invalid/quasar/rehearsal:never-run"}', 1024, 1, 1920, 1080, 60, 8000);
INSERT INTO hosts (id, node_name, status, agent_version, cpu_cores, mem_mb)
VALUES ('33333333-3333-3333-3333-333333333333', 'backup-rehearsal-host', 'offline', 'rehearsal', 4, 8192);
INSERT INTO gpus (id, host_id, index, vendor, model, vram_mb_total, encode_slots_total)
VALUES ('44444444-4444-4444-4444-444444444444', '33333333-3333-3333-3333-333333333333', 0,
        'rehearsal', 'rehearsal', 4096, 1);
INSERT INTO sessions (id, user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps)
VALUES ('55555555-5555-5555-5555-555555555555', '11111111-1111-1111-1111-111111111111',
        '22222222-2222-2222-2222-222222222222', '33333333-3333-3333-3333-333333333333',
        '44444444-4444-4444-4444-444444444444', 'stopped', 1920, 1080, 60, 8000);
INSERT INTO admin_activity (actor_user_id, action, target_type, target_id, details)
VALUES ('11111111-1111-1111-1111-111111111111', 'backup_rehearsal', 'database', 'ephemeral',
        '{"purpose":"backup-restore-validation"}');
-- entitlements (Phase 2, migration 0043) is an AUTHORIZATION table, so a backup
-- that silently failed to carry it would restore a fleet in which every user's
-- library is empty and no launch succeeds. Both shapes are seeded — an 'all'
-- row (subject_id NULL) and a per-user row — because they are stored under two
-- different partial unique indexes and a restore that dropped either would be a
-- distinct defect. The fingerprint below asserts both survive.
INSERT INTO entitlements (id, subject_type, subject_id, app_id, granted_by, granted_by_user)
VALUES ('66666666-6666-6666-6666-666666666666', 'all', NULL,
        '22222222-2222-2222-2222-222222222222', 'migration', NULL),
       ('77777777-7777-7777-7777-777777777777', 'user',
        '11111111-1111-1111-1111-111111111111',
        '22222222-2222-2222-2222-222222222222', 'admin',
        '11111111-1111-1111-1111-111111111111');
SQL

    pg_dump --format=custom --no-owner --no-privileges --dbname="$DATABASE_URL" >"$work/quasar.dump"
    [ -s "$work/quasar.dump" ] || fail "backup dump is empty"

    # A new database is deliberate: restore must not depend on original objects.
    createdb -w -h postgres -U quasar_test quasar_restore
    local restore_url='postgres://quasar_test:quasar_test@postgres:5432/quasar_restore?sslmode=disable'
    pg_restore --exit-on-error --clean --if-exists --no-owner --no-privileges \
        --dbname="$restore_url" "$work/quasar.dump"

    source_schema="$(pg_dump --schema-only --no-owner --no-privileges --dbname="$DATABASE_URL" | sha256sum | awk '{print $1}')"
    restore_schema="$(pg_dump --schema-only --no-owner --no-privileges --dbname="$restore_url" | sha256sum | awk '{print $1}')"
    [ "$source_schema" = "$restore_schema" ] || fail "restored schema fingerprint differs"

    # Check head and application relations before binary preflight. pg_restore
    # validates every archived object; these checks prove useful application data.
    local fingerprint_sql
    fingerprint_sql="SELECT concat_ws(':',
      (SELECT count(*) FROM users WHERE id='11111111-1111-1111-1111-111111111111'),
      (SELECT count(*) FROM apps WHERE id='22222222-2222-2222-2222-222222222222' AND runtime_spec->>'image'='example.invalid/quasar/rehearsal:never-run'),
      (SELECT count(*) FROM hosts WHERE id='33333333-3333-3333-3333-333333333333'),
      (SELECT count(*) FROM gpus WHERE id='44444444-4444-4444-4444-444444444444' AND host_id='33333333-3333-3333-3333-333333333333'),
      (SELECT count(*) FROM sessions WHERE id='55555555-5555-5555-5555-555555555555' AND state='stopped'),
      (SELECT count(*) FROM admin_activity WHERE action='backup_rehearsal' AND target_id='ephemeral'),
      (SELECT count(*) FROM entitlements WHERE app_id='22222222-2222-2222-2222-222222222222'
         AND subject_type='all' AND subject_id IS NULL),
      (SELECT count(*) FROM entitlements WHERE app_id='22222222-2222-2222-2222-222222222222'
         AND subject_type='user' AND subject_id='11111111-1111-1111-1111-111111111111'))"
    [ "$(psql "$DATABASE_URL" -XAtqc "$fingerprint_sql")" = '1:1:1:1:1:1:1:1' ] || fail "source seed integrity assertion failed"
    [ "$(psql "$restore_url" -XAtqc "$fingerprint_sql")" = '1:1:1:1:1:1:1:1' ] || fail "restored seed integrity assertion failed"

    stop_control
    run_control "$restore_url" 18081 "$work/restore-control.log"
    restored_state="$(psql "$restore_url" -XAtqc "SELECT version::text || ':' || dirty::text FROM schema_migrations")"
    [ "$restored_state" = "${expected_head}:false" ] || \
        fail "restored migration head is $restored_state; expected ${expected_head}:false"
    stop_control

    printf 'PASS: backup/restore rehearsal migration_head=%s schema_sha256=%s seed_rows=%s\n' \
        "$source_state" "$source_schema" 'users,apps,hosts,gpus,sessions,admin_activity,entitlements'
}

if [ "${1:-}" = '--inside' ]; then
    inside
    exit 0
fi

project="quasar-db-rehearsal-$RANDOM-$RANDOM"
compose=(docker compose -p "$project" -f "$COMPOSE_FILE")
internet_network="${project}_internet"
rehearsal_container=""
cleanup() {
    if [ -n "$rehearsal_container" ]; then
        docker rm -f "$rehearsal_container" >/dev/null 2>&1 || true
    fi
    docker network rm "$internet_network" >/dev/null 2>&1 || true
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${compose[@]}" up --build --wait postgres
# Do not `compose run devtools`: that service has shared named compiler caches.
# A plain transient container is isolated to this project network and cannot
# delete or relabel another developer's cache volumes during cleanup.
docker compose -f "$COMPOSE_FILE" build devtools
docker network create "$internet_network" >/dev/null
rehearsal_container="$(docker create --network "$internet_network" \
    -v "$ROOT:/workspace" -w /workspace \
    -e 'DATABASE_URL=postgres://quasar_test:quasar_test@postgres:5432/quasar_test?sslmode=disable' \
    quasar-devtools:local bash /workspace/deploy/db-backup-restore-drill.sh --inside)"
docker network connect "${project}_test" "$rehearsal_container"
docker start -a "$rehearsal_container"
docker rm "$rehearsal_container" >/dev/null
rehearsal_container=""
