-- 0053_setup_wizard_state.down.sql — reverse the first-run wizard state columns.
BEGIN;

ALTER TABLE instance_settings
    DROP COLUMN IF EXISTS setup_completed_at,
    DROP COLUMN IF EXISTS setup_state;

COMMIT;
