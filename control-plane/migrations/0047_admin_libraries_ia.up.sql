-- 0047_admin_libraries_ia.up.sql — Admin Libraries IA + Steam provider page,
-- backend half (docs/design/plans/2026-08-01-admin-libraries-ia-spec.md §4/§5,
-- Michael-approved 2026-08-01). Protocol amendment already landed
-- (quasar-protocol pin 3e0f692): SettingsEnvelope + the PATCH body gain
-- library_discovery_interval_minutes / library_discovery_appdetails_enabled;
-- LibraryStatus gains interval_overridden_by_env / appdetails_overridden_by_env
-- / last_scan_completed_at (control-plane-only resolution, no new column for
-- those three — they are computed, never stored).
--
-- TWO INDEPENDENT PIECES, ONE MIGRATION:
--
-- (1) Two new instance_settings columns — the operator UI path for the scan
-- interval and the appdetails lookup, alongside the existing
-- library_discovery_enabled master switch (0045). THE ENV VARS ARE NOT
-- RETIRED: QUASAR_LIBRARY_SCAN_INTERVAL and QUASAR_STEAM_APPDETAILS_LOOKUP
-- become OVERRIDES when set (env wins over the database), never lifted by a
-- database write — that resolution lives in control-plane Go
-- (internal/library's shared resolver), not in SQL. The columns just hold the
-- operator's database-side intent so a deployment with no env override has
-- something to read.
--
-- Bounds (15..10080 minutes = 15 minutes..7 days) are enforced BOTH here (a
-- CHECK, the durable guard) and in the PATCH handler (a 400 validation_failed,
-- the actionable one) — the same two-layer posture library_appid_rules' appid
-- CHECK already uses (0045).
--
-- (2) The default Steam runtime preset (§5) — a shipped, idempotent
-- runtime_presets row so a fresh install can point its Steam app at a working
-- container spec without hand-writing one. ON CONFLICT (id) DO NOTHING, keyed
-- by a STABLE id constant, so an operator's later edits to this exact row
-- survive every future migration run — the same idempotent-seed posture 0046
-- uses for the profile catalog, except 0046 UPSERTs (it owns those rows
-- forever) and this is a DO-NOTHING seed (the operator owns this row the
-- moment it exists; a re-run must never clobber their edit).
--
-- runtime_presets HAS NO `gpu` COLUMN, deliberately not added here. Per
-- mergeRuntimePreset's doc comment (internal/session/runtime_preset.go):
-- "Any other key in the app's runtime_spec (`gpu`, and anything added later)
-- is carried through untouched — the merge only owns [image/args/env/mounts]."
-- gpu is an APP-level runtime_spec key, never a preset-level one, by the
-- existing merge contract — adding a gpu column to this table would be a
-- schema change the spec did not ask for and mergeRuntimePreset does not
-- consume. The Steam app that adopts this preset sets `gpu: true` on its own
-- runtime_spec, same as every other GPU-needing app today.
BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN library_discovery_interval_minutes INTEGER NOT NULL DEFAULT 360
        CHECK (library_discovery_interval_minutes BETWEEN 15 AND 10080),
    ADD COLUMN library_discovery_appdetails_enabled BOOLEAN NOT NULL DEFAULT false;

-- The default Steam runtime preset. Stable id so re-running this migration (or
-- a fresh database reaching it) never duplicates or clobbers an operator edit.
-- Per-user home persistence is expressed via MANAGED_HOME (mount at the
-- default home_container_path /home/quasar), NOT a hand-set HOME env var —
-- the live reference deployment runs the Steam provider app exactly this way
-- (managed_home=true), and a duplicate HOME constant in env would just be a
-- second copy of home_container_path for an operator to desynchronise.
INSERT INTO runtime_presets (id, name, description, image, env, args, mounts, managed_home)
VALUES (
    '007ef4c6-28ee-4a6d-9194-25eb56fa862c',
    'Steam',
    'Shipped default for the quasar-steam provider image — the container spec ' ||
    'Steam library discovery''s provider app is expected to launch with. ' ||
    'Edit freely; this seed never overwrites a changed row.',
    'quasar-steam:latest',
    '{"PUID":"99","PGID":"100","UNAME":"quasar"}'::jsonb,
    '[]'::jsonb,
    '[]'::jsonb,
    true
)
ON CONFLICT (id) DO NOTHING;

COMMIT;
