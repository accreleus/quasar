-- 0053_setup_wizard_state.up.sql — first-run setup wizard state (Spec B W1)
-- (docs/design/plans/2026-08-07-first-run-wizard-spec.md "Wizard state & resumability",
-- protocol/control-api.md "First-run setup", protocol/openapi.yaml SetupStatus).
--
-- Two additive columns on the instance_settings singleton, no existing row
-- changes:
--
--   setup_completed_at — non-NULL once the wizard finished OR was explicitly
--   skipped. GET /v1/setup/status reports (setup_completed_at IS NOT NULL) as
--   its setup_completed boolean.
--
--   setup_state — RESERVED and currently unwritten. Intended for opaque
--   SPA-owned resume state in a later phase; today no server code writes it
--   (furthest-step resume is client-side) and it stays '{}'. Defaults to '{}'
--   so a read never has to special-case NULL.
--
-- Upgrade vs fresh install — the backfill below is load-bearing:
--
--   * An EXISTING installation has already run settings.Seed on a prior boot,
--     so the instance_settings singleton row exists when this migration runs.
--     Backfilling setup_completed_at = now() for that row keeps an upgraded,
--     already-operating instance (which has admins and never ran the wizard)
--     from suddenly reading setup_completed=false and prompting every admin
--     with the first-run resume banner.
--
--   * A FRESH database has NO singleton row at migration time: boot order is
--     migrate.Run (all migrations) THEN settings.Seed (cmd/quasar-control),
--     so the UPDATE touches zero rows and the later-seeded row keeps the
--     column default NULL — a genuinely new instance correctly enters the
--     wizard.
BEGIN;

ALTER TABLE instance_settings
    ADD COLUMN setup_completed_at TIMESTAMPTZ NULL,
    ADD COLUMN setup_state        JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Backfill: any row that exists at migration time belongs to an existing
-- installation (see header) — mark it complete so the upgrade is invisible.
UPDATE instance_settings
    SET setup_completed_at = now()
    WHERE setup_completed_at IS NULL;

COMMENT ON COLUMN instance_settings.setup_completed_at IS
    'First-run wizard (migration 0053). Non-NULL once the setup flow finished or was explicitly skipped; GET /v1/setup/status returns (this IS NOT NULL) as setup_completed. Backfilled to now() for rows existing at migration time (upgraded installs); NULL only on a genuinely fresh instance.';
COMMENT ON COLUMN instance_settings.setup_state IS
    'First-run wizard (migration 0053). RESERVED, currently unwritten: intended for opaque SPA-owned resume state in a later phase; no server code writes it today. Default {}.';

COMMIT;
